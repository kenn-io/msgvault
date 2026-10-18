package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMergeParticipantsTransfersAndDeduplicatesLoopSuppressions(t *testing.T) {
	for _, test := range []struct {
		name             string
		survivorIsMuted  bool
		wantSurvivorNote string
	}{
		{name: "transfer absorbed mute", wantSurvivorNote: "absorbed mute"},
		{name: "keep survivor mute", survivorIsMuted: true, wantSurvivorNote: "survivor mute"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx := t.Context()
			f := storetest.New(t)
			st := f.Store
			survivorID, err := st.EnsureParticipant("survivor@example.test", "Survivor", "example.test")
			require.NoError(err)
			absorbedID, err := st.EnsureParticipant("absorbed@example.test", "Absorbed", "example.test")
			require.NoError(err)
			if test.survivorIsMuted {
				_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO loop_suppressions
					(target_kind, participant_id, note) VALUES ('participant', ?, 'survivor mute')`), survivorID)
				require.NoError(err)
			}
			_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO loop_suppressions
				(target_kind, participant_id, note) VALUES ('participant', ?, 'absorbed mute')`), absorbedID)
			require.NoError(err)
			loopID := insertSchemaLoop(t, f, "participant-merge-loop", "reply_owed", "turn_state")
			loopExec(t, st, `UPDATE open_loops SET counterparty_participant_id = ?, summary = ?,
				status = 'closed', closed_at = CURRENT_TIMESTAMP,
				closed_reason = 'done', closed_by = 'user' WHERE id = ?`,
				absorbedID, "preserve loop history", loopID)

			require.NoError(st.MergeParticipantsContext(ctx, absorbedID, survivorID))

			var survivorNote string
			require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT note FROM loop_suppressions
				WHERE participant_id = ?`), survivorID).Scan(&survivorNote))
			assert.Equal(test.wantSurvivorNote, survivorNote)
			var remaining int
			require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM loop_suppressions
				WHERE target_kind = 'participant'`)).Scan(&remaining))
			assert.Equal(1, remaining)
			var loopParticipantID int64
			var loopStatus, summary, closedReason string
			require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT counterparty_participant_id,
				status, summary, closed_reason FROM open_loops WHERE id = ?`), loopID).Scan(
				&loopParticipantID, &loopStatus, &summary, &closedReason))
			assert.Equal(survivorID, loopParticipantID)
			assert.Equal("closed", loopStatus)
			assert.Equal("preserve loop history", summary)
			assert.Equal("done", closedReason)
		})
	}
}

func TestMergePersonsTransfersAndRestoresLoopSuppressions(t *testing.T) {
	for _, test := range []struct {
		name            string
		survivorIsMuted bool
	}{
		{name: "transfer absorbed mute"},
		{name: "deduplicate and restore both mutes", survivorIsMuted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx := t.Context()
			f := storetest.New(t)
			st := f.Store
			survivorParticipantID, err := st.EnsureParticipant(
				"person-survivor@example.test", "Survivor", "example.test")
			require.NoError(err)
			absorbedParticipantID, err := st.EnsureParticipant(
				"person-absorbed@example.test", "Absorbed", "example.test")
			require.NoError(err)
			survivor, _, err := st.CreatePersonFromParticipant(survivorParticipantID)
			require.NoError(err)
			absorbed, _, err := st.CreatePersonFromParticipant(absorbedParticipantID)
			require.NoError(err)
			if test.survivorIsMuted {
				_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO loop_suppressions
					(target_kind, person_id, note) VALUES ('person', ?, 'survivor mute')`), survivor.ID)
				require.NoError(err)
			}
			_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO loop_suppressions
				(target_kind, person_id, note) VALUES ('person', ?, 'absorbed mute')`), absorbed.ID)
			require.NoError(err)
			loopID := insertSchemaLoop(t, f, "person-merge-loop", "reply_owed", "turn_state")
			loopExec(t, st, `UPDATE open_loops SET counterparty_person_id = ?, summary = ?,
				status = 'closed', closed_at = CURRENT_TIMESTAMP,
				closed_reason = 'done', closed_by = 'user' WHERE id = ?`,
				absorbed.ID, "preserve loop history", loopID)

			merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{
				SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
				ExpectedSurvivorRevision: survivor.Revision,
				ExpectedAbsorbedRevision: absorbed.Revision,
				IdempotencyKey:           "merge-loop-suppressions", Actor: "test",
			})
			require.NoError(err)
			var mergedLoopPersonID int64
			var loopStatus, summary, closedReason string
			require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT counterparty_person_id,
				status, summary, closed_reason FROM open_loops WHERE id = ?`), loopID).Scan(
				&mergedLoopPersonID, &loopStatus, &summary, &closedReason))
			assert.Equal(merged.Person.ID, mergedLoopPersonID)
			assert.Equal("closed", loopStatus)
			assert.Equal("preserve loop history", summary)
			assert.Equal("done", closedReason)
			var mergedNote string
			require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT note FROM loop_suppressions
				WHERE person_id = ?`), merged.Person.ID).Scan(&mergedNote))
			wantMergedNote := "absorbed mute"
			if test.survivorIsMuted {
				wantMergedNote = "survivor mute"
			}
			assert.Equal(wantMergedNote, mergedNote)

			split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
				SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
				ParticipantIDs:         []int64{absorbedParticipantID},
				ExpectedSourceRevision: merged.Person.Revision,
				IdempotencyKey:         "split-loop-suppressions", Actor: "test",
			})
			require.NoError(err)
			assert.True(split.ExactReversal)
			assert.Empty(split.UnrestoredRows)
			var splitLoopPersonID int64
			require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT counterparty_person_id,
				status, summary, closed_reason FROM open_loops WHERE id = ?`), loopID).Scan(
				&splitLoopPersonID, &loopStatus, &summary, &closedReason))
			assert.Equal(split.NewPerson.ID, splitLoopPersonID)
			assert.Equal("closed", loopStatus)
			assert.Equal("preserve loop history", summary)
			assert.Equal("done", closedReason)

			rows, err := st.DB().QueryContext(ctx, st.Rebind(`SELECT person_id, note FROM loop_suppressions
				WHERE target_kind = 'person' ORDER BY person_id`))
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			got := make(map[int64]string)
			for rows.Next() {
				var personID int64
				var note string
				require.NoError(rows.Scan(&personID, &note))
				got[personID] = note
			}
			require.NoError(rows.Err())
			want := map[int64]string{split.NewPerson.ID: "absorbed mute"}
			if test.survivorIsMuted {
				want[merged.Person.ID] = "survivor mute"
			}
			assert.Equal(want, got)
		})
	}
}
