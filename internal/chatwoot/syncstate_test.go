package chatwoot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSyncStateRejectsInvalidCheckpoints(t *testing.T) {
	for name, blob := range map[string]string{
		"malformed":               `{`,
		"version":                 `{"version":2,"conversations":{}}`,
		"null_conversations":      `{"version":1,"conversations":null}`,
		"unknown_walk":            `{"version":1,"conversations":{},"walk":"sideways"}`,
		"negative_page":           `{"version":1,"conversations":{},"next_page":-1}`,
		"negative_activity_page":  `{"version":1,"conversations":{},"activity_next_page":-1}`,
		"settled_after_watermark": `{"version":1,"conversations":{},"activity_settled":5,"activity_watermark":4}`,
		"null_conversation":       `{"version":1,"conversations":{"1":null}}`,
		"artifact_past_max_id":    `{"version":1,"conversations":{"1":{"last_artifact":2147483648}}}`,
		"empty_range":             `{"version":1,"conversations":{"1":{"pending":[{"after":5,"before":5}]}}}`,
		"zero_after":              `{"version":1,"conversations":{"1":{"pending":[{"after":0,"before":5}]}}}`,
		"overlapping_ranges":      `{"version":1,"conversations":{"1":{"pending":[{"after":5,"before":20},{"after":1,"before":10}]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseSyncState(blob)
			assert.Error(t, err)
		})
	}
}

func TestParseSyncStateKeepsPendingRangeOrder(t *testing.T) {
	state, err := parseSyncState(`{"version":1,"conversations":{"1":{"pending":[{"after":5,"before":10},{"after":1,"before":5}]}}}`)
	require.NoError(t, err)
	assert.Equal(t, []idRange{{5, 10}, {1, 5}}, state.Conversations["1"].Pending)
}
