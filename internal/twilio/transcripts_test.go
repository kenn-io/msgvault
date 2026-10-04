package twilio

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSentenceOptionalNumbersRemainAbsent(t *testing.T) {
	for _, fields := range []string{``, `,"media_channel":null,"start_time":null,"end_time":null`} {
		t.Run(fields, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			var value sentence
			require.NoError(json.Unmarshal([]byte(`{"sentence_index":0,"transcript":"Retained text"`+fields+`}`), &value))
			assert.Nil(value.Start)
			assert.Nil(value.End)
			assert.Nil(value.Channel)
			require.NotNil(value.Text)
			assert.Equal("Retained text", *value.Text)
		})
	}
}

func TestRawEvidenceReportsSerializationFailure(t *testing.T) {
	raw, err := rawEvidence([]json.RawMessage{json.RawMessage(`{"incomplete":`)})
	require.Error(t, err)
	assert.Nil(t, raw)
}
