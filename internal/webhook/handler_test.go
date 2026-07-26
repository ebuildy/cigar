package webhook

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"
)

const secret = "s3cret"

type fakeQueue struct {
	events []Event
	full   bool
}

func (q *fakeQueue) Enqueue(ev Event) bool {
	if q.full {
		return false
	}
	q.events = append(q.events, ev)
	return true
}

const validPayload = `{
	"object_kind": "pipeline",
	"object_attributes": {"id": 42, "status": "success", "ref": "feature-x"},
	"project": {"id": 7},
	"merge_request": {"iid": 3}
}`

func TestHandler(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		token      string
		event      string
		body       string
		queueFull  bool
		wantStatus int
		wantQueued int
		wantRef    string // expected ObjectAttributes.Ref on the queued event
	}{
		{
			name: "valid terminal pipeline with MR is enqueued",
			body: validPayload, wantStatus: http.StatusOK, wantQueued: 1, wantRef: "feature-x",
		},
		{
			name:  "missing token",
			token: "-", body: validPayload, wantStatus: http.StatusUnauthorized,
		},
		{
			name:  "invalid token",
			token: "wrong", body: validPayload, wantStatus: http.StatusUnauthorized,
		},
		{
			name:  "non-pipeline event ignored with 200",
			event: "Push Hook", body: validPayload, wantStatus: http.StatusOK,
		},
		{
			name: "non-terminal status ignored",
			body: `{"object_attributes":{"id":1,"status":"running"},"merge_request":{"iid":3}}`,
			wantStatus: http.StatusOK,
		},
		{
			name: "terminal pipeline without MR is enqueued (branch pushed before MR)",
			body: `{"object_attributes":{"id":42,"status":"success","ref":"feature-x"}}`,
			wantStatus: http.StatusOK, wantQueued: 1, wantRef: "feature-x",
		},
		{
			name: "malformed JSON",
			body: `{not json`, wantStatus: http.StatusBadRequest,
		},
		{
			name:   "GET not allowed",
			method: http.MethodGet, wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:      "queue full",
			body:      validPayload,
			queueFull: true, wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = http.MethodPost
			}
			token := tt.token
			switch token {
			case "":
				token = secret
			case "-":
				token = ""
			}
			event := tt.event
			if event == "" {
				event = "Pipeline Hook"
			}

			queue := &fakeQueue{full: tt.queueFull}
			app := NewApp([]Authenticator{NewSecretAuth(secret)}, queue, zap.NewNop(), false, nil)

			req := httptest.NewRequest(method, "/webhook", strings.NewReader(tt.body))
			req.Header.Set("X-Gitlab-Token", token)
			req.Header.Set("X-Gitlab-Event", event)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if len(queue.events) != tt.wantQueued {
				t.Fatalf("queued = %d, want %d", len(queue.events), tt.wantQueued)
			}
			if tt.wantQueued == 1 {
				if queue.events[0].Pipeline == nil {
					t.Fatalf("queued event has no pipeline payload")
				}
				if queue.events[0].Pipeline.ObjectAttributes.ID != 42 {
					t.Fatalf("queued pipeline id = %d, want 42", queue.events[0].Pipeline.ObjectAttributes.ID)
				}
				if queue.events[0].Pipeline.ObjectAttributes.Ref != tt.wantRef {
					t.Fatalf("queued ref = %q, want %q", queue.events[0].Pipeline.ObjectAttributes.Ref, tt.wantRef)
				}
			}
		})
	}
}

type webhookCall struct {
	project int64
	status  int
}

type fakeRecorder struct {
	webhooks []webhookCall
	users    []int64
}

func (r *fakeRecorder) RecordWebhook(projectID int64, status int) {
	r.webhooks = append(r.webhooks, webhookCall{projectID, status})
}
func (r *fakeRecorder) RecordUser(userID int64) { r.users = append(r.users, userID) }

func TestRecorderInvoked(t *testing.T) {
	const pipelineWithUser = `{
		"object_kind": "pipeline",
		"object_attributes": {"id": 42, "status": "success", "ref": "feature-x"},
		"project": {"id": 7},
		"user": {"id": 99},
		"merge_request": {"iid": 3}
	}`
	const noteCmd = `{
		"object_kind": "note",
		"object_attributes": {"id": 5, "note": "/cigar help", "noteable_type": "MergeRequest", "author_id": 88},
		"project": {"id": 7},
		"merge_request": {"iid": 3}
	}`

	tests := []struct {
		name         string
		event        string
		body         string
		token        string // "" uses the valid secret; anything else is sent verbatim
		wantWebhooks []webhookCall
		wantUsers    []int64
	}{
		{"pipeline records project and 200", "Pipeline Hook", pipelineWithUser, "", []webhookCall{{7, 200}}, []int64{99}},
		{"note records project and 200", "Note Hook", noteCmd, "", []webhookCall{{7, 200}}, []int64{88}},
		// Auth failure: recorded with project 0 (payload never parsed) and 401.
		{"bad token records 401 with no project", "Pipeline Hook", pipelineWithUser, "wrong", []webhookCall{{0, 401}}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &fakeRecorder{}
			app := NewApp([]Authenticator{NewSecretAuth(secret)}, &fakeQueue{}, zap.NewNop(), true, rec)

			token := secret
			if tt.token != "" {
				token = tt.token
			}
			req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(tt.body))
			req.Header.Set("X-Gitlab-Token", token)
			req.Header.Set("X-Gitlab-Event", tt.event)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if !equalWebhookCalls(rec.webhooks, tt.wantWebhooks) {
				t.Errorf("webhooks = %v, want %v", rec.webhooks, tt.wantWebhooks)
			}
			if !equalInt64s(rec.users, tt.wantUsers) {
				t.Errorf("users = %v, want %v", rec.users, tt.wantUsers)
			}
		})
	}
}

func equalWebhookCalls(a, b []webhookCall) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Oversized bodies are rejected by Fiber's BodyLimit at the fasthttp layer,
// which app.Test cannot observe, so this test drives a real listener.
func TestOversizedBodyRejected(t *testing.T) {
	queue := &fakeQueue{}
	app := NewApp([]Authenticator{NewSecretAuth(secret)}, queue, zap.NewNop(), false, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		_ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	defer func() { _ = app.Shutdown() }()

	body := `{"pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	req, err := http.NewRequest(http.MethodPost,
		"http://"+ln.Addr().String()+"/webhook", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Gitlab-Token", secret)
	req.Header.Set("X-Gitlab-Event", "Pipeline Hook")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
	if len(queue.events) != 0 {
		t.Fatalf("queued = %d, want 0", len(queue.events))
	}
}

// TestHandlerAuthOrdering proves first-match-wins across an ordered list and
// that a request valid only under a non-enabled method is rejected.
func TestHandlerAuthOrdering(t *testing.T) {
	sig, err := NewSignatureAuth(testSigningToken(), 5*time.Minute)
	if err != nil {
		t.Fatalf("NewSignatureAuth: %v", err)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	// signature-only app accepts a signed request but rejects a secret-only one.
	app := NewApp([]Authenticator{sig}, &fakeQueue{}, zap.NewNop(), false, nil)

	signed := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(validPayload))
	signed.Header.Set("webhook-id", "m1")
	signed.Header.Set("webhook-timestamp", ts)
	signed.Header.Set("webhook-signature", signBody("m1", ts, validPayload))
	signed.Header.Set("X-Gitlab-Event", "Pipeline Hook")
	resp, err := app.Test(signed)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed request: status %d, want 200", resp.StatusCode)
	}

	secretOnly := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(validPayload))
	secretOnly.Header.Set("X-Gitlab-Token", secret)
	secretOnly.Header.Set("X-Gitlab-Event", "Pipeline Hook")
	resp2, err := app.Test(secretOnly)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("secret request against signature-only app: status %d, want 401", resp2.StatusCode)
	}
}

// countingAuth records how many times it was consulted; result is fixed.
type countingAuth struct {
	result bool
	calls  *int
}

func (a countingAuth) Name() string { return "counting" }

func (a countingAuth) Authenticate(fiber.Ctx) bool {
	*a.calls++
	return a.result
}

// TestHandlerAuthFirstMatchWins proves the authenticate loop tries
// authenticators in order, falls through on failure, and short-circuits on the
// first success.
func TestHandlerAuthFirstMatchWins(t *testing.T) {
	post := func(app *fiber.App) int {
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(validPayload))
		req.Header.Set("X-Gitlab-Event", "Pipeline Hook")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// Fall-through: first denies, second accepts -> authenticated, both consulted.
	var firstCalls, secondCalls int
	app := NewApp([]Authenticator{
		countingAuth{result: false, calls: &firstCalls},
		countingAuth{result: true, calls: &secondCalls},
	}, &fakeQueue{}, zap.NewNop(), false, nil)
	if got := post(app); got != http.StatusOK {
		t.Fatalf("fall-through: status %d, want 200", got)
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("fall-through calls: first=%d second=%d, want 1/1", firstCalls, secondCalls)
	}

	// Short-circuit: first accepts -> second must not be consulted.
	var a1, a2 int
	app2 := NewApp([]Authenticator{
		countingAuth{result: true, calls: &a1},
		countingAuth{result: false, calls: &a2},
	}, &fakeQueue{}, zap.NewNop(), false, nil)
	if got := post(app2); got != http.StatusOK {
		t.Fatalf("short-circuit: status %d, want 200", got)
	}
	if a1 != 1 || a2 != 0 {
		t.Fatalf("short-circuit calls: first=%d second=%d, want 1/0", a1, a2)
	}

	// All deny -> 401.
	var d1, d2 int
	app3 := NewApp([]Authenticator{
		countingAuth{result: false, calls: &d1},
		countingAuth{result: false, calls: &d2},
	}, &fakeQueue{}, zap.NewNop(), false, nil)
	if got := post(app3); got != http.StatusUnauthorized {
		t.Fatalf("all deny: status %d, want 401", got)
	}
}

func postNote(t *testing.T, app *fiber.App, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Gitlab-Token", secret)
	req.Header.Set("X-Gitlab-Event", "Note Hook")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func TestNoteHookDisabledIgnored(t *testing.T) {
	queue := &fakeQueue{}
	app := NewApp([]Authenticator{NewSecretAuth(secret)}, queue, zap.NewNop(), false, nil)
	body := `{"object_kind":"note","object_attributes":{"id":1,"note":"help","noteable_type":"MergeRequest","discussion_id":"abc","author_id":9},"project":{"id":7},"merge_request":{"iid":3}}`
	if s := postNote(t, app, body); s != http.StatusOK {
		t.Fatalf("status = %d, want 200", s)
	}
	if len(queue.events) != 0 {
		t.Fatalf("enqueued %d, want 0 when commands disabled", len(queue.events))
	}
}

func TestNoteHookMatchingEnqueues(t *testing.T) {
	queue := &fakeQueue{}
	app := NewApp([]Authenticator{NewSecretAuth(secret)}, queue, zap.NewNop(), true, nil)
	body := `{"object_kind":"note","object_attributes":{"id":1,"note":"details job build","noteable_type":"MergeRequest","discussion_id":"abc","author_id":9},"project":{"id":7},"merge_request":{"iid":3}}`
	if s := postNote(t, app, body); s != http.StatusOK {
		t.Fatalf("status = %d, want 200", s)
	}
	if len(queue.events) != 1 {
		t.Fatalf("enqueued %d, want 1", len(queue.events))
	}
	ev := queue.events[0]
	if ev.Note == nil || ev.Note.Body != "details job build" || ev.Note.DiscussionID != "abc" ||
		ev.Note.MRIID != 3 || ev.Note.ProjectID != 7 || ev.Note.AuthorID != 9 || ev.Note.NoteID != 1 {
		t.Fatalf("bad note event: %+v", ev.Note)
	}
}

func TestNoteHookNonCommandIgnored(t *testing.T) {
	queue := &fakeQueue{}
	app := NewApp([]Authenticator{NewSecretAuth(secret)}, queue, zap.NewNop(), true, nil)
	body := `{"object_kind":"note","object_attributes":{"id":1,"note":"thanks!","noteable_type":"MergeRequest","discussion_id":"abc","author_id":9},"project":{"id":7},"merge_request":{"iid":3}}`
	if s := postNote(t, app, body); s != http.StatusOK {
		t.Fatalf("status = %d, want 200", s)
	}
	if len(queue.events) != 0 {
		t.Fatalf("enqueued %d, want 0 for a non-command note", len(queue.events))
	}
}

func TestNoteHookNonMRIgnored(t *testing.T) {
	queue := &fakeQueue{}
	app := NewApp([]Authenticator{NewSecretAuth(secret)}, queue, zap.NewNop(), true, nil)
	body := `{"object_kind":"note","object_attributes":{"id":1,"note":"help","noteable_type":"Issue","discussion_id":"abc","author_id":9},"project":{"id":7}}`
	if s := postNote(t, app, body); s != http.StatusOK {
		t.Fatalf("status = %d, want 200", s)
	}
	if len(queue.events) != 0 {
		t.Fatalf("enqueued %d, want 0 for a non-MR note", len(queue.events))
	}
}
