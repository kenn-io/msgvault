package metawebhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

var ErrSignatureMissing = errors.New("missing webhook signature")
var ErrSignatureMismatch = errors.New("invalid webhook signature")

// VerifySignature authenticates the exact received bytes, including JSON escapes.
func VerifySignature(secret, body []byte, header string) error {
	if header == "" {
		return ErrSignatureMissing
	}
	if !strings.HasPrefix(header, "sha256=") {
		return ErrSignatureMismatch
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil || len(got) != sha256.Size {
		return ErrSignatureMismatch
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), got) {
		return ErrSignatureMismatch
	}
	return nil
}
