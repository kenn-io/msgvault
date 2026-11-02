package tiktok

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommentObjectsPreserveUnknownProviderFields(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		value      any
	}{
		{"video", `{"item_id":9007199254740993,"provider_extension":{"id":9007199254740999}}`, &Video{}},
		{"comment", `{"comment_id":9007199254740993,"provider_extension":{"id":9007199254740999}}`, &Comment{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			requirements.NoError(json.Unmarshal([]byte(tc.body), tc.value))
			data, err := json.Marshal(tc.value)
			requirements.NoError(err)
			var fields map[string]jsontext.Value
			requirements.NoError(json.Unmarshal(data, &fields))
			requirements.Contains(fields, "provider_extension")
			assert.Equal(t, `{"id":9007199254740999}`, string(fields["provider_extension"]))
		})
	}
}
