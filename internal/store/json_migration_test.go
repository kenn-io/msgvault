package store

import (
	"crypto/sha256"
	"encoding/hex"
	legacyjson "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestAttributeValuePreservesPresentEmptyJSON(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{}`, `[]`, `""`, `null`} {
		t.Run(raw, func(t *testing.T) {
			require := require.New(t)
			value := AttributeValue{Type: AttributeValueJSON, JSON: jsontext.Value(raw)}
			encoded, err := json.Marshal(value)
			require.NoError(err)
			assert.JSONEq(t, `{"type":"json","json":`+raw+`}`, string(encoded))
			client := generated.AttributeValue{Type: "json", JSON: jsontext.Value(raw)}
			encoded, err = json.Marshal(client)
			require.NoError(err)
			assert.JSONEq(t, `{"type":"json","json":`+raw+`}`, string(encoded))
		})
	}
}

func TestPersonVCardFingerprintPreservesStoredJSONEncoding(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	snapshot := &PersonVCardSnapshot{}
	legacy, err := legacyjson.Marshal(personVCardFingerprintView(snapshot))
	require.NoError(err)
	digest := sha256.Sum256(legacy)
	got, err := personVCardSnapshotFingerprint(snapshot)
	require.NoError(err)
	assert.Equal(t, hex.EncodeToString(digest[:]), got)
}
