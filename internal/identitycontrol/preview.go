package identitycontrol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"
)

const PreviewTTL = 5 * time.Minute
const identityPreviewDomain = "msgvault:identity-preview:v1:"
const maxPreviewTokenBytes = 8192

var ErrInvalidPreview = errors.New("identity preview is invalid, changed or expired; preview again")

// PreviewBinding authenticates an exact operation and the complete native
// evidence digest. AuthorityFingerprint identifies current principal/grant
// authority; it contains no credential. Native services must resolve both
// digests independently and reauthorize current evidence before mutation.
type PreviewBinding struct {
	PrincipalID          string         `json:"principal_id"`
	AuthorityFingerprint string         `json:"authority_fingerprint"`
	Request              PreviewRequest `json:"request"`
	Fingerprint          string         `json:"fingerprint"`
}

type PreviewClaims struct {
	Binding   PreviewBinding `json:"binding"`
	IssuedAt  time.Time      `json:"issued_at"`
	ExpiresAt time.Time      `json:"expires_at"`
}

// SignPreview signs only identifiers and digests. It is read-only and does not
// authorize an operation by itself. The daemon owns the signing key.
func SignPreview(key []byte, claims PreviewClaims) (string, error) {
	if len(key) < 32 || !claims.valid() {
		return "", ErrInvalidPreview
	}
	claims.Binding = claims.Binding.canonical()
	payload, err := json.Marshal(claims, json.Deterministic(true))
	if err != nil {
		return "", ErrInvalidPreview
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(identityPreviewDomain))
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// VerifyPreview checks the signature, bounded lifetime and independently
// resolved current binding. It cannot replace native scope/revision guards or
// a receipt lookup for an unknown outcome after a committed operation.
func VerifyPreview(key []byte, token string, expected PreviewBinding, now time.Time) error {
	if len(key) < 32 || len(token) > maxPreviewTokenBytes || !expected.valid() {
		return ErrInvalidPreview
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return ErrInvalidPreview
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return ErrInvalidPreview
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return ErrInvalidPreview
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(identityPreviewDomain))
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return ErrInvalidPreview
	}
	var claims PreviewClaims
	if err := json.Unmarshal(payload, &claims, json.RejectUnknownMembers(true)); err != nil || !claims.valid() {
		return ErrInvalidPreview
	}
	if claims.Binding.canonical() != expected.canonical() || now.Before(claims.IssuedAt) || !now.Before(claims.ExpiresAt) {
		return ErrInvalidPreview
	}
	return nil
}

func (binding PreviewBinding) canonical() PreviewBinding {
	binding.Request.Target = binding.Request.Target.Canonical(binding.Request.Operation)
	return binding
}

func (binding PreviewBinding) valid() bool {
	return strings.TrimSpace(binding.PrincipalID) != "" && len(binding.PrincipalID) <= 256 && !strings.ContainsRune(binding.PrincipalID, 0) && binding.Request.Validate() == nil && validPreviewDigest(binding.AuthorityFingerprint) && validPreviewDigest(binding.Fingerprint)
}

func (claims PreviewClaims) valid() bool {
	ttl := claims.ExpiresAt.Sub(claims.IssuedAt)
	return claims.Binding.valid() && !claims.IssuedAt.IsZero() && ttl > 0 && ttl <= PreviewTTL
}

func validPreviewDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
