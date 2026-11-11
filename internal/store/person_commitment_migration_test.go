package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

// The same production upgrade runs on SQLite and the PostgreSQL CI backend.
// Restore the actual v40 ledger vocabularies and missing slice tables/column,
// retaining an existing attempt, claim and evidence link across initialization.
func TestCommitmentMigrationUpgradesVersion40(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepBudgetFixture(t, "commitment-v40")
	st := f.store
	seedPersonFactClaimLedger(t, st, f.personID, "before-commitment")
	installPersonFactClaimOrigin(t, st, `CHECK (origin IN ('extraction', 'enrichment', 'brief', 'system', 'invalid'))`)
	installPersonSweepBatchPurpose(t, st, `CHECK ((call_ordinal = 0 AND purpose IN ('primary', 'brief')) OR (call_ordinal = 1 AND purpose IN ('repair', 'brief_repair')))`)
	for _, query := range []string{
		`DROP TABLE person_commitment_agenda`,
		`DROP TABLE person_commitment_frontiers`,
		`DROP TABLE person_commitments`,
		`ALTER TABLE person_sweep_attempts DROP COLUMN commitment_failure_class`,
		`DELETE FROM applied_migrations WHERE name = 'person_commitments_v1'`,
		`UPDATE archive_metadata SET value = '40' WHERE key = 'schema_version'`,
	} {
		_, err := st.DB().ExecContext(ctx, query)
		r.NoError(err)
	}
	version, err := st.SchemaVersionContext(ctx)
	r.NoError(err)
	a.Equal(40, version)
	for range 2 {
		r.NoError(st.InitSchemaContext(ctx))
		version, err = st.SchemaVersionContext(ctx)
		r.NoError(err)
		a.Equal(store.SchemaVersion, version)
		var claims, links, attempts int
		r.NoError(st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_fact_claims`).Scan(&claims))
		r.NoError(st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_fact_claim_evidence`).Scan(&links))
		r.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM person_sweep_attempts WHERE id = ? AND commitment_failure_class = ''`), f.attemptID).Scan(&attempts))
		a.Equal(1, claims)
		a.Equal(1, links)
		a.Equal(1, attempts)
		var rows int
		r.NoError(st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_commitment_frontiers WHERE scope_fingerprint = '' AND selection_at IS NULL`).Scan(&rows))
		a.Zero(rows)
	}
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_fact_claims
 (person_id,generation_id,claim_key,target_kind,target_key,target_revision,relation,submitted_value_json,origin,confidence_json)
 SELECT ?,MIN(id),'upgraded-commitment','commitment','promise','rev-1','support','"Send report"','commitment','{}'
 FROM person_fact_generations WHERE person_id = ?`), f.personID, f.personID)
	r.NoError(err)
	for ordinal, purpose := range []string{peoplesweep.ProviderCallPurposeCommitment, peoplesweep.ProviderCallPurposeCommitmentRepair} {
		request := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
		request.CallOrdinal, request.Purpose = ordinal, purpose
		reservation, reserveErr := st.ReservePersonSweepBudget(ctx, request)
		r.NoError(reserveErr)
		if ordinal == 0 {
			r.NoError(st.MarkPersonSweepBudgetStarted(ctx, reservation, sweepAttemptLease(f)))
		}
	}
	source, err := st.GetOrCreateSource("slack", "commitment-v40")
	r.NoError(err)
	conversation, err := st.EnsureConversationWithType(source.ID, "commitment-v40", "direct_chat", "Synthetic upgrade")
	r.NoError(err)
	anchor, err := st.PersistMessageContext(ctx, &store.MessagePersistData{Message: &store.Message{
		SourceID: source.ID, ConversationID: conversation, MessageType: "slack", SourceMessageID: "commitment-v40-anchor",
	}})
	r.NoError(err)
	var id int64
	r.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`INSERT INTO person_commitments
 (person_id,target_key,anchor_message_id,direction,quote,quote_start,quote_end,summary,promised_at,expires_at)
 VALUES (?, 'promise', ?, 'owed_to', 'I will send it.', 0, 15, 'Send it', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) RETURNING id`), f.personID, anchor).Scan(&id))
	a.Positive(id)
}
