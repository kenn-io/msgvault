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
			require := require.New(t)
			assert := assert.New(t)
			st, person := tierManualPerson(t)
			at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking(person_id,tracked_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), person.ID, at)
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_brief_enrollments(person_id,enabled_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), person.ID, at)
			require.NoError(err)
			require.NoError(st.SetPersonTierOptOutContext(t.Context(), person.ID, lane, true, "owner", "explicit preference"))
			tracking, err := st.GetPersonTrackingContext(t.Context(), person.ID)
			require.NoError(err)
			assert.Equal(tc.tracked, tracking.Tracked)
			brief, err := st.GetPersonBriefEnrollmentContext(t.Context(), person.ID)
			require.NoError(err)
			assert.Equal(tc.enrolled, brief.Enrolled)
			opts, err := st.ListPersonTierOptOutsContext(t.Context(), person.ID)
			require.NoError(err)
			require.Len(opts, 1)
			assert.Equal(lane, opts[0].Lane)
			assert.Equal("owner", opts[0].Actor)
			assert.Equal("explicit preference", opts[0].Reason)
			require.NoError(st.SetPersonTierOptOutContext(t.Context(), person.ID, lane, false, "owner", ""))
			opts, err = st.ListPersonTierOptOutsContext(t.Context(), person.ID)
			require.NoError(err)
			assert.Empty(opts)
		})
	}
	require := require.New(t)
	assert := assert.New(t)
	st, person := tierManualPerson(t)
	_, err := st.SetPersonBriefEnrollmentContext(t.Context(), person.ID, true, "owner", true)
	require.NoError(err)
	for _, lane := range []string{"tracking", "briefs"} {
		err = st.SetPersonTierOptOutContext(t.Context(), person.ID, lane, true, "owner", "")
		require.ErrorIs(err, store.ErrPersonTierManualGrant)
	}
	require.NoError(st.SetPersonTierOptOutContext(t.Context(), person.ID, "enrichment", true, "owner", ""))
	tracking, err := st.GetPersonTrackingContext(t.Context(), person.ID)
	require.NoError(err)
	assert.True(tracking.Tracked)
}

func TestPersonTierManualTrackPreservesFirstManualActor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, person := tierManualPerson(t)
	first, err := st.SetPersonTrackingActorContext(t.Context(), person.ID, true, "owner")
	require.NoError(err)
	again, err := st.SetPersonTrackingActorContext(t.Context(), person.ID, true, "second-owner")
	require.NoError(err)
	assert.Equal("manual", again.Provenance)
	assert.Equal("owner", again.Actor)
	require.NotNil(first.TrackedAt)
	require.NotNil(again.TrackedAt)
	assert.True(first.TrackedAt.Equal(*again.TrackedAt))
}

func TestPersonTierOptOutRejectsInvalidLaneAndMissingPerson(t *testing.T) {
	st, person := tierManualPerson(t)
	t.Run("invalid lane", func(t *testing.T) {
		require := require.New(t)
		require.Error(st.SetPersonTierOptOutContext(t.Context(), person.ID, "other", true, "owner", ""))
	})
	t.Run("missing person", func(t *testing.T) {
		require := require.New(t)
		require.ErrorIs(st.SetPersonTierOptOutContext(t.Context(), person.ID+9999, "enrichment", true, "owner", ""), store.ErrPersonNotFound)
	})
}
