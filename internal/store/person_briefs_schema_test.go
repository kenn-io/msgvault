package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonBriefSchemaParity(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	requirements.NoError(st.InitSchemaContext(t.Context()))

	for _, table := range []string{
		"person_brief_enrollments", "person_briefs", "person_brief_evidence",
	} {
		assertPersonFactTableQueryable(t, st, table)
	}

	personID := personFactSchemaPerson(t, st, "brief-schema@example.com")
	generationID := insertPersonFactSchemaGeneration(t, st, personID, "brief-schema")
	evidenceID := insertPersonFactSchemaEvidence(t, st, personID, "brief-schema-evidence")

	var count int
	indexQuery := `SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'person_briefs' AND name = ?`
	if st.IsPostgreSQL() {
		indexQuery = `SELECT COUNT(*) FROM pg_indexes
			WHERE schemaname = current_schema() AND tablename = 'person_briefs' AND indexname = $1`
	}
	requirements.NoError(st.DB().QueryRowContext(t.Context(), indexQuery,
		"idx_person_briefs_current").Scan(&count))
	checks.Equal(1, count, "both backends need the one-current partial unique index")

	first := insertPersonBriefSchemaVersion(t, st, personID, generationID, 1, "current")
	_, err := insertPersonBriefSchemaVersionErr(t, st, personID, generationID, 2, "current")
	checks.Error(err, "a second current version must violate the partial unique index") //nolint:testifylint // Independent schema checks stay nonfatal.
	_, err = insertPersonBriefSchemaVersionErr(t, st, personID, generationID, 1, "superseded")
	checks.Error(err, "(person_id, version) must be unique") //nolint:testifylint // Independent schema checks stay nonfatal.
	_, err = insertPersonBriefSchemaVersionErr(t, st, personID, generationID, 3, "withdrawn")
	checks.Error(err, "the status vocabulary must be closed") //nolint:testifylint // Independent schema checks stay nonfatal.
	_, err = insertPersonBriefSchemaVersionErr(t, st, personID, generationID, 0, "superseded")
	checks.Error(err, "versions start at 1") //nolint:testifylint // Independent schema checks stay nonfatal.

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_brief_evidence (brief_id, evidence_id, ordinal)
		VALUES (?, ?, 0)`), first, evidenceID)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_brief_evidence (brief_id, evidence_id, ordinal)
		VALUES (?, ?, 0)`), first, evidenceID)
	checks.Error(err, "one ordinal per version") //nolint:testifylint // Independent schema checks stay nonfatal.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_brief_evidence (brief_id, evidence_id, ordinal)
		VALUES (?, ?, -1)`), first, evidenceID)
	checks.Error(err, "ordinals must not be negative") //nolint:testifylint // Independent schema checks stay nonfatal.

	assertForeignKeyTarget(t, st, "person_brief_enrollments", "person_id", "persons")
	assertForeignKeyTarget(t, st, "person_briefs", "generation_id", "person_fact_generations")
	assertForeignKeyTarget(t, st, "person_brief_evidence", "evidence_id", "person_fact_evidence")
}

func insertPersonBriefSchemaVersion(
	t *testing.T, st *store.Store, personID, generationID int64, version int, status string,
) int64 {
	t.Helper()
	id, err := insertPersonBriefSchemaVersionErr(t, st, personID, generationID, version, status)
	require.NoError(t, err)
	return id
}

func insertPersonBriefSchemaVersionErr(
	t *testing.T, st *store.Store, personID, generationID int64, version int, status string,
) (int64, error) {
	t.Helper()
	var id int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_briefs
			(person_id, version, generation_id, status, program_id, program_version,
			 program_fingerprint, provider, provider_version, model, model_version,
			 provider_policy_fingerprint, boundary_json, structured_json, rendered_text,
			 renderer_policy, generated_at)
		VALUES (?, ?, ?, ?, 'msgvault-person-brief', 'v1', ?, 'provider', 'v1',
		        'model', 'v1', 'policy', '{}', '{}', 'text', 'person-brief-render-v1',
		        CURRENT_TIMESTAMP)
		RETURNING id`), personID, version, generationID, status,
		strings.Repeat("b", 64)).Scan(&id)
	return id, err
}
