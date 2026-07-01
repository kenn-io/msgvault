package daemonclient

import (
	"encoding/json"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestCLIDeleteDedupedExecuteBodyKeepsEmptyExpectedBatches(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)

	expectedTotal := int64(0)
	expectedBatchCount := int64(0)
	body := cliDeleteDedupedExecuteBodyFromRequest(CLIDeleteDedupedRequest{
		BatchIDs:           []string{"batch-a"},
		ExpectedTotal:      &expectedTotal,
		ExpectedBatchCount: &expectedBatchCount,
		ExpectedBatches:    []CLIDeleteDedupedBatch{},
	})

	data, err := json.Marshal(body)
	require.NoError(err, "marshal execute body")

	var decoded map[string]any
	require.NoError(json.Unmarshal(data, &decoded), "decode execute body")
	expectedBatches, ok := decoded["expected_batches"].([]any)
	require.True(ok, "expected_batches should be present as an array: %s", string(data))
	assert.Empty(expectedBatches, "expected_batches")
}
