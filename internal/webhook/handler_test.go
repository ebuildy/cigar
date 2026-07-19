package webhook

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	"object_attributes": {"id": 42, "status": "success"},
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
	}{
		{
			name: "valid terminal pipeline with MR is enqueued",
			body: validPayload, wantStatus: http.StatusOK, wantQueued: 1,
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
			name: "no merge request ignored",
			body: `{"object_attributes":{"id":1,"status":"success"}}`,
			wantStatus: http.StatusOK,
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
			app := NewApp(secret, queue, slog.New(slog.DiscardHandler))

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
			if tt.wantQueued == 1 && queue.events[0].ObjectAttributes.ID != 42 {
				t.Fatalf("queued pipeline id = %d, want 42", queue.events[0].ObjectAttributes.ID)
			}
		})
	}
}

// Oversized bodies are rejected by Fiber's BodyLimit at the fasthttp layer,
// which app.Test cannot observe, so this test drives a real listener.
func TestOversizedBodyRejected(t *testing.T) {
	queue := &fakeQueue{}
	app := NewApp(secret, queue, slog.New(slog.DiscardHandler))

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
