package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestParticipantsByIdentifiersReloadsMergedOwnersAcrossChunks(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	old, err := st.EnsureParticipantByIdentifierContext(t.Context(), "beeper", "old", "Old")
	require.NoError(err)
	winner, err := st.EnsureParticipantByPhone(t.Context(), "+12025550123", "Winner", "beeper")
	require.NoError(err)
	require.NoError(st.MergeParticipantsContext(t.Context(), old, winner))
	require.NoError(st.SetParticipantIdentifierContext(t.Context(), winner, "beeper", "last"))
	keys := make([]string, 301)
	for i := range keys {
		keys[i] = fmt.Sprintf("missing-%d", i)
	}
	keys[0], keys[300] = "old", "last"
	bindings, err := st.ParticipantsByIdentifiersContext(t.Context(), "beeper", keys)
	require.NoError(err)
	require.Len(bindings, 2)
	assert.Equal(winner, bindings["old"].ID)
	assert.True(bindings["old"].HasPhone)
	assert.Equal(winner, bindings["last"].ID)
}
