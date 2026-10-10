package inboxcontrol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
)

const previewTTL = 5 * time.Minute
const previewDomain = "msgvault:inbox-preview:v1:"

// PreviewClaims authenticate one caller's preview of an exact intent and state.
// IntentHash must cover the resolved source/account/item, operation and
// destination. StateHash must cover semantic provider state, excluding read
// observation timestamps. The service recomputes both hashes before dispatch.
type PreviewClaims struct {
	PrincipalID string    `json:"principal_id"`
	IntentHash  string    `json:"intent_hash"`
	StateHash   string    `json:"state_hash"`
	IssuedAt    time.Time `json:"issued_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// SignPreview produces a stateless, authenticated preview without writing to
// the archive. The daemon supplies its persisted secret; callers never get it.
func SignPreview(key []byte, claims PreviewClaims) (string, error) {
	if len(key) < 32 || !claims.valid() {
		return "", fmt.Errorf("%w: invalid preview claims or signing key", ErrInvalid)
	}
	payload, err := json.Marshal(claims, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("%w: cannot encode preview", ErrInvalid)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(previewDomain))
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// VerifyPreview checks issuance, expiry and the service's independently
// resolved principal/intent/state. A replay ledger must be consulted before
// this check: a completed idempotency key returns its authorized receipt even
// after this preview expires or provider state changes.
func VerifyPreview(key []byte, token, principalID, intentHash, stateHash string, now time.Time) error {
	invalid := func() error { return fmt.Errorf("%w: preview is invalid, changed or expired", ErrInvalid) }
	if len(key) < 32 || len(token) > 16384 {
		return invalid()
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return invalid()
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return invalid()
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return invalid()
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(previewDomain))
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return invalid()
	}
	var claims PreviewClaims
	if err := json.Unmarshal(payload, &claims, json.RejectUnknownMembers(true)); err != nil || !claims.valid() {
		return invalid()
	}
	if claims.PrincipalID != principalID || claims.IntentHash != intentHash || claims.StateHash != stateHash || now.Before(claims.IssuedAt) || !now.Before(claims.ExpiresAt) {
		return invalid()
	}
	return nil
}

func (c PreviewClaims) valid() bool {
	ttl := c.ExpiresAt.Sub(c.IssuedAt)
	return validIdentity(c.PrincipalID) && validDigest(c.IntentHash) && validDigest(c.StateHash) && !c.IssuedAt.IsZero() && ttl > 0 && ttl <= previewTTL
}

func validDigest(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
