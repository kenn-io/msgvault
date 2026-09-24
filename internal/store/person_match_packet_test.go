package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonMatchPairSummariesContainOnlyTwoBoundedEndpoints(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "pair-left", "Pair Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "pair-right", "Pair Right")
	require.NoError(err)
	summaries, err := st.PersonMatchPairSummariesContext(t.Context(), left, right)
	require.NoError(err)
	assert.Equal("Pair Left", summaries.Left.DisplayName)
	assert.Equal("Pair Right", summaries.Right.DisplayName)
	assert.Equal("participant", summaries.Left.Kind)
	assert.Len(summaries.Left.Identifiers, 1)
	assert.Equal("pair-left", summaries.Left.Identifiers[0].Value)
}

func TestPersonMatchPairSummariesKeepIdentifiersFromBothEndpoints(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "many-identifiers-left", "Many Identifiers Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "few-identifiers-right", "Few Identifiers Right")
	require.NoError(err)
	for i := range 65 {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_identifiers
			(participant_id, identifier_type, identifier_value, display_value, is_primary)
			VALUES (?, 'email', ?, ?, FALSE)`), left, fmt.Sprintf("left-%02d@example.test", i), fmt.Sprintf("left-%02d@example.test", i))
		require.NoError(err)
	}

	summaries, err := st.PersonMatchPairSummariesContext(t.Context(), left, right)
	require.NoError(err)
	assert.Len(summaries.Left.Identifiers, 8)
	require.Len(summaries.Right.Identifiers, 1)
	assert.Equal("few-identifiers-right", summaries.Right.Identifiers[0].Value)
	assert.True(summaries.Truncated)
}
