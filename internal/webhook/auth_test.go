package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// testSigningKey is the raw HMAC key; the whsec_ token is its base64 form.
var testSigningKey = []byte("0123456789abcdef0123456789abcdef")

func testSigningToken() string {
	return "whsec_" + base64.StdEncoding.EncodeToString(testSigningKey)
}

// signBody returns a valid "v1,<base64>" signature for the given parts.
func signBody(id, ts, body string) string {
	mac := hmac.New(sha256.New, testSigningKey)
	mac.Write([]byte(id + "." + ts + "." + body))
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// runAuth wires a single authenticator behind a probe route and reports
// whether a request built by decorate() authenticates (200) or not (401).
func runAuth(t *testing.T, a Authenticator, body string, decorate func(*http.Request)) int {
	t.Helper()
	app := fiber.New()
	app.Post("/p", func(c fiber.Ctx) error {
		if a.Authenticate(c) {
			return c.SendStatus(fiber.StatusOK)
		}
		return c.SendStatus(fiber.StatusUnauthorized)
	})
	req := httptest.NewRequest(http.MethodPost, "/p", strings.NewReader(body))
	decorate(req)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func TestSecretAuth(t *testing.T) {
	a := NewSecretAuth("s3cret")
	if got := runAuth(t, a, "", func(r *http.Request) { r.Header.Set("X-Gitlab-Token", "s3cret") }); got != 200 {
		t.Fatalf("valid token: status %d, want 200", got)
	}
	if got := runAuth(t, a, "", func(r *http.Request) { r.Header.Set("X-Gitlab-Token", "nope") }); got != 401 {
		t.Fatalf("wrong token: status %d, want 401", got)
	}
	if got := runAuth(t, a, "", func(r *http.Request) {}); got != 401 {
		t.Fatalf("missing token: status %d, want 401", got)
	}

	// An empty configured secret must never authenticate, even with an empty header.
	empty := NewSecretAuth("")
	if got := runAuth(t, empty, "", func(r *http.Request) {}); got != 401 {
		t.Fatalf("empty secret, no token: status %d, want 401", got)
	}
	if got := runAuth(t, empty, "", func(r *http.Request) { r.Header.Set("X-Gitlab-Token", "") }); got != 401 {
		t.Fatalf("empty secret, empty token: status %d, want 401", got)
	}
}

func TestSignatureAuth(t *testing.T) {
	a, err := NewSignatureAuth(testSigningToken(), 5*time.Minute)
	if err != nil {
		t.Fatalf("NewSignatureAuth: %v", err)
	}
	const body = `{"object_kind":"pipeline"}`
	now := func() string { return strconv.FormatInt(time.Now().Unix(), 10) }

	valid := func(r *http.Request) {
		ts := now()
		r.Header.Set("webhook-id", "msg_1")
		r.Header.Set("webhook-timestamp", ts)
		r.Header.Set("webhook-signature", signBody("msg_1", ts, body))
	}

	if got := runAuth(t, a, body, valid); got != 200 {
		t.Fatalf("valid signature: status %d, want 200", got)
	}

	// Tampered body: signature no longer matches the delivered body.
	if got := runAuth(t, a, `{"object_kind":"push"}`, valid); got != 401 {
		t.Fatalf("tampered body: status %d, want 401", got)
	}

	// Wrong key.
	if got := runAuth(t, a, body, func(r *http.Request) {
		ts := now()
		r.Header.Set("webhook-id", "msg_1")
		r.Header.Set("webhook-timestamp", ts)
		mac := hmac.New(sha256.New, []byte("the-wrong-key-the-wrong-key-!!!!"))
		mac.Write([]byte("msg_1." + ts + "." + body))
		r.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}); got != 401 {
		t.Fatalf("wrong key: status %d, want 401", got)
	}

	// Stale timestamp (10 minutes old, tolerance 5m).
	if got := runAuth(t, a, body, func(r *http.Request) {
		ts := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
		r.Header.Set("webhook-id", "msg_1")
		r.Header.Set("webhook-timestamp", ts)
		r.Header.Set("webhook-signature", signBody("msg_1", ts, body))
	}); got != 401 {
		t.Fatalf("stale timestamp: status %d, want 401", got)
	}

	// Missing headers.
	if got := runAuth(t, a, body, func(r *http.Request) {}); got != 401 {
		t.Fatalf("missing headers: status %d, want 401", got)
	}

	// Multi-entry header (key rotation): one entry matches.
	if got := runAuth(t, a, body, func(r *http.Request) {
		ts := now()
		r.Header.Set("webhook-id", "msg_1")
		r.Header.Set("webhook-timestamp", ts)
		r.Header.Set("webhook-signature", "v1,deadbeef "+signBody("msg_1", ts, body))
	}); got != 200 {
		t.Fatalf("multi-entry header: status %d, want 200", got)
	}

	// Future-skew beyond tolerance.
	if got := runAuth(t, a, body, func(r *http.Request) {
		ts := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
		r.Header.Set("webhook-id", "msg_1")
		r.Header.Set("webhook-timestamp", ts)
		r.Header.Set("webhook-signature", signBody("msg_1", ts, body))
	}); got != 401 {
		t.Fatalf("future timestamp: status %d, want 401", got)
	}

	// Non-numeric timestamp.
	if got := runAuth(t, a, body, func(r *http.Request) {
		r.Header.Set("webhook-id", "msg_1")
		r.Header.Set("webhook-timestamp", "not-a-number")
		r.Header.Set("webhook-signature", signBody("msg_1", "not-a-number", body))
	}); got != 401 {
		t.Fatalf("non-numeric timestamp: status %d, want 401", got)
	}

	// Whitespace-only signature header.
	if got := runAuth(t, a, body, func(r *http.Request) {
		ts := now()
		r.Header.Set("webhook-id", "msg_1")
		r.Header.Set("webhook-timestamp", ts)
		r.Header.Set("webhook-signature", "   ")
	}); got != 401 {
		t.Fatalf("whitespace signature: status %d, want 401", got)
	}
}

func TestNewSignatureAuthRejectsBadToken(t *testing.T) {
	if _, err := NewSignatureAuth("whsec_!!!not-base64!!!", time.Minute); err == nil {
		t.Fatal("expected error for non-base64 token")
	}
	if _, err := NewSignatureAuth("whsec_", time.Minute); err == nil {
		t.Fatal("expected error for empty token")
	}
}
