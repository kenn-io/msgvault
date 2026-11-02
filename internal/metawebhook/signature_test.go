package metawebhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func signed(body []byte) string {
	mac := hmac.New(sha256.New, []byte("synthetic-app-secret"))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
func TestVerifySignatureValid(t *testing.T) {
	body := []byte(`{"object":"page"}`)
	require.NoError(t, VerifySignature([]byte("synthetic-app-secret"), body, signed(body)))
}
func TestVerifySignatureRejects(t *testing.T) {
	body := []byte("synthetic")
	for _, header := range []string{"", "sha1=123", "sha256=abc", "sha256=" + hex.EncodeToString(make([]byte, 32)), signed([]byte("changed"))} {
		assert.Error(t, VerifySignature([]byte("synthetic-app-secret"), body, header))
	}
	assert.Error(t, VerifySignature([]byte("other"), body, signed(body)))
	assert.ErrorIs(t, VerifySignature([]byte("synthetic-app-secret"), body, ""), ErrSignatureMissing)
	assert.ErrorIs(t, VerifySignature([]byte("synthetic-app-secret"), body, signed([]byte("other"))), ErrSignatureMismatch)
}
func TestVerifySignatureRawEscapedUnicode(t *testing.T) {
	raw := []byte(`{"m":"\u00e4"}`)
	decoded := []byte(`{"m":"ä"}`)
	require.NoError(t, VerifySignature([]byte("synthetic-app-secret"), raw, signed(raw)))
	assert.Error(t, VerifySignature([]byte("synthetic-app-secret"), raw, signed(decoded)))
	var value map[string]string
	require.NoError(t, json.Unmarshal(raw, &value))
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	assert.NotEqual(t, raw, encoded)
}
