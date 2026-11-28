package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// After a merge deletes a participant, every Slack ID cached on it resolves
// to the surviving participant, so later writes never name the deleted row.
func TestResolverRepointsCachedIDsAfterMerge(t *testing.T) {
	t.Parallel()
	r := newParticipantResolver(nil, "T01")
	r.rich["U1"] = 7
	r.fallback["U2"] = 7
	r.fallback["bot:B1"] = 9

	r.repoint(7, 11)

	assert.Equal(t, int64(11), r.rich["U1"])
	assert.Equal(t, int64(11), r.fallback["U2"])
	assert.Equal(t, int64(9), r.fallback["bot:B1"], "IDs on other participants stay")
}
