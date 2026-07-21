# Webhook Signing Tokens Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add GitLab signing-token (HMAC-SHA256) webhook authentication alongside the legacy secret token, selectable and ordered via `AUTH_METHODS`.

**Architecture:** Introduce an `Authenticator` interface in `internal/webhook` with two implementations — `secretAuth` (constant-time `X-Gitlab-Token` compare, moved from the handler) and `signatureAuth` (verifies GitLab's `webhook-signature` with replay protection). `NewApp` takes an ordered `[]Authenticator`; the handler accepts a request when any authenticator (tried in order) succeeds. `cmd/bot/serve.go` builds the slice from config; `config` parses and validates `AUTH_METHODS` and reads `WEBHOOK_SIGNING_TOKEN`.

**Tech Stack:** Go ≥1.26, `gofiber/fiber/v3`, stdlib `crypto/hmac`, `crypto/sha256`, `crypto/subtle`, `encoding/base64`.

**Design:** `docs/superpowers/specs/2026-07-21-webhook-signing-tokens-design.md`

---

## Task 1: Config — `AUTH_METHODS` + `WEBHOOK_SIGNING_TOKEN`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/config/config_test.go`:

```go
package config

import (
	"reflect"
	"testing"
)

func TestParseAuthMethods(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "empty defaults to secret", raw: "", want: []string{"secret"}},
		{name: "whitespace defaults to secret", raw: "   ", want: []string{"secret"}},
		{name: "single signature", raw: "signature", want: []string{"signature"}},
		{name: "ordered pair", raw: "secret,signature", want: []string{"secret", "signature"}},
		{name: "reversed order preserved", raw: "signature,secret", want: []string{"signature", "secret"}},
		{name: "trims and lowercases", raw: " Secret , SIGNATURE ", want: []string{"secret", "signature"}},
		{name: "skips empty entries", raw: "secret,,signature", want: []string{"secret", "signature"}},
		{name: "unknown method errors", raw: "secret,bogus", wantErr: true},
		{name: "only commas errors", raw: ",,", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAuthMethods(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseAuthMethods(%q) = %v, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAuthMethods(%q): unexpected error %v", tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseAuthMethods(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestParseAuthMethods -v`
Expected: FAIL — `undefined: parseAuthMethods`.

- [ ] **Step 3: Add config fields and parser**

In `internal/config/config.go`, add `"strings"` to the import block. Add fields to `Config` right after `WebhookSecret string`:

```go
	WebhookSecret       string
	WebhookSigningToken string
	AuthMethods         []string
```

In `Load`, set `WebhookSigningToken` in the struct literal next to `WebhookSecret`:

```go
		WebhookSecret:       os.Getenv("WEBHOOK_SECRET"),
		WebhookSigningToken: os.Getenv("WEBHOOK_SIGNING_TOKEN"),
```

Immediately before the final `return cfg, nil`, parse the methods:

```go
	methods, err := parseAuthMethods(os.Getenv("AUTH_METHODS"))
	if err != nil {
		return nil, err
	}
	cfg.AuthMethods = methods
```

At the end of the file add:

```go
var validAuthMethods = map[string]bool{"secret": true, "signature": true}

// parseAuthMethods parses the comma-separated, ordered AUTH_METHODS list.
// Order is significant (the handler tries methods in this order). An empty
// value defaults to the legacy ["secret"] for backward compatibility.
func parseAuthMethods(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{"secret"}, nil
	}
	var methods []string
	for _, part := range strings.Split(raw, ",") {
		m := strings.ToLower(strings.TrimSpace(part))
		if m == "" {
			continue
		}
		if !validAuthMethods[m] {
			return nil, fmt.Errorf("AUTH_METHODS: unknown method %q (allowed: secret, signature)", m)
		}
		methods = append(methods, m)
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("AUTH_METHODS is set but lists no valid methods")
	}
	return methods, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestParseAuthMethods -v`
Expected: PASS (all sub-tests).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): parse AUTH_METHODS and WEBHOOK_SIGNING_TOKEN"
```

---

## Task 2: `Authenticator` interface + `secretAuth` + `signatureAuth`

**Files:**
- Create: `internal/webhook/auth.go`
- Test: `internal/webhook/auth_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/webhook/auth_test.go`:

```go
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
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
}

func TestNewSignatureAuthRejectsBadToken(t *testing.T) {
	if _, err := NewSignatureAuth("whsec_!!!not-base64!!!", time.Minute); err == nil {
		t.Fatal("expected error for non-base64 token")
	}
	if _, err := NewSignatureAuth("whsec_", time.Minute); err == nil {
		t.Fatal("expected error for empty token")
	}
}
```

Note: this test file uses `strings`; add it to the import block (`"strings"`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webhook/ -run 'TestSecretAuth|TestSignatureAuth|TestNewSignatureAuth' -v`
Expected: FAIL — `undefined: NewSecretAuth` / `undefined: NewSignatureAuth`.

- [ ] **Step 3: Implement `auth.go`**

Create `internal/webhook/auth.go`:

```go
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
)

// DefaultTimestampTolerance bounds how far a signed webhook's timestamp may be
// from now before it is rejected as a replay.
const DefaultTimestampTolerance = 5 * time.Minute

// Authenticator verifies that a webhook request is authentic. Implementations
// must be safe for concurrent use and must not read the body destructively.
type Authenticator interface {
	Name() string
	Authenticate(c fiber.Ctx) bool
}

// secretAuth validates the legacy X-Gitlab-Token shared secret.
type secretAuth struct{ secret []byte }

// NewSecretAuth builds an Authenticator for the legacy secret token.
func NewSecretAuth(secret string) Authenticator {
	return secretAuth{secret: []byte(secret)}
}

func (a secretAuth) Name() string { return "secret" }

func (a secretAuth) Authenticate(c fiber.Ctx) bool {
	token := []byte(c.Get("X-Gitlab-Token"))
	return subtle.ConstantTimeCompare(token, a.secret) == 1
}

// signatureAuth validates GitLab signing-token HMAC-SHA256 signatures.
// See https://docs.gitlab.com/user/project/integrations/webhooks/#signing-tokens
type signatureAuth struct {
	key       []byte
	tolerance time.Duration
	now       func() time.Time
}

// NewSignatureAuth builds an Authenticator for GitLab signing tokens. The
// token is the whsec_-prefixed value shown in the GitLab UI; it is decoded
// and validated up front so a misconfiguration fails at startup, not per
// request.
func NewSignatureAuth(signingToken string, tolerance time.Duration) (Authenticator, error) {
	raw := strings.TrimPrefix(signingToken, "whsec_")
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode signing token: %w", err)
	}
	if len(key) == 0 {
		return nil, errors.New("signing token decodes to an empty key")
	}
	return signatureAuth{key: key, tolerance: tolerance, now: time.Now}, nil
}

func (a signatureAuth) Name() string { return "signature" }

func (a signatureAuth) Authenticate(c fiber.Ctx) bool {
	id := c.Get("webhook-id")
	tsStr := c.Get("webhook-timestamp")
	sigHeader := c.Get("webhook-signature")
	if id == "" || tsStr == "" || sigHeader == "" {
		return false
	}

	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false
	}
	if skew := a.now().Unix() - ts; skew > int64(a.tolerance/time.Second) ||
		-skew > int64(a.tolerance/time.Second) {
		return false
	}

	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(id + "." + tsStr + "." + string(c.Body())))
	expected := []byte("v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	// GitLab may send several space-separated signatures during key rotation.
	for _, entry := range strings.Fields(sigHeader) {
		if subtle.ConstantTimeCompare([]byte(entry), expected) == 1 {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/webhook/ -run 'TestSecretAuth|TestSignatureAuth|TestNewSignatureAuth' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/auth.go internal/webhook/auth_test.go
git commit -m "feat(webhook): add Authenticator with secret and signature verifiers"
```

---

## Task 3: Handler consumes ordered `[]Authenticator`

**Files:**
- Modify: `internal/webhook/handler.go`
- Modify: `internal/webhook/handler_test.go`

- [ ] **Step 1: Update handler tests to the new `NewApp` signature and add an ordering test**

In `internal/webhook/handler_test.go`, replace **both** `NewApp(secret, queue, ...)` / `NewApp(secret, ...)` call sites:

In `TestHandler`:

```go
			queue := &fakeQueue{full: tt.queueFull}
			app := NewApp([]Authenticator{NewSecretAuth(secret)}, queue, slog.New(slog.DiscardHandler))
```

In `TestOversizedBodyRejected`:

```go
	app := NewApp([]Authenticator{NewSecretAuth(secret)}, queue, slog.New(slog.DiscardHandler))
```

Then add a new ordering test at the end of the file:

```go
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
```

Add `"strconv"` and `"time"` to `handler_test.go`'s import block (the ordering test uses them). `signBody` and `testSigningToken` come from `auth_test.go` in the same package.

- [ ] **Step 2: Run tests to verify they fail to compile**

Run: `go test ./internal/webhook/ -run TestHandler -v`
Expected: FAIL — `NewApp` signature mismatch (too many arguments / cannot use `secret`).

- [ ] **Step 3: Change the handler to accept `[]Authenticator`**

In `internal/webhook/handler.go`, remove `"crypto/subtle"` from the imports (the compare now lives in `auth.go`). Replace `NewApp`, the `handler` struct, and the auth check:

```go
// NewApp builds the webhook Fiber app: POST /webhook authenticated by the
// given authenticators (tried in order, first success wins), with event
// filtering and a 1 MiB body limit.
func NewApp(auths []Authenticator, queue Enqueuer, log *slog.Logger) *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit:    maxBodyBytes,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	})
	h := &handler{auths: auths, queue: queue, log: log}
	app.Post("/webhook", h.handle)
	return app
}

type handler struct {
	auths []Authenticator
	queue Enqueuer
	log   *slog.Logger
}

func (h *handler) authenticate(c fiber.Ctx) bool {
	for _, a := range h.auths {
		if a.Authenticate(c) {
			return true
		}
	}
	return false
}
```

In `handle`, replace the opening token block:

```go
func (h *handler) handle(c fiber.Ctx) error {
	if !h.authenticate(c) {
		return c.SendStatus(fiber.StatusUnauthorized) // deliberately no body detail
	}
```

Leave everything below (event filter, JSON parse, terminal-status filter, enqueue) unchanged.

- [ ] **Step 4: Run the full webhook package tests**

Run: `go test ./internal/webhook/ -v`
Expected: PASS — including `TestHandler`, `TestOversizedBodyRejected`, `TestHandlerAuthOrdering`, and Task 2's auth tests.

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/handler.go internal/webhook/handler_test.go
git commit -m "feat(webhook): authenticate via ordered Authenticator list"
```

---

## Task 4: Wire authenticators in `serve.go`

**Files:**
- Modify: `cmd/bot/serve.go`
- Test: `cmd/bot/serve_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `cmd/bot/serve_test.go`:

```go
package main

import (
	"testing"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"
)

func TestBuildAuthenticators(t *testing.T) {
	signing := "whsec_" + "MDEyMzQ1Njc4OWFiY2RlZg==" // base64("0123456789abcdef")

	tests := []struct {
		name      string
		cfg       *config.Config
		wantNames []string
		wantErr   bool
	}{
		{
			name:      "secret only",
			cfg:       &config.Config{AuthMethods: []string{"secret"}, WebhookSecret: "x"},
			wantNames: []string{"secret"},
		},
		{
			name:      "signature only",
			cfg:       &config.Config{AuthMethods: []string{"signature"}, WebhookSigningToken: signing},
			wantNames: []string{"signature"},
		},
		{
			name:      "ordered pair preserves order",
			cfg:       &config.Config{AuthMethods: []string{"signature", "secret"}, WebhookSecret: "x", WebhookSigningToken: signing},
			wantNames: []string{"signature", "secret"},
		},
		{
			name:    "secret enabled but unset",
			cfg:     &config.Config{AuthMethods: []string{"secret"}},
			wantErr: true,
		},
		{
			name:    "signature enabled but unset",
			cfg:     &config.Config{AuthMethods: []string{"signature"}},
			wantErr: true,
		},
		{
			name:    "signature token invalid",
			cfg:     &config.Config{AuthMethods: []string{"signature"}, WebhookSigningToken: "whsec_@@@"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auths, err := buildAuthenticators(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", auths)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var names []string
			for _, a := range auths {
				names = append(names, a.Name())
			}
			if len(names) != len(tt.wantNames) {
				t.Fatalf("names = %v, want %v", names, tt.wantNames)
			}
			for i := range names {
				if names[i] != tt.wantNames[i] {
					t.Fatalf("names = %v, want %v", names, tt.wantNames)
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/bot/ -run TestBuildAuthenticators -v`
Expected: FAIL — `undefined: buildAuthenticators`.

- [ ] **Step 3: Add `buildAuthenticators` and rewire `serve`**

In `cmd/bot/serve.go`, delete the old guard:

```go
	if cfg.WebhookSecret == "" {
		return errors.New("missing required environment variable WEBHOOK_SECRET")
	}
```

Replace the `app := webhook.NewApp(cfg.WebhookSecret, q, log)` line with:

```go
	auths, err := buildAuthenticators(cfg)
	if err != nil {
		return err
	}
	app := webhook.NewApp(auths, q, log)
```

(`err` is already declared earlier in `serve`, so keep using `=`, not `:=`.)

Add at the end of the file:

```go
// buildAuthenticators turns the ordered cfg.AuthMethods into webhook
// authenticators, failing fast when an enabled method's credential is absent
// or malformed.
func buildAuthenticators(cfg *config.Config) ([]webhook.Authenticator, error) {
	var auths []webhook.Authenticator
	for _, m := range cfg.AuthMethods {
		switch m {
		case "secret":
			if cfg.WebhookSecret == "" {
				return nil, errors.New(`AUTH_METHODS includes "secret" but WEBHOOK_SECRET is not set`)
			}
			auths = append(auths, webhook.NewSecretAuth(cfg.WebhookSecret))
		case "signature":
			if cfg.WebhookSigningToken == "" {
				return nil, errors.New(`AUTH_METHODS includes "signature" but WEBHOOK_SIGNING_TOKEN is not set`)
			}
			a, err := webhook.NewSignatureAuth(cfg.WebhookSigningToken, webhook.DefaultTimestampTolerance)
			if err != nil {
				return nil, fmt.Errorf("signature auth: %w", err)
			}
			auths = append(auths, a)
		default:
			return nil, fmt.Errorf("unknown auth method %q", m)
		}
	}
	return auths, nil
}
```

`errors` and `fmt` are already imported in `serve.go`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/bot/ -run TestBuildAuthenticators -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bot/serve.go cmd/bot/serve_test.go
git commit -m "feat(bot): build ordered webhook authenticators from AUTH_METHODS"
```

---

## Task 5: e2e exercises the signature path

**Files:**
- Modify: `internal/e2e/e2e_test.go`

- [ ] **Step 1: Update the harness and delivery to sign requests**

In `internal/e2e/e2e_test.go`, add these imports to the import block:

```go
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"time"
```

Add a signing key near the existing `secret = "e2e-secret"` const (keep `secret` — unused package consts are legal, and it documents the legacy value):

```go
const signingKeyRaw = "e2e-signing-key-0123456789abcdef"

func e2eSigningToken() string {
	return "whsec_" + base64.StdEncoding.EncodeToString([]byte(signingKeyRaw))
}
```

Replace the harness return (line ~207):

```go
	sigAuth, err := webhook.NewSignatureAuth(e2eSigningToken(), webhook.DefaultTimestampTolerance)
	if err != nil {
		t.Fatalf("signature auth: %v", err)
	}
	return webhook.NewApp([]webhook.Authenticator{sigAuth}, chanQueue(q), log), glMock, promMock
```

Replace the two header lines in `postWebhook` (currently `X-Gitlab-Token` + `X-Gitlab-Event`) with signed headers:

```go
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	const msgID = "e2e-msg"
	mac := hmac.New(sha256.New, []byte(signingKeyRaw))
	mac.Write([]byte(msgID + "." + ts + "." + payload))
	req.Header.Set("webhook-id", msgID)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Gitlab-Event", "Pipeline Hook")
```

- [ ] **Step 2: Run the e2e test**

Run: `go test ./internal/e2e/ -v -count=1`
Expected: PASS — the full webhook→queue→worker→GitLab chain now authenticates via signature.

- [ ] **Step 3: Run the whole suite with the race detector**

Run: `mise r test`
Expected: PASS across all packages.

- [ ] **Step 4: Commit**

```bash
git add internal/e2e/e2e_test.go
git commit -m "test(e2e): drive the webhook chain through signature auth"
```

---

## Task 6: Docs and Helm chart

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md`
- Modify: `deploy/chart/cigar/templates/deployment.yaml`
- Modify: `deploy/chart/cigar/templates/secret.yaml`
- Modify: `deploy/chart/cigar/values.yaml`

- [ ] **Step 1: Add the signing-token env to the Deployment**

In `deploy/chart/cigar/templates/deployment.yaml`, insert after the `WEBHOOK_SECRET` env block (before `GITLAB_TOKEN`):

```yaml
            {{- if .Values.config.authMethods }}
            - name: AUTH_METHODS
              value: {{ .Values.config.authMethods | quote }}
            {{- end }}
            {{- if or .Values.secrets.signingToken .Values.secrets.existingSecret }}
            - name: WEBHOOK_SIGNING_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.secrets.existingSecret | default (include "cigar.fullname" .) }}
                  key: WEBHOOK_SIGNING_TOKEN
            {{- end }}
```

- [ ] **Step 2: Add the key to the chart-managed Secret**

In `deploy/chart/cigar/templates/secret.yaml`, after the `WEBHOOK_SECRET` line add:

```yaml
  WEBHOOK_SIGNING_TOKEN: {{ .Values.secrets.signingToken | default "" | quote }}
```

- [ ] **Step 3: Document the new values**

In `deploy/chart/cigar/values.yaml`, under `secrets:` add (next to `webhookSecret`):

```yaml
  # WEBHOOK_SIGNING_TOKEN: GitLab signing token (whsec_...). Required only when
  # authMethods includes "signature". Prefer existingSecret in production.
  signingToken: ""
```

Under `config:` add:

```yaml
  # AUTH_METHODS: ordered, comma-separated webhook auth methods (secret,signature).
  # Empty defaults to "secret" (legacy X-Gitlab-Token).
  authMethods: ""
```

- [ ] **Step 4: Validate the chart renders**

Run: `helm lint deploy/chart/cigar && helm template deploy/chart/cigar --set secrets.signingToken=whsec_abc --set config.authMethods=secret,signature | grep -A2 -E 'AUTH_METHODS|WEBHOOK_SIGNING_TOKEN'`
Expected: `helm lint` reports 0 failures; the template output shows both env vars. (IDE YAML errors on Go template syntax are noise.)

- [ ] **Step 5: Update `CLAUDE.md` and `README.md`**

In `CLAUDE.md`, in the "Webhook security" section, replace the first bullet (the `X-Gitlab-Token` line) with:

```markdown
- Authenticate via `AUTH_METHODS` (ordered, comma-separated: `secret`, `signature`; default `secret`). `secret`: constant-time compare of `X-Gitlab-Token` against `WEBHOOK_SECRET`. `signature`: GitLab signing token — verify the `webhook-signature` HMAC-SHA256 over `{webhook-id}.{webhook-timestamp}.{body}` using `WEBHOOK_SIGNING_TOKEN` (whsec_), rejecting timestamps outside a 5m window (replay protection). Any enabled method that authenticates the request wins; none → `401`, no body detail.
```

In the `CLAUDE.md` "Config (env only, 12-factor)" section, add `AUTH_METHODS` (default `secret`) and `WEBHOOK_SIGNING_TOKEN` to the variable list, and note `WEBHOOK_SECRET` is required only when `secret` is an enabled method.

In `README.md`, add `AUTH_METHODS` and `WEBHOOK_SIGNING_TOKEN` to the configuration/env table and a one-line migration note: "To move off the legacy secret token, set `AUTH_METHODS=secret,signature` with both credentials configured, migrate your GitLab webhooks to the signing token, then switch to `AUTH_METHODS=signature`."

- [ ] **Step 6: Final verification**

Run: `mise r lint test`
Expected: lint clean, all tests pass with the race detector.

- [ ] **Step 7: Commit**

```bash
git add CLAUDE.md README.md deploy/chart/cigar/
git commit -m "docs,chart: document AUTH_METHODS and WEBHOOK_SIGNING_TOKEN"
```

---

## Self-Review Notes

- **Spec coverage:** `AUTH_METHODS` ordering (Task 1, 3, 4), `WEBHOOK_SIGNING_TOKEN` separate var (Task 1, 4, 6), replay window (Task 2), `Authenticator` interface + both verifiers (Task 2), first-match handler (Task 3), fail-fast wiring (Task 4), e2e signature proof (Task 5), docs + chart (Task 6). All spec sections mapped.
- **Type consistency:** `Authenticator{Name(), Authenticate(fiber.Ctx) bool}`, `NewSecretAuth(string) Authenticator`, `NewSignatureAuth(string, time.Duration) (Authenticator, error)`, `DefaultTimestampTolerance`, `NewApp([]Authenticator, Enqueuer, *slog.Logger)`, `buildAuthenticators(*config.Config) ([]webhook.Authenticator, error)`, `config.Config.{WebhookSigningToken, AuthMethods}`, `parseAuthMethods(string) ([]string, error)` — used consistently across tasks.
- **No placeholders:** every code step is complete.
```
