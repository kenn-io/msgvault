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
			// Real ledger references must be detached in the subset, whose
			// immutable inference ledger is deliberately absent.
			var generationID, claimID, evidenceID int64
			r.NoError(source.db.QueryRowContext(ctx, `INSERT INTO person_fact_generations
              (person_id,generation_key,source_cursors_json,program_id,program_version,program_fingerprint,catalog_fingerprint,provider,provider_version,model,model_version,provider_policy_fingerprint,resolved_at)
              VALUES (?, 'subset-generation','[]','program','v1','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','catalog','provider','v1','model','v1','policy',?) RETURNING id`, person.ID, personFactLedgerNow).Scan(&generationID))
			r.NoError(source.db.QueryRowContext(ctx, `INSERT INTO person_fact_claims
              (person_id,generation_id,claim_key,target_kind,target_key,target_revision,relation,submitted_value_json,origin,confidence_json)
              VALUES (?,?,'subset-claim','commitment','promise','revision','support','"Send it"','commitment','{}') RETURNING id`, person.ID, generationID).Scan(&claimID))
			r.NoError(source.db.QueryRowContext(ctx, `INSERT INTO person_fact_evidence
              (person_id,evidence_key,source_class,directness,authority,event_time,recorded_time,identity_score)
              VALUES (?,'subset-evidence','archive','direct-self','ordinary',?,?,900) RETURNING id`, person.ID, personFactLedgerNow, personFactLedgerNow).Scan(&evidenceID))
			var first, last int64
			r.NoError(source.db.QueryRowContext(ctx, `SELECT MIN(id), MAX(id) FROM messages`).Scan(&first, &last))
			for _, anchor := range []int64{first, last} {
				_, err := source.db.ExecContext(ctx, `INSERT INTO person_commitments
					(person_id, target_key, anchor_message_id, direction, quote, quote_start, quote_end, summary, promised_at, expires_at, owner_state,current_claim_id,fulfilment_evidence_id)
					VALUES (?, ?, ?, 'owed_to', 'I will send it.', 0, 15, 'Send it', ?, ?, 'done',?,?)`,
					person.ID, "subset-promise-"+strconv.FormatInt(anchor, 10), anchor, personFactLedgerNow, personFactLedgerNow.Add(24*time.Hour), claimID, evidenceID)
				r.NoError(err)
			}
			r.NoError(source.Close())
			destination := filepath.Join(t.TempDir(), "subset")
			_, err = CopySubsetWithOptions(ctx, path, destination, 1, CopySubsetOptions{IncludeProfiles: profiles})
			r.NoError(err)
			copied, err := Open(filepath.Join(destination, "msgvault.db"))
			r.NoError(err)
			t.Cleanup(func() { r.NoError(copied.Close()) })
			rows, err := copied.db.QueryContext(ctx, `SELECT owner_state, current_claim_id, fulfilment_evidence_id, EXISTS(SELECT 1 FROM messages WHERE id = anchor_message_id) FROM person_commitments`)
			r.NoError(err)
			defer func() { r.NoError(rows.Close()) }()
			count := 0
			for rows.Next() {
				var state string
				var claim, fulfilment sql.NullInt64
				var exists bool
				r.NoError(rows.Scan(&state, &claim, &fulfilment, &exists))
				a.Equal(LoopStateDone, state)
				a.False(claim.Valid)
				a.False(fulfilment.Valid)
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
