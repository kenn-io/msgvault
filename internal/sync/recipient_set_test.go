package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/mime"
)

// TestBuildRecipientSetKeepsAliasEnvelopeSnapshots covers the dedup key of
// buildRecipientSet: one row per (participant, normalized envelope address),
// not one row per participant. Two aliases resolving to the same merged
// participant must each keep their own envelope snapshot, a case variant of
// an already-seen address must collapse, and the display-name upgrade (an
// empty first-seen name picks up a later non-empty one) must apply to every
// row of that participant.
func TestBuildRecipientSetKeepsAliasEnvelopeSnapshots(t *testing.T) {
	assert := assert.New(t)

	// primary@ and alias@ resolve to the same participant (7), as an
	// already-merged alias pair would; other@ is an unrelated participant.
	participantMap := map[string]int64{
		"primary@example.test": 7,
		"alias@example.test":   7,
		"PRIMARY@example.test": 7,
		"other@example.test":   9,
	}
	addresses := []mime.Address{
		{Email: "primary@example.test"},
		{Email: "alias@example.test", Name: "Prim"},
		{Email: "PRIMARY@example.test", Name: "Prim Upper"},
		{Email: "other@example.test", Name: "Other"},
		{Email: "unknown@example.test", Name: "Skipped"},
	}

	rs := buildRecipientSet("to", addresses, participantMap)

	assert.Equal("to", rs.Type)
	assert.Equal([]int64{7, 7, 9}, rs.ParticipantIDs,
		"aliases of one participant must produce one row each; case variants and unknown addresses must not")
	assert.Equal(
		[]string{"primary@example.test", "alias@example.test", "other@example.test"},
		rs.EmailAddresses,
		"each row must pin the envelope address that produced it")
	assert.Equal([]string{"Prim", "Prim", "Other"}, rs.DisplayNames,
		"a later non-empty display name must upgrade every row of the participant")
}
