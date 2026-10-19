package client_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestEmailFamilyExportBodyPreservesEmptySourceSelection(t *testing.T) {
	body := generated.ExportEmailFamilyBody{
		ExpectedFamilyRevision: "revision",
		SourceIds:              []int64{},
	}

	encoded, err := json.Marshal(body)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, []any{}, decoded["source_ids"])

	body.SourceIds = nil
	encoded, err = json.Marshal(body)
	require.NoError(t, err)
	var omitted map[string]any
	require.NoError(t, json.Unmarshal(encoded, &omitted))
	assert.NotContains(t, omitted, "source_ids")
}
