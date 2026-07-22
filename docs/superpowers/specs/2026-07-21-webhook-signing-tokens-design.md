# Webhook signing tokens — design

**Date:** 2026-07-21
**Status:** Approved (pending spec review)

## Problem

The webhook handler authenticates GitLab deliveries by comparing the
`X-Gitlab-Token` header against `WEBHOOK_SECRET` with a constant-time compare.
GitLab now marks the secret token as legacy ("not recommended for new
webhooks") and offers **signing tokens**: an HMAC-SHA256 signature over the
request that also protects integrity and enables replay protection.

We migrate to signing tokens while keeping a supported path for the legacy
secret token, so existing webhooks keep working during rollout.

## GitLab signing-token scheme

Reference: https://docs.gitlab.com/19.2/user/project/integrations/webhooks/#signing-tokens

When a signing token is configured, GitLab sends:

- `webhook-id` — unique delivery id.
- `webhook-timestamp` — Unix seconds.
- `webhook-signature` — space-separated list of `v1,{base64_signature}`
  entries (multiple entries during key rotation).

Verification:

1. `message = "{webhook-id}.{webhook-timestamp}.{raw-body}"`.
2. `key = base64_decode(strip_prefix(signing_token, "whsec_"))`.
3. `mac = HMAC_SHA256(key, message)`.
4. `expected = "v1," + base64_std(mac)`.
5. Constant-time compare `expected` against each entry in `webhook-signature`;
   any match ⇒ valid.

`X-Gitlab-Token` remains functional and works alongside the signing token.

## Decisions

- **Configurable methods, ordered.** `AUTH_METHODS` (comma-separated) lists the
  enabled auth methods in priority order, e.g. `secret,signature`. Allowed
  values: `secret`, `signature`. First method that authenticates a request
  wins.
- **Replay protection.** Reject a signed request when
  `|now − webhook-timestamp| > tolerance` (constant `5m`).
- **Separate config var.** Signing token comes from a new
  `WEBHOOK_SIGNING_TOKEN`; `WEBHOOK_SECRET` stays for the legacy path.

## Architecture

### `internal/config`

- `Config.AuthMethods []string` from `AUTH_METHODS`. Parsing: split on `,`,
  trim, lower-case, drop empties. Default (unset/empty) ⇒ `["secret"]`
  (backward compatible). Any value not in `{secret, signature}` ⇒ `Load`
  returns an error. An empty resulting list (e.g. `AUTH_METHODS=","`) ⇒ error.
- `Config.WebhookSigningToken` from `WEBHOOK_SIGNING_TOKEN`. `Config.WebhookSecret`
  unchanged.
- `Load` validates only the **method names**. Per-method token presence is a
  `serve`-time concern (below), keeping `bot run` unaffected — it never
  authenticates webhooks.

### `internal/webhook/auth.go` (new)

```go
type Authenticator interface {
    Name() string                    // "secret" | "signature"
    Authenticate(c fiber.Ctx) bool
}
```

- `secretAuth{secret []byte}` — `subtle.ConstantTimeCompare(X-Gitlab-Token, secret) == 1`.
  Moved verbatim from the current handler.
- `signatureAuth{key []byte, tolerance time.Duration, now func() time.Time}`:
  - read `webhook-id`, `webhook-timestamp`, `webhook-signature`; any missing ⇒ false.
  - parse timestamp as int64 Unix seconds; unparseable ⇒ false; stale beyond
    tolerance ⇒ false.
  - compute `expected` per the scheme; constant-time compare against each
    space-separated entry; match ⇒ true.
  - `now` is injectable so tests can pin time; `tolerance` injectable too.
- Constructors:
  - `NewSecretAuth(secret string) Authenticator`.
  - `NewSignatureAuth(signingToken string, tolerance time.Duration) (Authenticator, error)`
    — decodes/validates the `whsec_` token up front (bad token ⇒ error at wiring
    time, not per request). Exposes `DefaultTimestampTolerance = 5 * time.Minute`.

### `internal/webhook/handler.go`

- `NewApp(auths []Authenticator, queue Enqueuer, log *slog.Logger) *fiber.App`.
- `handler` holds `auths []Authenticator`.
- `handle` iterates `auths` in order; first `Authenticate` returning true ⇒
  proceed. None ⇒ `401`, no body. Auth runs before JSON parsing;
  `signatureAuth` reads raw `c.Body()`. Event filter, terminal-status filter,
  body-limit (413) and enqueue logic are unchanged.

### `cmd/bot/serve.go`

- Build the ordered `[]webhook.Authenticator` from `cfg.AuthMethods`:
  - `secret` ⇒ require `cfg.WebhookSecret != ""`, else error
    `"AUTH_METHODS includes \"secret\" but WEBHOOK_SECRET is not set"`.
  - `signature` ⇒ require `cfg.WebhookSigningToken != ""`, else analogous error;
    then `NewSignatureAuth(token, DefaultTimestampTolerance)` (propagate a bad
    token error).
- Replaces today's bare `cfg.WebhookSecret == ""` check.

## Error handling

- Missing/mismatched credentials for every enabled method ⇒ `401`, no body
  detail (unchanged security posture).
- Misconfiguration (enabled method without its token, malformed signing token)
  ⇒ `serve` fails fast at startup.
- Malformed signature/timestamp headers on a request ⇒ treated as auth failure
  (`401`), never a 500.

## Testing

- **Unit (`internal/webhook`)**, table-driven:
  - secret: valid ⇒ 200; wrong ⇒ 401; missing ⇒ 401.
  - signature: valid ⇒ 200; tampered body ⇒ 401; wrong key ⇒ 401; stale
    timestamp ⇒ 401; future-skew beyond tolerance ⇒ 401; missing any of the
    three headers ⇒ 401; multi-entry header where one entry matches ⇒ 200.
  - ordering: `secret,signature` and `signature,secret` both authenticate a
    request valid under either method; a request valid only under the
    non-enabled method ⇒ 401.
  - body-limit 413 preserved (real-listener test as today).
  - Test helper computes a real `webhook-signature` (`v1,{base64(hmac)}`).
- **e2e (`internal/e2e`)**: switch the chain to `AUTH_METHODS=signature` with a
  signing token, so the signature path is proven webhook→queue→worker→GitLab.

## Docs & chart

- `CLAUDE.md` webhook-security section: document `AUTH_METHODS`,
  `WEBHOOK_SIGNING_TOKEN`, the signing scheme, and replay window.
- `README`: config table + migration note.
- Helm chart: add `WEBHOOK_SIGNING_TOKEN` (from `secrets.existingSecret`) and
  `AUTH_METHODS` env; NetworkPolicy/ports unchanged.

## Out of scope (YAGNI)

- Timestamp tolerance as an env var (constant `5m`).
- Metrics/labels for which auth method succeeded.
- Removing the legacy secret path (kept until a follow-up deprecation).
