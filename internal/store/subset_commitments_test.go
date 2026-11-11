package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopySubsetCommitmentDecisionsRequireProfilesAndCopiedAnchor(t *testing.T) {
	for _, profiles := range []bool{false, true} {
		t.Run(map[bool]string{false: "without profiles", true: "with profiles"}[profiles], func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			path := createTestSourceDB(t, t.TempDir(), 5)
			source, err := Open(path)
			r.NoError(err)
			person, _, err := source.CreatePersonFromParticipant(2)
			r.NoError(err)
			var first, last int64
			r.NoError(source.db.QueryRowContext(ctx, `SELECT MIN(id), MAX(id) FROM messages`).Scan(&first, &last))
			for _, anchor := range []int64{first, last} {
				_, err := source.db.ExecContext(ctx, `INSERT INTO person_commitments
					(person_id, target_key, anchor_message_id, direction, quote, quote_start, quote_end, summary, promised_at, expires_at, owner_state)
					VALUES (?, ?, ?, 'owed_to', 'I will send it.', 0, 15, 'Send it', ?, ?, 'done')`,
					person.ID, "subset-promise-"+strconv.FormatInt(anchor, 10), anchor, personFactLedgerNow, personFactLedgerNow.Add(24*time.Hour))
				r.NoError(err)
			}
			r.NoError(source.Close())
			destination := filepath.Join(t.TempDir(), "subset")
			_, err = CopySubsetWithOptions(ctx, path, destination, 1, CopySubsetOptions{IncludeProfiles: profiles})
			r.NoError(err)
			copied, err := Open(filepath.Join(destination, "msgvault.db"))
			r.NoError(err)
			t.Cleanup(func() { r.NoError(copied.Close()) })
			rows, err := copied.db.QueryContext(ctx, `SELECT owner_state, current_claim_id, EXISTS(SELECT 1 FROM messages WHERE id = anchor_message_id) FROM person_commitments`)
			r.NoError(err)
			defer func() { r.NoError(rows.Close()) }()
			count := 0
			for rows.Next() {
				var state string
				var claim sql.NullInt64
				var exists bool
				r.NoError(rows.Scan(&state, &claim, &exists))
				a.Equal(LoopStateDone, state)
				a.False(claim.Valid)
				a.True(exists)
				count++
			}
			r.NoError(rows.Err())
			if profiles {
				a.Equal(1, count)
			} else {
				a.Zero(count)
			}
		})
	}
}
