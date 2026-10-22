package client_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestEmailFamilyExportBodyPreservesEmptySourceSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	body := generated.ExportEmailFamilyBody{
		ExpectedFamilyRevision: "revision",
		SourceIds:              []int64{},
	}

	encoded, err := json.Marshal(body)
	require.NoError(err)

	var decoded map[string]any
	require.NoError(json.Unmarshal(encoded, &decoded))
	assert.Equal([]any{}, decoded["source_ids"])

	body.SourceIds = nil
	encoded, err = json.Marshal(body)
	require.NoError(err)
	var omitted map[string]any
	require.NoError(json.Unmarshal(encoded, &omitted))
	assert.NotContains(omitted, "source_ids")
}
