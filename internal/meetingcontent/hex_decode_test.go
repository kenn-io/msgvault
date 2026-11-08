package meetingcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHexDecodeFutureVersionUnavailable(t *testing.T) {
	content := Decode("hex_json", []byte(`{"version":2}`), nil)
	assert.Equal(t, StateUnavailable, content.Transcript.State)
	assert.Equal(t, "unsupported_version", content.Transcript.Reason)
	assert.Equal(t, "unsupported_version", content.ActionReason)
}

func TestHexDecodeTypedEnvelope(t *testing.T) {
	must, check := require.New(t), assert.New(t)
	content := Decode("hex_json", []byte(`{"version":1,"meeting":{"manifest":{"schema_version":1,"id":"synthetic","title":"Example call","status":"complete","created_at_ms":1790000000000},"transcript_state":"final","entries":[{"source":"system","start_ms":0,"end_ms":1000,"text":"Synthetic decoded words"}]}}`), nil)
	must.Equal(StateAvailable, content.Transcript.State)
	check.Contains(content.Transcript.Text, "Synthetic decoded words")
	check.Contains(content.Transcript.Text, "Them")
	check.Empty(content.Transcript.Reason)
}
