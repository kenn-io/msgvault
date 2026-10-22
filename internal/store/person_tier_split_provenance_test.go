package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonTierExactSplitKeepsProvenanceOnlyManualReplacement(t *testing.T) {
	st, survivor, absorbed := tierLifecyclePeople(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking(person_id,tracked_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), absorbed.ID, at)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_brief_enrollments(person_id,enabled_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), absorbed.ID, at)
	require.NoError(t, err)
	merged := tierMerge(t, st, survivor, absorbed)
	// Using the same actor makes provenance the only changed grant column.
	_, err = st.SetPersonBriefEnrollmentContext(t.Context(), survivor.ID, true, "signal-tiers", false)
	require.NoError(t, err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "tier-replacement-split", Actor: "owner",
	})
	require.NoError(t, err)
	assert.True(t, split.ExactReversal)
	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		for i, personID := range []int64{split.SourcePerson.ID, split.NewPerson.ID} {
			var provenance string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT provenance FROM `+table+` WHERE person_id=?`), personID).Scan(&provenance))
			expected := "manual"
			if i == 1 {
				expected = "auto"
			}
			assert.Equal(t, expected, provenance)
		}
	}
}
