package webhook

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

const secret = "s3cret"

type fakeQueue struct {
	events []PipelineEvent
	full   bool
}

func (q *fakeQueue) Enqueue(ev PipelineEvent) bool {
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
			app := NewApp([]Authenticator{NewSecretAuth(secret)}, queue, slog.New(slog.DiscardHandler))

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
				if queue.events[0].ObjectAttributes.ID != 42 {
					t.Fatalf("queued pipeline id = %d, want 42", queue.events[0].ObjectAttributes.ID)
				}
				if queue.events[0].ObjectAttributes.Ref != tt.wantRef {
					t.Fatalf("queued ref = %q, want %q", queue.events[0].ObjectAttributes.Ref, tt.wantRef)
				}
			}
		})
	}
}

// Oversized bodies are rejected by Fiber's BodyLimit at the fasthttp layer,
// which app.Test cannot observe, so this test drives a real listener.
func TestOversizedBodyRejected(t *testing.T) {
	queue := &fakeQueue{}
	app := NewApp([]Authenticator{NewSecretAuth(secret)}, queue, slog.New(slog.DiscardHandler))

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
	app := NewApp([]Authenticator{sig}, &fakeQueue{}, slog.New(slog.DiscardHandler))

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
