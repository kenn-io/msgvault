package meetingcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHexDecodeErrorCategories(t *testing.T) {
	for _, tc := range []struct{ name, raw, reason string }{
		{"empty", "", "missing_raw"},
		{"whitespace", " \t", "missing_raw"},
		{"incomplete JSON", "{", "invalid_raw"},
		{"array", "[]", "invalid_raw"},
		{"scalar", "42", "invalid_raw"},
		{"null", "null", "invalid_raw"},
		{"duplicate key", `{"version":1,"version":2}`, "invalid_raw"},
		{"trailing value", `{"version":1}true`, "invalid_raw"},
		{"invalid UTF8", "{\"value\":\"" + string([]byte{0xff}) + "\"}", "invalid_raw"},
		{"typed error before malformed suffix", `{"version":"bad","meeting":}`, "invalid_raw"},
		{"invalid version type", `{"version":"bad"}`, "invalid_section"},
		{"invalid meeting type", `{"version":1,"meeting":"bad"}`, "invalid_section"},
		{"invalid domain", `{"version":1,"meeting":{}}`, "invalid_section"},
		{"future version", `{"version":2}`, "unsupported_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := Decode("hex_json", []byte(tc.raw), nil)
			assert.Equal(t, StateUnavailable, content.Transcript.State)
			assert.Equal(t, tc.reason, content.Transcript.Reason)
			assert.Equal(t, tc.reason, content.ActionReason)
		})
	}
}

func TestHexDecodeTypedEnvelope(t *testing.T) {
	content := Decode("hex_json", []byte(`{"version":1,"meeting":{"manifest":{"schema_version":1,"id":"synthetic","title":"Example call","status":"complete","created_at_ms":1790000000000},"transcript_state":"final","entries":[{"source":"system","start_ms":0,"end_ms":1000,"text":"Synthetic decoded words"}]}}`), nil)
	require.Equal(t, StateAvailable, content.Transcript.State)
	assert.Contains(t, content.Transcript.Text, "Synthetic decoded words")
	assert.Empty(t, content.Transcript.Reason)
}
