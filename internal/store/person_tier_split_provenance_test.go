package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonTierExactSplitKeepsProvenanceOnlyManualReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, survivor, absorbed := tierLifecyclePeople(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking(person_id,tracked_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), absorbed.ID, at)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_brief_enrollments(person_id,enabled_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), absorbed.ID, at)
	require.NoError(err)
	merged := tierMerge(t, st, survivor, absorbed)
	// Using the same actor makes provenance the only changed grant column.
	_, err = st.SetPersonBriefEnrollmentContext(t.Context(), survivor.ID, true, "signal-tiers", false)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "tier-replacement-split", Actor: "owner",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		for i, personID := range []int64{split.SourcePerson.ID, split.NewPerson.ID} {
			var provenance string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT provenance FROM `+table+` WHERE person_id=?`), personID).Scan(&provenance))
			expected := "manual"
			if i == 1 {
				expected = "auto"
			}
			assert.Equal(expected, provenance)
		}
	}
}

func TestPersonTierExactSplitTreatsLegacyMergeDefaultsAsUnchanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, survivor, absorbed := tierLifecyclePeople(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking(person_id,tracked_at) VALUES(?,?)`), absorbed.ID, at)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_brief_enrollments(person_id,enabled_at,actor) VALUES(?,?,'legacy-operator')`), absorbed.ID, at)
	require.NoError(err)

	merged := tierMerge(t, st, survivor, absorbed)
	removeLegacyPersonMergeSnapshotColumns(t, st, merged.Merge.ID, "person_tracking", "provenance", "actor")
	removeLegacyPersonMergeSnapshotColumns(t, st, merged.Merge.ID, "person_brief_enrollments", "provenance")

	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "tier-legacy-snapshot-split", Actor: "owner",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		for i, personID := range []int64{split.SourcePerson.ID, split.NewPerson.ID} {
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM `+table+` WHERE person_id=?`), personID).Scan(&count))
			want := 0
			if i == 1 {
				want = 1
			}
			assert.Equal(want, count, table)
		}
	}
	var trackingProvenance, trackingActor, briefProvenance, briefActor string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT provenance,actor FROM person_tracking WHERE person_id=?`), split.NewPerson.ID).Scan(&trackingProvenance, &trackingActor))
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT provenance,actor FROM person_brief_enrollments WHERE person_id=?`), split.NewPerson.ID).Scan(&briefProvenance, &briefActor))
	assert.Equal("manual", trackingProvenance)
	assert.Empty(trackingActor)
	assert.Equal("manual", briefProvenance)
	assert.Equal("legacy-operator", briefActor)
}

func removeLegacyPersonMergeSnapshotColumns(
	t *testing.T, st *store.Store, mergeID int64, table string, removed ...string,
) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	var encoded string
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT post_merge_row_json FROM person_merge_rows
		WHERE merge_id=? AND table_name=? AND post_merge_row_json IS NOT NULL`), mergeID, table).Scan(&encoded)
	require.NoError(err)
	var snapshot map[string]json.RawMessage
	require.NoError(json.Unmarshal([]byte(encoded), &snapshot))
	var columns []json.RawMessage
	require.NoError(json.Unmarshal(snapshot["columns"], &columns))
	kept := make([]json.RawMessage, 0, len(columns))
	for _, column := range columns {
		var named struct {
			Name string `json:"name"`
		}
		require.NoError(json.Unmarshal(column, &named))
		remove := false
		for _, name := range removed {
			if named.Name == name {
				remove = true
				break
			}
		}
		if !remove {
			kept = append(kept, column)
		}
	}
	snapshot["columns"], err = json.Marshal(kept)
	require.NoError(err)
	updated, err := json.Marshal(snapshot)
	require.NoError(err)
	result, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_merge_rows
		SET post_merge_row_json=? WHERE merge_id=? AND table_name=?`), string(updated), mergeID, table)
	require.NoError(err)
	count, err := result.RowsAffected()
	require.NoError(err)
	assert.Equal(int64(1), count)
}
