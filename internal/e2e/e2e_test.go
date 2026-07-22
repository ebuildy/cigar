// Package e2e wires the real webhook app, reporter, GitLab client and
// Prometheus clients against mock HTTP servers, and drives the full chain:
// webhook delivery -> queue -> worker -> Prometheus queries -> MR note upsert.
package e2e

import (
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

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/correlate"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
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
	mux.HandleFunc(fmt.Sprintf("GET /api/v4/projects/%d/merge_requests/%d/notes", projectID, mrIID),
		func(w http.ResponseWriter, _ *http.Request) {
			m.mu.Lock()
			defer m.mu.Unlock()
			items := make([]string, len(m.notes))
			for i, body := range m.notes {
				items[i] = fmt.Sprintf(`{"id":%d,"body":%q}`, i+1, body)
			}
			_, _ = fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
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
			m.mu.Lock()
			defer m.mu.Unlock()
			m.notes[0] = noteBody(t, r)
			m.updates++
			_, _ = fmt.Fprint(w, `{"id":1}`)
		})
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
func harness(t *testing.T) (*fiber.App, *mockGitLab, *mockProm) {
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
	resolver, err := correlate.NewPromResolver(promSrv.URL, 30*time.Second, log)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	source, err := metrics.NewPromSource(promSrv.URL, 30*time.Second, log)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	rep := &reporter.Reporter{
		GitLab:            glClient,
		Resolver:          resolver,
		Metrics:           source,
		ThrottleWarnRatio: 0.25,
		Log:               log,
	}

	// Same queue+worker shape as `bot serve`: merge_request may be absent, in
	// which case ProcessPipeline resolves the MR from the branch ref.
	ctx := t.Context()
	q := make(chan webhook.PipelineEvent, 8)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-q:
				var mrIID int64
				if ev.MergeRequest != nil {
					mrIID = ev.MergeRequest.IID
				}
				if _, err := rep.ProcessPipeline(ctx, ev.Project.ID, ev.ObjectAttributes.ID,
					mrIID, ev.ObjectAttributes.Ref, ev.ObjectAttributes.Status); err != nil {
					t.Errorf("process pipeline: %v", err)
				}
			}
		}
	}()
	sigAuth, err := webhook.NewSignatureAuth(e2eSigningToken(), webhook.DefaultTimestampTolerance)
	if err != nil {
		t.Fatalf("signature auth: %v", err)
	}
	return webhook.NewApp([]webhook.Authenticator{sigAuth}, chanQueue(q), log), glMock, promMock
}

func TestWebhookToMRNote(t *testing.T) {
	app, glMock, promMock := harness(t)

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

// TestWebhookBranchResolvesMR covers a pipeline whose webhook carries no
// merge_request (branch pushed before the MR was created): the worker must
// resolve the open MR from object_attributes.ref and still post the note.
func TestWebhookBranchResolvesMR(t *testing.T) {
	app, glMock, _ := harness(t)

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

type chanQueue chan webhook.PipelineEvent

func (q chanQueue) Enqueue(ev webhook.PipelineEvent) bool {
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
