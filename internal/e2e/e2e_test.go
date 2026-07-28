// Package e2e wires the real webhook app, reporter, GitLab client and
// Prometheus clients against mock HTTP servers, and drives the full chain:
// webhook delivery -> queue -> worker -> Prometheus queries -> MR note upsert.
package e2e

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/advice"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/command"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/correlate"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/history"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/report"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/reporter"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/webhook"
)

const (
	secret        = "e2e-secret"
	signingKeyRaw = "e2e-signing-key-0123456789abcdef"
	projectID     = 7
	pipelineID    = 42
	mrIID         = 3
	jobID         = 101
	branchRef     = "feature-x"
	podName       = "runner-abc123-project-7-concurrent-0"
	commandsKey   = "e2e-commands-key"
)

func e2eSigningToken() string {
	return "whsec_" + base64.StdEncoding.EncodeToString([]byte(signingKeyRaw))
}

// mockGitLab serves the subset of the GitLab REST API the bot uses and
// records every note create/update.
type mockGitLab struct {
	mu      sync.Mutex
	notes   []string // note bodies, index+1 = note ID
	updates int
	uploads int
	replies []string
}

func (m *mockGitLab) server(t *testing.T) *httptest.Server {
	t.Helper()
	started := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	finished := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("GET /api/v4/projects/%d/pipelines/%d/jobs", projectID, pipelineID),
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `[{"id":%d,"name":"build","status":"success","started_at":%q,"finished_at":%q}]`,
				jobID, started, finished)
		})
	mux.HandleFunc(fmt.Sprintf("GET /api/v4/projects/%d/merge_requests", projectID),
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("source_branch") == branchRef && r.URL.Query().Get("state") == "opened" {
				_, _ = fmt.Fprintf(w, `[{"iid":%d,"state":"opened","source_branch":%q}]`, mrIID, branchRef)
				return
			}
			_, _ = fmt.Fprint(w, `[]`)
		})
	// UpsertNote finds the report note through the discussions API. Each stored
	// note is one discussion; the first report note owns any command replies as
	// its sub-notes, so a report with replies reads back as a multi-note thread.
	mux.HandleFunc(fmt.Sprintf("GET /api/v4/projects/%d/merge_requests/%d/discussions", projectID, mrIID),
		func(w http.ResponseWriter, _ *http.Request) {
			m.mu.Lock()
			defer m.mu.Unlock()
			discs := make([]string, len(m.notes))
			for i, body := range m.notes {
				notes := []string{fmt.Sprintf(`{"id":%d,"body":%q,"author":{"id":555},"system":false}`, i+1, body)}
				if i == 0 {
					for j, reply := range m.replies {
						notes = append(notes, fmt.Sprintf(`{"id":%d,"body":%q,"author":{"id":9},"system":false}`, 1000+j, reply))
					}
				}
				discs[i] = fmt.Sprintf(`{"id":"d%d","individual_note":%t,"notes":[%s]}`,
					i+1, len(notes) == 1, strings.Join(notes, ","))
			}
			_, _ = fmt.Fprintf(w, "[%s]", strings.Join(discs, ","))
		})
	mux.HandleFunc(fmt.Sprintf("POST /api/v4/projects/%d/merge_requests/%d/notes", projectID, mrIID),
		func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.notes = append(m.notes, noteBody(t, r))
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":%d}`, len(m.notes))
		})
	mux.HandleFunc(fmt.Sprintf("PUT /api/v4/projects/%d/merge_requests/%d/notes/{id}", projectID, mrIID),
		func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.Atoi(r.PathValue("id"))
			m.mu.Lock()
			defer m.mu.Unlock()
			if id >= 1 && id <= len(m.notes) {
				m.notes[id-1] = noteBody(t, r)
			}
			m.updates++
			_, _ = fmt.Fprintf(w, `{"id":%d}`, id)
		})
	mux.HandleFunc(fmt.Sprintf("GET /api/v4/projects/%d/jobs/%d/trace", projectID, jobID),
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, "Preparing environment\nRunning on %s via gitlab-runner-mgr...\nJob succeeded\n", podName)
		})
	mux.HandleFunc("GET /api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":555,"username":"cigar-bot"}`)
	})
	mux.HandleFunc(fmt.Sprintf("GET /api/v4/projects/%d/merge_requests/%d/discussions/{id}", projectID, mrIID),
		func(w http.ResponseWriter, _ *http.Request) {
			marker := report.SignedMarker(pipelineID, mrIID, []byte(commandsKey))
			_, _ = fmt.Fprintf(w, `{"id":"disc1","notes":[{"id":1,"body":%q,"author":{"id":555}}]}`, marker)
		})
	mux.HandleFunc(fmt.Sprintf("POST /api/v4/projects/%d/uploads", projectID),
		func(w http.ResponseWriter, _ *http.Request) {
			m.mu.Lock()
			m.uploads++
			m.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"markdown":"![c](/uploads/x/c.svg)","url":"/uploads/x/c.svg"}`)
		})
	mux.HandleFunc(fmt.Sprintf("POST /api/v4/projects/%d/merge_requests/%d/discussions/{id}/notes", projectID, mrIID),
		func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			m.replies = append(m.replies, noteBody(t, r))
			m.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":99}`)
		})
	// Baseline: three successful pipelines on other refs (2m, 4m, 6m -> median
	// 4m) plus one on the reported branch and one under the MR ref form, both of
	// which must be excluded from the comparison.
	baselineJobs := map[int64]time.Duration{
		901: 2 * time.Minute,
		902: 4 * time.Minute,
		903: 6 * time.Minute,
		904: 30 * time.Minute, // on branchRef: excluded
		905: 40 * time.Minute, // on refs/merge-requests/<mrIID>/head: excluded
	}
	mux.HandleFunc(fmt.Sprintf("GET /api/v4/projects/%d/pipelines", projectID),
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `[
				{"id":905,"ref":"refs/merge-requests/%d/head","status":"success"},
				{"id":904,"ref":%q,"status":"success"},
				{"id":903,"ref":"main","status":"success"},
				{"id":902,"ref":"main","status":"success"},
				{"id":901,"ref":"main","status":"success"}
			]`, mrIID, branchRef)
		})
	for id, dur := range baselineJobs {
		mux.HandleFunc(fmt.Sprintf("GET /api/v4/projects/%d/pipelines/%d/jobs", projectID, id),
			func(w http.ResponseWriter, _ *http.Request) {
				start := time.Now().Add(-2 * time.Hour).UTC()
				_, _ = fmt.Fprintf(w, `[{"id":%d,"name":"build","status":"success","started_at":%q,"finished_at":%q}]`,
					id*10, start.Format(time.RFC3339), start.Add(dur).Format(time.RFC3339))
			})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("mock gitlab: unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func noteBody(t *testing.T, r *http.Request) string {
	t.Helper()
	var payload struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("mock gitlab: decode note payload: %v", err)
	}
	return payload.Body
}

// mockProm serves /api/v1/query and records every PromQL query received.
type mockProm struct {
	mu      sync.Mutex
	queries []string
}

func (m *mockProm) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/v1/query_range") {
			_ = r.ParseForm()
			m.mu.Lock()
			m.queries = append(m.queries, r.FormValue("query"))
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1752912000,"1"],[1752912030,"2"]]}]}}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/api/v1/query") {
			t.Errorf("mock prometheus: unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		query := r.FormValue("query")
		m.mu.Lock()
		m.queries = append(m.queries, query)
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(query, "kube_pod_labels") {
			_, _ = fmt.Fprintf(w,
				`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"pod":%q},"value":[1752912000,"1"]}]}}`,
				podName)
			return
		}
		_, _ = fmt.Fprint(w,
			`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1752912000,"123.45"]}]}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (m *mockProm) sawQuery(substr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, q := range m.queries {
		if strings.Contains(q, substr) {
			return true
		}
	}
	return false
}

// harness wires the real webhook app + queue + worker + clients against the
// mock GitLab/Prometheus servers, mirroring `bot serve`. It returns the app to
// deliver webhooks to plus the two mocks to assert against.
func harness(t *testing.T, podResolver string) (*fiber.App, *mockGitLab, *mockProm) {
	t.Helper()
	glMock := &mockGitLab{}
	promMock := &mockProm{}
	glSrv := glMock.server(t)
	promSrv := promMock.server(t)

	log := zap.NewNop()
	glClient, err := gitlab.New(glSrv.URL, "test-token", log)
	if err != nil {
		t.Fatalf("gitlab client: %v", err)
	}
	var resolver correlate.Resolver
	switch podResolver {
	case "trace":
		resolver = correlate.NewTraceResolver(glClient, log)
	default:
		var err error
		resolver, err = correlate.NewPromResolver(promSrv.URL, 30*time.Second, log)
		if err != nil {
			t.Fatalf("resolver: %v", err)
		}
	}
	source, err := metrics.NewPromSource(promSrv.URL, 30*time.Second, log, nil)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	rep := &reporter.Reporter{
		GitLab:   glClient,
		Resolver: resolver,
		Metrics:  source,
		History: &history.Fetcher{
			GitLab:    glClient,
			Pipelines: 3,
			TTL:       time.Hour,
			Log:       log,
		},
		ThrottleWarnRatio:  0.25,
		DurationDeltaRatio: 0.05,
		Log:                log,
	}

	// Same queue+worker shape as `bot serve`: merge_request may be absent, in
	// which case ProcessPipeline resolves the MR from the branch ref.
	ctx := t.Context()
	q := make(chan webhook.Event, 8)
	eng, err := advice.New(advice.Thresholds{
		ThrottleWarnRatio:   0.25,
		LongJob:             10 * time.Minute,
		MemoryPressureRatio: 0.9,
	}, nil)
	if err != nil {
		t.Fatalf("advice engine: %v", err)
	}
	cmdHandler := &command.Handler{
		GitLab:     glClient,
		Resolver:   resolver,
		Series:     source, // *metrics.PromSource satisfies metrics.SeriesSource
		Advisor:    e2eAdvisor{rep: rep, eng: eng},
		SigningKey: []byte(commandsKey),
		BotUserID:  555,
		Log:        log,
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-q:
				switch {
				case ev.Pipeline != nil:
					pe := ev.Pipeline
					var mrIID int64
					if pe.MergeRequest != nil {
						mrIID = pe.MergeRequest.IID
					}
					if _, err := rep.ProcessPipeline(ctx, pe.Project.ID, pe.ObjectAttributes.ID,
						mrIID, pe.ObjectAttributes.Ref, pe.ObjectAttributes.Status); err != nil {
						t.Errorf("process pipeline: %v", err)
					}
				case ev.Note != nil:
					if err := cmdHandler.Handle(ctx, *ev.Note); err != nil {
						t.Errorf("handle note: %v", err)
					}
				}
			}
		}
	}()
	auth, err := webhook.NewSigningTokenAuth(e2eSigningToken(), webhook.DefaultTimestampTolerance)
	if err != nil {
		t.Fatalf("signing token auth: %v", err)
	}
	return webhook.NewApp(auth, chanQueue(q), log, true, nil), glMock, promMock
}

func TestWebhookToMRNote(t *testing.T) {
	app, glMock, promMock := harness(t, "prometheus")

	payload := fmt.Sprintf(`{
		"object_kind": "pipeline",
		"object_attributes": {"id": %d, "status": "success"},
		"project": {"id": %d},
		"merge_request": {"iid": %d}
	}`, pipelineID, projectID, mrIID)

	// First delivery: a note must be created.
	postWebhook(t, app, payload)
	waitFor(t, "note created", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return len(glMock.notes) == 1
	})

	glMock.mu.Lock()
	body := glMock.notes[0]
	glMock.mu.Unlock()
	if !strings.Contains(body, report.Marker) {
		t.Errorf("note body missing marker %q:\n%s", report.Marker, body)
	}

	for _, metric := range []string{
		"kube_pod_labels",
		"container_memory_working_set_bytes",
		"container_cpu_usage_seconds_total",
		"container_cpu_cfs_throttled_periods_total",
		"container_network_receive_bytes_total",
		"kube_pod_container_resource_requests",
	} {
		if !promMock.sawQuery(metric) {
			t.Errorf("prometheus never received a query for %s", metric)
		}
	}
	if !promMock.sawQuery(podName) {
		t.Error("usage queries were not filtered by the correlated pod name")
	}

	// Second delivery (retry/idempotency): the note must be updated in place.
	postWebhook(t, app, payload)
	waitFor(t, "note updated", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return glMock.updates == 1
	})
	glMock.mu.Lock()
	defer glMock.mu.Unlock()
	if len(glMock.notes) != 1 {
		t.Fatalf("notes = %d after retry, want 1 (updated, not duplicated)", len(glMock.notes))
	}
}

// TestWebhookNewNoteAfterReplies proves the report is reposted as a fresh note
// once its discussion has sub-notes (e.g. an advise/details reply), rather than
// editing a note people are already discussing.
func TestWebhookNewNoteAfterReplies(t *testing.T) {
	app, glMock, _ := harness(t, "prometheus")

	payload := fmt.Sprintf(`{
		"object_kind": "pipeline",
		"object_attributes": {"id": %d, "status": "success"},
		"project": {"id": %d},
		"merge_request": {"iid": %d}
	}`, pipelineID, projectID, mrIID)

	// First delivery creates the report note.
	postWebhook(t, app, payload)
	waitFor(t, "note created", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return len(glMock.notes) == 1
	})

	// A reply lands in the report's discussion, as an advise/details command would.
	glMock.mu.Lock()
	glMock.replies = append(glMock.replies, "an interactive reply")
	glMock.mu.Unlock()

	// Second delivery: the report note now has a sub-note, so a NEW note is
	// posted rather than the existing thread being edited.
	postWebhook(t, app, payload)
	waitFor(t, "second note posted", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return len(glMock.notes) == 2
	})
	glMock.mu.Lock()
	defer glMock.mu.Unlock()
	if glMock.updates != 0 {
		t.Fatalf("updates = %d, want 0 (a replied report must not be edited)", glMock.updates)
	}
}

// TestWebhookBranchResolvesMR covers a pipeline whose webhook carries no
// merge_request (branch pushed before the MR was created): the worker must
// resolve the open MR from object_attributes.ref and still post the note.
func TestWebhookBranchResolvesMR(t *testing.T) {
	app, glMock, _ := harness(t, "prometheus")

	// No "merge_request" field — only the branch ref.
	payload := fmt.Sprintf(`{
		"object_kind": "pipeline",
		"object_attributes": {"id": %d, "status": "success", "ref": %q},
		"project": {"id": %d}
	}`, pipelineID, branchRef, projectID)

	postWebhook(t, app, payload)
	waitFor(t, "note created via branch-resolved MR", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return len(glMock.notes) == 1
	})

	glMock.mu.Lock()
	defer glMock.mu.Unlock()
	if !strings.Contains(glMock.notes[0], report.Marker) {
		t.Errorf("note body missing marker %q:\n%s", report.Marker, glMock.notes[0])
	}
}

// TestWebhookTraceResolver drives the full chain with POD_RESOLVER=trace: the
// pod is parsed from the job's GitLab trace, and usage queries must be filtered
// by that pod name.
func TestWebhookTraceResolver(t *testing.T) {
	app, glMock, promMock := harness(t, "trace")

	payload := fmt.Sprintf(`{
		"object_kind": "pipeline",
		"object_attributes": {"id": %d, "status": "success"},
		"project": {"id": %d},
		"merge_request": {"iid": %d}
	}`, pipelineID, projectID, mrIID)

	postWebhook(t, app, payload)
	waitFor(t, "note created via trace-resolved pod", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return len(glMock.notes) == 1
	})

	if !promMock.sawQuery(podName) {
		t.Error("usage queries were not filtered by the trace-resolved pod name")
	}
	if promMock.sawQuery("kube_pod_labels") {
		t.Error("trace resolver must not issue kube_pod_labels queries")
	}
}

type chanQueue chan webhook.Event

func (q chanQueue) Enqueue(ev webhook.Event) bool {
	select {
	case q <- ev:
		return true
	default:
		return false
	}
}

func postWebhook(t *testing.T, app *fiber.App, payload string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	const msgID = "e2e-msg"
	mac := hmac.New(sha256.New, []byte(signingKeyRaw))
	mac.Write([]byte(msgID + "." + ts + "." + payload))
	req.Header.Set("webhook-id", msgID)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Gitlab-Event", "Pipeline Hook")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("deliver webhook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200", resp.StatusCode)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func postNoteWebhook(t *testing.T, app *fiber.App, payload string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	const msgID = "e2e-note"
	mac := hmac.New(sha256.New, []byte(signingKeyRaw))
	mac.Write([]byte(msgID + "." + ts + "." + payload))
	req.Header.Set("webhook-id", msgID)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Gitlab-Event", "Note Hook")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("deliver note webhook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("note webhook status = %d, want 200", resp.StatusCode)
	}
}

func TestNoteCommandDetailsJob(t *testing.T) {
	app, glMock, _ := harness(t, "trace")
	payload := fmt.Sprintf(`{
		"object_kind":"note",
		"object_attributes":{"id":77,"note":"details job build","noteable_type":"MergeRequest","discussion_id":"disc1","author_id":9},
		"project":{"id":%d},
		"merge_request":{"iid":%d}
	}`, projectID, mrIID)

	postNoteWebhook(t, app, payload)
	waitFor(t, "command reply posted", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return len(glMock.replies) == 1
	})
	glMock.mu.Lock()
	defer glMock.mu.Unlock()
	if glMock.uploads != 3 {
		t.Fatalf("uploads = %d, want 3", glMock.uploads)
	}
}

// TestNoteCommandLoopGuard proves the bot ignores its own notes, identified by
// the marker (not by author) so a shared/human token account still works. The
// note parses as a command but carries the marker, so it must be dropped.
func TestNoteCommandLoopGuard(t *testing.T) {
	app, glMock, _ := harness(t, "trace")
	payload := fmt.Sprintf(`{
		"object_kind":"note",
		"object_attributes":{"id":78,"note":"help\n<!-- ci-resources-bot -->","noteable_type":"MergeRequest","discussion_id":"disc1","author_id":9},
		"project":{"id":%d},"merge_request":{"iid":%d}
	}`, projectID, mrIID)
	postNoteWebhook(t, app, payload)
	time.Sleep(200 * time.Millisecond)
	glMock.mu.Lock()
	defer glMock.mu.Unlock()
	if len(glMock.replies) != 0 {
		t.Fatalf("replied to a marker-tagged (own) note; replies=%d", len(glMock.replies))
	}
}

// e2eAdvisor adapts the reporter to command.Advisor, mirroring the adapter
// `bot serve` wires in cmd/bot/advise.go.
type e2eAdvisor struct {
	rep *reporter.Reporter
	eng *advice.Engine
}

func (a e2eAdvisor) Advise(ctx context.Context, projectID, pipelineID int64, jobFilter string) ([]advice.Advice, error) {
	return a.rep.Advise(ctx, projectID, pipelineID, jobFilter, a.eng)
}

// TestNoteCommandAdvise drives the whole chain — Note Hook webhook, queue,
// worker, command handler, reporter, advice engine — and asserts the reply
// carries real recommendations.
func TestNoteCommandAdvise(t *testing.T) {
	app, glMock, _ := harness(t, "trace")
	payload := fmt.Sprintf(`{
		"object_kind":"note",
		"object_attributes":{"id":79,"note":"advise","noteable_type":"MergeRequest","discussion_id":"disc1","author_id":9},
		"project":{"id":%d},
		"merge_request":{"iid":%d}
	}`, projectID, mrIID)

	postNoteWebhook(t, app, payload)
	waitFor(t, "advice reply posted", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return len(glMock.replies) == 1
	})

	glMock.mu.Lock()
	defer glMock.mu.Unlock()
	reply := glMock.replies[0]
	if glMock.uploads != 0 {
		t.Fatalf("uploads = %d, want 0 (advice posts no charts)", glMock.uploads)
	}
	for _, want := range []string{"Advice for pipeline", "KUBERNETES_CPU_LIMIT", report.MarkerPrefix} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply missing %q:\n%s", want, reply)
		}
	}
}

// TestPipelineReportComparesDuration proves the whole chain produces a duration
// delta, and that pipelines on the reported refs are excluded from the median.
func TestPipelineReportComparesDuration(t *testing.T) {
	app, glMock, _ := harness(t, "trace")

	payload := fmt.Sprintf(`{
		"object_kind": "pipeline",
		"object_attributes": {"id": %d, "status": "success", "ref": %q},
		"project": {"id": %d},
		"merge_request": {"iid": %d}
	}`, pipelineID, branchRef, projectID, mrIID)

	postWebhook(t, app, payload)
	waitFor(t, "note created", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return len(glMock.notes) == 1
	})
	glMock.mu.Lock()
	body := glMock.notes[0]
	glMock.mu.Unlock()

	// The reported pipeline's job ran from -10m to -5m, i.e. 5m, against the 4m
	// median of pipelines 901-903 (2m, 4m, 6m): +1m, +25%. If either excluded
	// pipeline (30m on branchRef, 40m on the MR ref) had leaked into the sample
	// set, the median — and this delta — would be far larger.
	const wantDelta = "🔺 +1m 00s (+25%)"
	if !strings.Contains(body, wantDelta) {
		t.Errorf("report is missing the expected delta %q:\n%s", wantDelta, body)
	}
	if !strings.Contains(body, "| Duration |") {
		t.Errorf("details table is missing the Duration column:\n%s", body)
	}
	// Three samples is a thin baseline, so the footnote must name the count.
	if !strings.Contains(body, "median of 3 recent successful pipelines") {
		t.Errorf("thin-baseline footnote missing:\n%s", body)
	}
}
