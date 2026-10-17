package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func tierManualPerson(t *testing.T) (*store.Store, *store.Person) {
	t.Helper()
	fixture := storetest.New(t)
	participant := fixture.EnsureParticipant("manual-tiers@example.test", "Manual Tier", "example.test")
	person, _, err := fixture.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	return fixture.Store, person
}

// A manual untrack must remove its dependent brief and persist the owner's
// negative decisions even when no tier automation has run.
func TestPersonTierManualUntrackRemovesBriefAndRecordsOptOuts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, person := tierManualPerson(t)
	_, err := st.SetPersonBriefEnrollmentContext(t.Context(), person.ID, true, "owner", true)
	require.NoError(err)
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, false)
	require.NoError(err)
	brief, err := st.GetPersonBriefEnrollmentContext(t.Context(), person.ID)
	require.NoError(err)
	assert.False(brief.Enrolled)
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`SELECT lane, actor FROM person_tier_opt_outs WHERE person_id = ? ORDER BY lane`), person.ID)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var lanes []string
	actors := make(map[string]string)
	for rows.Next() {
		var lane, actor string
		require.NoError(rows.Scan(&lane, &actor))
		lanes = append(lanes, lane)
		actors[lane] = actor
	}
	require.NoError(rows.Err())
	assert.Equal([]string{"briefs", "tracking"}, lanes)
	assert.Equal(map[string]string{"briefs": "owner", "tracking": "owner"}, actors)
}

// Upgrading an automatic grant must retain the original enrollment date and
// capture the first human actor, including through a manual brief enrollment.
func TestPersonTierManualBriefUpgradesAutomaticTracking(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, person := tierManualPerson(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking (person_id, tracked_at, provenance, actor) VALUES (?, ?, 'auto', 'signal-tiers')`), person.ID, at)
	require.NoError(err)
	_, err = st.SetPersonBriefEnrollmentContext(t.Context(), person.ID, true, "owner", false)
	require.NoError(err)
	var provenance, actor string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT provenance, actor FROM person_tracking WHERE person_id = ?`), person.ID).Scan(&provenance, &actor))
	assert.Equal("manual", provenance)
	assert.Equal("owner", actor)
	tracking, err := st.GetPersonTrackingContext(t.Context(), person.ID)
	require.NoError(err)
	require.NotNil(tracking.TrackedAt)
	assert.True(at.Equal(*tracking.TrackedAt))
}

func TestPersonTierManualTrackingClearsOnlyTrackingOptOut(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, person := tierManualPerson(t)
	for _, lane := range []string{"tracking", "briefs", "enrichment"} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tier_opt_outs(person_id,lane,actor) VALUES(?,?, 'owner')`), person.ID, lane)
		require.NoError(err)
	}
	_, err := st.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM person_tier_opt_outs WHERE person_id=? AND lane='tracking'`), person.ID).Scan(&count))
	assert.Zero(count)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM person_tier_opt_outs WHERE person_id=?`), person.ID).Scan(&count))
	assert.Equal(2, count)
}

func TestPersonTierManualUnenrollRecordsOptOut(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, person := tierManualPerson(t)
	_, err := st.SetPersonBriefEnrollmentContext(t.Context(), person.ID, false, "owner", false)
	require.NoError(err)
	var actor string
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT actor FROM person_tier_opt_outs WHERE person_id=? AND lane='briefs'`), person.ID).Scan(&actor)
	require.NoError(err)
	assert.Equal("owner", actor)
}

func TestPersonTierManualBriefUpgradePreservesGrantTimeAndFirstManualActor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, person := tierManualPerson(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking(person_id,tracked_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), person.ID, at)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_brief_enrollments(person_id,enabled_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), person.ID, at)
	require.NoError(err)
	first, err := st.SetPersonBriefEnrollmentContext(t.Context(), person.ID, true, "owner", false)
	require.NoError(err)
	assert.Equal("manual", first.Provenance)
	assert.Equal("owner", first.Actor)
	require.NotNil(first.EnabledAt)
	assert.True(at.Equal(*first.EnabledAt))
	again, err := st.SetPersonBriefEnrollmentContext(t.Context(), person.ID, true, "other-owner", false)
	require.NoError(err)
	assert.Equal("manual", again.Provenance)
	assert.Equal("owner", again.Actor)
	require.NotNil(again.EnabledAt)
	assert.True(at.Equal(*again.EnabledAt))
	tracking, err := st.GetPersonTrackingContext(t.Context(), person.ID)
	require.NoError(err)
	assert.Equal("manual", tracking.Provenance)
	assert.Equal("owner", tracking.Actor)
}
