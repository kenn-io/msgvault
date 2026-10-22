package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func tierLifecyclePeople(t *testing.T) (*store.Store, *store.Person, *store.Person) {
	t.Helper()
	f := storetest.New(t)
	var people []*store.Person
	for _, name := range []string{"survivor", "absorbed"} {
		p := f.EnsureParticipant(name+"-tier@example.test", name, "example.test")
		person, _, err := f.Store.CreatePersonFromParticipant(p)
		require.NoError(t, err)
		people = append(people, person)
	}
	return f.Store, people[0], people[1]
}

func tierMerge(t *testing.T, st *store.Store, survivor, absorbed *store.Person) *store.PersonMergeResult {
	t.Helper()
	merged, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "tier-merge", Actor: "owner",
	})
	require.NoError(t, err)
	return merged
}

// Dropping an absorbed manual grant during deduplication would let the next
// automated demotion erase the owner's explicit enrollment.
func TestPersonTierMergeManualGrantDominatesAndSplitRestores(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, survivor, absorbed := tierLifecyclePeople(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i, person := range []*store.Person{survivor, absorbed} {
		provenance, actor := "auto", "signal-tiers"
		if i == 1 {
			provenance, actor = "manual", "owner"
		}
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking(person_id,tracked_at,provenance,actor) VALUES(?,?,?,?)`), person.ID, at, provenance, actor)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_brief_enrollments(person_id,enabled_at,provenance,actor) VALUES(?,?,?,?)`), person.ID, at, provenance, actor)
		require.NoError(err)
	}
	merged := tierMerge(t, st, survivor, absorbed)
	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		var provenance, actor string
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT provenance,actor FROM `+table+` WHERE person_id=?`), survivor.ID).Scan(&provenance, &actor))
		assert.Equal("manual", provenance)
		assert.Equal("owner", actor)
	}
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs: absorbed.ParticipantIDs, ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey: "tier-split", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)
	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		for i, personID := range []int64{split.SourcePerson.ID, split.NewPerson.ID} {
			var provenance, actor string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT provenance,actor FROM `+table+` WHERE person_id=?`), personID).Scan(&provenance, &actor))
			expectedProvenance, expectedActor := "auto", "signal-tiers"
			if i == 1 {
				expectedProvenance, expectedActor = "manual", "owner"
			}
			assert.Equal(expectedProvenance, provenance)
			assert.Equal(expectedActor, actor)
		}
	}
}

func TestPersonTierMergeMovesAuditHistoryAndKeepsSurvivorState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, survivor, absorbed := tierLifecyclePeople(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, person := range []*store.Person{survivor, absorbed} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tier_state(person_id,tier,tier_since,rule,config_fingerprint,rule_version,enrichment_eligible,lanes_json,as_of_local_date,run_id,run_started_at,applied_at) VALUES(?,1,'2026-10-01','tier1_intensity','config',1,TRUE,'["tracking","briefs","enrichment"]','2026-10-01','run',?,?)`), person.ID, at, at)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tier_observations(person_id,computed_tier,rule,signals_json,shadow_tier,shadow_since,rule_version,config_fingerprint,as_of_local_date,activity_ready,evaluated_at) VALUES(?,1,'tier1_intensity','{}',1,'2026-10-01',1,'config','2026-10-01',TRUE,?)`), person.ID, at)
		require.NoError(err)
	}
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tier_events(person_id,from_tier,to_tier,reason,mode,lanes_json,signals_json,rule,rule_version,actor,run_id) VALUES(?,3,1,'promotion','apply','{}','{}','tier1_intensity',1,'owner','run')`), absorbed.ID)
	require.NoError(err)
	tierMerge(t, st, survivor, absorbed)
	for _, table := range []string{"person_tier_state", "person_tier_observations"} {
		var count int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM `+table+` WHERE person_id=?`), survivor.ID).Scan(&count))
		assert.Equal(1, count)
	}
	var personID, mergedFrom int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT person_id,merged_from_person_id FROM person_tier_events`).Scan(&personID, &mergedFrom))
	assert.Equal(survivor.ID, personID)
	assert.Equal(absorbed.ID, mergedFrom)
}
