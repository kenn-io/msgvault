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
func TestVerifySignatureRejects(t *testing.T) {
	require := require.New(t)
	body := []byte("synthetic")
	for _, header := range []string{"sha1=123", "sha256=abc", "sha256=" + hex.EncodeToString(make([]byte, 32))} {
		require.Error(VerifySignature([]byte("synthetic-app-secret"), body, header))
	}
	require.Error(VerifySignature([]byte("other"), body, signed(body)))
	require.ErrorIs(VerifySignature([]byte("synthetic-app-secret"), body, ""), ErrSignatureMissing)
	require.ErrorIs(VerifySignature([]byte("synthetic-app-secret"), body, signed([]byte("other"))), ErrSignatureMismatch)
}
func TestVerifySignatureRawEscapedUnicode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte(`{"m":"\u00e4"}`)
	decoded := []byte(`{"m":"ä"}`)
	require.NoError(VerifySignature([]byte("synthetic-app-secret"), raw, signed(raw)))
	require.Error(VerifySignature([]byte("synthetic-app-secret"), raw, signed(decoded)))
	var value map[string]string
	require.NoError(json.Unmarshal(raw, &value))
	encoded, err := json.Marshal(value)
	require.NoError(err)
	assert.NotEqual(raw, encoded)
}
