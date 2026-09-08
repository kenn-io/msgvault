package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// legacyPersonFactClaimOriginCheck is the claim-origin vocabulary archives
// carried before the person brief became a first-class claim origin.
const legacyPersonFactClaimOriginCheck = `CHECK (origin IN ('extraction', 'enrichment', 'system', 'invalid'))`

// seedPersonFactClaimLedger writes one generation, one evidence row, one
// extraction claim, and the link between them, so a migration runs against an
// archive that already spent extraction calls and has referencing rows the
// rebuild must not destroy.
func seedPersonFactClaimLedger(t *testing.T, st *store.Store, personID int64, claimKey string) {
	t.Helper()
	requirements := require.New(t)
	resolvedAt := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_generations
			(person_id, generation_key, source_cursors_json, program_id, program_version,
			 program_fingerprint, catalog_fingerprint, provider, provider_version, model,
			 model_version, provider_policy_fingerprint, resolved_at)
		VALUES (?, ?, '[]', 'program', 'v1', ?, 'catalog', 'provider', 'pv1', 'model', 'mv1', 'policy', ?)`),
		personID, "generation-"+claimKey, strings.Repeat("a", 64), resolvedAt)
	requirements.NoError(err)

	var generationID int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT id FROM person_fact_generations WHERE person_id = ? AND generation_key = ?`),
		personID, "generation-"+claimKey).Scan(&generationID))

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_evidence
			(person_id, evidence_key, source_class, directness, authority, subject_person_id,
			 event_time, recorded_time, identity_score)
		VALUES (?, ?, 'archive', 'direct-self', 'ordinary', ?, ?, ?, 900)`),
		personID, "evidence-"+claimKey, personID, resolvedAt, resolvedAt)
	requirements.NoError(err)
	var evidenceID int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT id FROM person_fact_evidence WHERE person_id = ? AND evidence_key = ?`),
		personID, "evidence-"+claimKey).Scan(&evidenceID))

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_claims
			(person_id, generation_id, claim_key, target_kind, target_key, target_revision,
			 relation, submitted_value_json, origin, confidence_json)
		VALUES (?, ?, ?, 'attribute', 'city', 'rev-1', 'support', '"Riverton"', 'extraction', '{}')`),
		personID, generationID, claimKey)
	requirements.NoError(err)
	var claimID int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT id FROM person_fact_claims WHERE person_id = ? AND claim_key = ?`),
		personID, claimKey).Scan(&claimID))

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`INSERT INTO person_fact_claim_evidence (claim_id, evidence_id) VALUES (?, ?)`),
		claimID, evidenceID)
	requirements.NoError(err)
}

// installLegacyPersonFactClaimOrigin puts person_fact_claims back into its
// pre-brief shape with its rows intact.
func installLegacyPersonFactClaimOrigin(t *testing.T, st *store.Store) {
	t.Helper()
	requirements := require.New(t)
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(t.Context(), `
			ALTER TABLE person_fact_claims
				DROP CONSTRAINT IF EXISTS person_fact_claims_origin_check;
			ALTER TABLE person_fact_claims
				ADD CONSTRAINT person_fact_claims_origin_check `+legacyPersonFactClaimOriginCheck)
		requirements.NoError(err)
		return
	}
	conn, err := st.DB().Conn(t.Context())
	requirements.NoError(err)
	defer func() { requirements.NoError(conn.Close()) }()
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys = OFF`)
	requirements.NoError(err)
	_, err = conn.ExecContext(t.Context(), `
		CREATE TABLE person_fact_claims_legacy (
			id                    INTEGER PRIMARY KEY,
			person_id             INTEGER NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
			generation_id         INTEGER NOT NULL REFERENCES person_fact_generations(id) ON DELETE CASCADE,
			claim_key             TEXT NOT NULL,
			target_kind           TEXT NOT NULL,
			target_key            TEXT NOT NULL,
			target_revision       TEXT NOT NULL,
			relation              TEXT NOT NULL CHECK (relation IN ('support', 'contradict', 'supersede', 'invalid')),
			submitted_value_json  TEXT NOT NULL,
			normalized_value_json JSON,
			value_fingerprint     TEXT,
			valid_from            DATETIME,
			valid_until           DATETIME,
			origin                TEXT NOT NULL `+legacyPersonFactClaimOriginCheck+`,
			confidence_json       JSON NOT NULL,
			rejection_action      TEXT,
			rejection_reason      TEXT,
			rejection_detail      TEXT,
			created_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(person_id, claim_key)
		);
		INSERT INTO person_fact_claims_legacy (
			id, person_id, generation_id, claim_key, target_kind, target_key, target_revision,
			relation, submitted_value_json, normalized_value_json, value_fingerprint,
			valid_from, valid_until, origin, confidence_json, rejection_action,
			rejection_reason, rejection_detail, created_at)
		SELECT id, person_id, generation_id, claim_key, target_kind, target_key, target_revision,
			relation, submitted_value_json, normalized_value_json, value_fingerprint,
			valid_from, valid_until, origin, confidence_json, rejection_action,
			rejection_reason, rejection_detail, created_at
		FROM person_fact_claims;
		DROP TABLE person_fact_claims;
		ALTER TABLE person_fact_claims_legacy RENAME TO person_fact_claims`)
	requirements.NoError(err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys = ON`)
	requirements.NoError(err)
}

func insertBriefOriginClaim(
	ctx context.Context, st *store.Store, personID int64, claimKey string,
) error {
	var generationID int64
	if err := st.DB().QueryRowContext(ctx, st.Rebind(
		`SELECT MIN(id) FROM person_fact_generations WHERE person_id = ?`),
		personID).Scan(&generationID); err != nil {
		return err
	}
	_, err := st.DB().ExecContext(ctx, st.Rebind(`
		INSERT INTO person_fact_claims
			(person_id, generation_id, claim_key, target_kind, target_key, target_revision,
			 relation, submitted_value_json, origin, confidence_json)
		VALUES (?, ?, ?, 'attribute', 'city', 'rev-1', 'support', '"Riverton"', 'brief', '{}')`),
		personID, generationID, claimKey)
	return err
}

func TestPersonFactClaimOriginMigrationKeepsExtractionLedgerAndAdmitsBriefClaims(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newPersonSweepBudgetFixture(t, "claim-origin-migration")
	seedPersonFactClaimLedger(t, f.store, f.personID, "extraction-claim")

	installLegacyPersonFactClaimOrigin(t, f.store)
	requirements.Error(insertBriefOriginClaim(t.Context(), f.store, f.personID, "brief-before"),
		"the legacy constraint must reject a brief-origin claim")

	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "person_fact_claim_origin_brief_v1")
	requirements.NoError(err)
	requirements.NoError(f.store.InitSchema())

	var claims, links, extraction int
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*), SUM(CASE WHEN origin = 'extraction' THEN 1 ELSE 0 END)
		FROM person_fact_claims WHERE person_id = ?`), f.personID).Scan(&claims, &extraction))
	checks.Equal(1, claims, "the rebuild must preserve every claim row")
	checks.Equal(1, extraction)
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM person_fact_claim_evidence`).Scan(&links))
	checks.Equal(1, links, "the rebuild must not cascade away the claim's evidence links")

	requirements.NoError(
		insertBriefOriginClaim(t.Context(), f.store, f.personID, "brief-after"),
		"a brief-origin claim must insert on an archive that already has extraction claims")

	var rejected sql.NullString
	err = f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT origin FROM person_fact_claims WHERE person_id = ? AND claim_key = ?`),
		f.personID, "brief-after").Scan(&rejected)
	requirements.NoError(err)
	checks.Equal("brief", rejected.String)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "person_fact_claim_origin_brief_v1")
	requirements.NoError(err)
	requirements.NoError(f.store.InitSchema(), "the migration must be idempotent")
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_fact_claims WHERE person_id = ?`), f.personID).Scan(&claims))
	checks.Equal(2, claims)

	// The rebuild ran on a pooled connection with foreign keys suspended; every
	// later connection must still enforce them.
	_, err = f.store.DB().ExecContext(t.Context(), st1000ClaimEvidenceInsert(f.store))
	requirements.Error(err, "foreign key enforcement must be restored after the rebuild")
}

// st1000ClaimEvidenceInsert names a claim that cannot exist, so the statement
// only succeeds when foreign keys are not being enforced.
func st1000ClaimEvidenceInsert(st *store.Store) string {
	return st.Rebind(
		`INSERT INTO person_fact_claim_evidence (claim_id, evidence_id) VALUES (1000000, 1000001)`)
}

func TestPersonSweepAttemptBriefFailureMigrationAddsColumnToExistingArchives(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newPersonSweepBudgetFixture(t, "attempt-brief-failure-migration")

	_, err := f.store.DB().ExecContext(t.Context(),
		`ALTER TABLE person_sweep_attempts DROP COLUMN brief_failure_class`)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "person_sweep_attempt_brief_failure_v1")
	requirements.NoError(err)
	requirements.NoError(f.store.InitSchema())

	var attempts int
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM person_sweep_attempts WHERE id = ? AND brief_failure_class = ''`),
		f.attemptID).Scan(&attempts))
	checks.Equal(1, attempts, "an existing attempt gains the column with the empty default")

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_sweep_attempts SET brief_failure_class = 'not-a-class' WHERE id = ?`),
		f.attemptID)
	requirements.Error(err, "the added column keeps the failure-class vocabulary closed")

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "person_sweep_attempt_brief_failure_v1")
	requirements.NoError(err)
	requirements.NoError(f.store.InitSchema(), "the migration must be idempotent")
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM person_sweep_attempts WHERE id = ?`), f.attemptID).Scan(&attempts))
	checks.Equal(1, attempts)
}
