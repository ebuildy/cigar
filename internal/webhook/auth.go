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
	if len(a.secret) == 0 {
		return false
	}
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
