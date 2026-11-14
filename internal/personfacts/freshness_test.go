package personfacts

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimReadMetadataDoesNotChangePacketJSON(t *testing.T) {
	claim := Claim{ID: 1}
	before, err := json.Marshal(claim)
	require.NoError(t, err)
	claim.Freshness = Freshness{Kind: KindFact, Status: StatusFresh}
	after, err := json.Marshal(claim)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}
