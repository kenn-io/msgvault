package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopySubsetCommitmentDecisionsFollowCopiedPeople(t *testing.T) {
	for _, profiles := range []bool{false, true} {
		t.Run(map[bool]string{false: "without profiles", true: "with profiles"}[profiles], func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			path := createTestSourceDB(t, t.TempDir(), 5)
			source, err := Open(path)
			r.NoError(err)
			included, _, err := source.CreatePersonFromParticipant(2)
			r.NoError(err)
			excluded, _, err := source.CreatePersonFromParticipant(1)
			r.NoError(err)
			var evidenceID int64
			r.NoError(source.db.QueryRowContext(ctx, `INSERT INTO person_fact_evidence
				(person_id,evidence_key,source_class,directness,authority,event_time,recorded_time,identity_score)
				VALUES (?,'subset-evidence','archive','direct-self','ordinary',?,?,900) RETURNING id`, included.ID, personFactLedgerNow, personFactLedgerNow).Scan(&evidenceID))
			until := personFactLedgerNow.Add(24 * time.Hour)
			_, err = source.db.ExecContext(ctx, `INSERT INTO person_commitments
				(id, person_id, target_key, owner_state, snoozed_until, reopened_at, fulfilment_evidence_id, fulfilment_confirmed)
				VALUES (101, ?, 'included', 'snoozed', ?, ?, ?, 1), (102, ?, 'excluded', 'done', NULL, NULL, NULL, 0)`,
				included.ID, until, personFactLedgerNow, evidenceID, excluded.ID)
			r.NoError(err)
			r.NoError(source.Close())

			destination := filepath.Join(t.TempDir(), "subset")
			_, err = CopySubsetWithOptions(ctx, path, destination, 1, CopySubsetOptions{IncludeProfiles: profiles})
			r.NoError(err)
			copied, err := Open(filepath.Join(destination, "msgvault.db"))
			r.NoError(err)
			t.Cleanup(func() { r.NoError(copied.Close()) })
			rows, err := copied.db.QueryContext(ctx, `SELECT id, person_id, target_key, owner_state, snoozed_until, reopened_at, fulfilment_evidence_id, fulfilment_confirmed FROM person_commitments`)
			r.NoError(err)
			defer func() { r.NoError(rows.Close()) }()
			if profiles {
				r.True(rows.Next())
				var id, personID int64
				var target, state string
				var snooze, reopened nullableTimestamp
				var fulfilment sql.NullInt64
				var confirmed bool
				r.NoError(rows.Scan(&id, &personID, &target, &state, &snooze, &reopened, &fulfilment, &confirmed))
				a.Equal(int64(101), id)
				a.Equal(included.ID, personID)
				a.Equal("included", target)
				a.Equal(LoopStateSnoozed, state)
				a.True(snooze.Valid)
				a.True(snooze.Time.Equal(until))
				a.True(reopened.Valid)
				a.True(reopened.Time.Equal(personFactLedgerNow))
				a.False(fulfilment.Valid)
				a.False(confirmed)
			}
			a.False(rows.Next())
			r.NoError(rows.Err())
		})
	}
}
