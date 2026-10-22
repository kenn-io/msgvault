package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonTierOptOutReconcilesAutoRowsAndPreservesManual(t *testing.T) {
	for _, tc := range []struct {
		lane              string
		tracked, enrolled bool
	}{
		{"tracking", false, false}, {"briefs", true, false}, {"enrichment", true, true},
	} {
		lane := tc.lane
		t.Run(lane, func(t *testing.T) {
			st, person := tierManualPerson(t)
			at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking(person_id,tracked_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), person.ID, at)
			require.NoError(t, err)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_brief_enrollments(person_id,enabled_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), person.ID, at)
			require.NoError(t, err)
			require.NoError(t, st.SetPersonTierOptOutContext(t.Context(), person.ID, lane, true, "owner", "explicit preference"))
			tracking, err := st.GetPersonTrackingContext(t.Context(), person.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.tracked, tracking.Tracked)
			brief, err := st.GetPersonBriefEnrollmentContext(t.Context(), person.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.enrolled, brief.Enrolled)
			opts, err := st.ListPersonTierOptOutsContext(t.Context(), person.ID)
			require.NoError(t, err)
			require.Len(t, opts, 1)
			assert.Equal(t, lane, opts[0].Lane)
			assert.Equal(t, "owner", opts[0].Actor)
			assert.Equal(t, "explicit preference", opts[0].Reason)
			require.NoError(t, st.SetPersonTierOptOutContext(t.Context(), person.ID, lane, false, "owner", ""))
			opts, err = st.ListPersonTierOptOutsContext(t.Context(), person.ID)
			require.NoError(t, err)
			assert.Empty(t, opts)
		})
	}
	st, person := tierManualPerson(t)
	_, err := st.SetPersonBriefEnrollmentContext(t.Context(), person.ID, true, "owner", true)
	require.NoError(t, err)
	for _, lane := range []string{"tracking", "briefs"} {
		err = st.SetPersonTierOptOutContext(t.Context(), person.ID, lane, true, "owner", "")
		assert.ErrorIs(t, err, store.ErrPersonTierManualGrant)
	}
	require.NoError(t, st.SetPersonTierOptOutContext(t.Context(), person.ID, "enrichment", true, "owner", ""))
	tracking, err := st.GetPersonTrackingContext(t.Context(), person.ID)
	require.NoError(t, err)
	assert.True(t, tracking.Tracked)
}

func TestPersonTierManualTrackPreservesFirstManualActor(t *testing.T) {
	st, person := tierManualPerson(t)
	first, err := st.SetPersonTrackingActorContext(t.Context(), person.ID, true, "owner")
	require.NoError(t, err)
	again, err := st.SetPersonTrackingActorContext(t.Context(), person.ID, true, "second-owner")
	require.NoError(t, err)
	assert.Equal(t, "manual", again.Provenance)
	assert.Equal(t, "owner", again.Actor)
	require.NotNil(t, first.TrackedAt)
	require.NotNil(t, again.TrackedAt)
	assert.True(t, first.TrackedAt.Equal(*again.TrackedAt))
}

func TestPersonTierOptOutRejectsInvalidLaneAndMissingPerson(t *testing.T) {
	st, person := tierManualPerson(t)
	assert.Error(t, st.SetPersonTierOptOutContext(t.Context(), person.ID, "other", true, "owner", ""))
	assert.ErrorIs(t, st.SetPersonTierOptOutContext(t.Context(), person.ID+9999, "enrichment", true, "owner", ""), store.ErrPersonNotFound)
}
