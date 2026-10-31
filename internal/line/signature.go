package line

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// Verify authenticates the exact webhook bytes before decoding or sanitizing.
func Verify(secret string, body []byte, signature string) bool {
	// A SHA-256 signature is exactly 44 base64 characters. Reject whitespace
	// rather than accepting base64's otherwise permitted CR/LF characters.
	if len(signature) != 44 {
		return false
	}
	provided, err := base64.StdEncoding.Strict().DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(mac.Sum(nil), provided)
}
