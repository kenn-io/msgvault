package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonFactSchemaInitializesTwice(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	require.NoError(st.InitSchemaContext(t.Context()))

	for _, table := range []string{
		"person_fact_evidence", "person_fact_generations", "person_fact_claims",
		"person_fact_claim_evidence", "person_fact_evidence_status_events",
		"person_fact_resolutions", "person_fact_decisions", "person_fact_pin_events",
	} {
		assertPersonFactTableQueryable(t, st, table)
	}
}

func assertPersonFactTableQueryable(t *testing.T, st *store.Store, table string) {
	t.Helper()
	assertions := assert.New(t)
	requirements := require.New(t)

	rows, err := st.DB().QueryContext(t.Context(), `SELECT * FROM `+table+` WHERE 1 = 0`)
	requirements.NoError(err, table)
	defer func() {
		requirements.NoError(rows.Close(), table)
	}()
	columns, err := rows.Columns()
	requirements.NoError(err, table)
	requirements.NoError(rows.Err(), table)
	assertions.NotEmpty(columns, table)
}

func TestPersonFactSchemaHasScopedUniqueKeys(t *testing.T) {
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	firstPerson := personFactSchemaPerson(t, st, "first@example.com")
	secondPerson := personFactSchemaPerson(t, st, "second@example.com")

	firstGeneration := insertPersonFactSchemaGeneration(t, st, firstPerson, "shared-generation")
	secondGeneration := insertPersonFactSchemaGeneration(t, st, secondPerson, "shared-generation")
	assert.NotEqual(firstGeneration, secondGeneration)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_generations
			(person_id, generation_key, source_cursors_json, program_id, program_version,
			 program_fingerprint, catalog_fingerprint, provider, provider_version,
			 model, model_version, provider_policy_fingerprint, resolved_at)
		VALUES (?, ?, '[]', 'program', 'v1', ?, 'catalog', 'provider', 'v1',
		        'model', 'v1', 'policy', CURRENT_TIMESTAMP)`),
		firstPerson, "shared-generation", strings.Repeat("a", 64))
	assert.Error(err) //nolint:testifylint // Independent uniqueness checks must remain nonfatal.

	firstEvidence := insertPersonFactSchemaEvidence(t, st, firstPerson, "shared-evidence")
	secondEvidence := insertPersonFactSchemaEvidence(t, st, secondPerson, "shared-evidence")
	assert.NotEqual(firstEvidence, secondEvidence)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_evidence
			(person_id, evidence_key, source_class, directness, authority,
			 source_url, subject_person_id, subject_ref, excerpt, source_version,
			 event_time, recorded_time, identity_score)
		VALUES (?, ?, 'public', 'direct-other', 'ordinary', 'https://example.com/evidence',
		        ?, 'subject', 'excerpt', 'source-v1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 900)`),
		firstPerson, "shared-evidence", firstPerson)
	assert.Error(err) //nolint:testifylint // Independent uniqueness checks must remain nonfatal.

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_evidence_status_events
			(person_id, generation_id, evidence_id, evidence_key, source_version, supported, reason)
		VALUES (?, ?, ?, 'shared-evidence', 'source-v1', FALSE, 'source-edited')`),
		firstPerson, firstGeneration, firstEvidence)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_evidence_status_events
			(person_id, generation_id, evidence_id, evidence_key, source_version, supported, reason)
		VALUES (?, ?, ?, 'shared-evidence', 'source-v1', FALSE, 'source-deleted')`),
		firstPerson, firstGeneration, firstEvidence)
	assert.Error(err) //nolint:testifylint // Independent uniqueness checks must remain nonfatal.

	firstOtherGeneration := insertPersonFactSchemaGeneration(t, st, firstPerson, "other-generation")
	firstClaim := insertPersonFactSchemaClaim(t, st, firstPerson, firstGeneration, "shared-claim")
	secondClaim := insertPersonFactSchemaClaim(t, st, secondPerson, secondGeneration, "shared-claim")
	assert.NotEqual(firstClaim, secondClaim)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_claims
			(person_id, generation_id, claim_key, target_kind, target_key, target_revision,
			 relation, submitted_value_json, origin, confidence_json)
		VALUES (?, ?, 'shared-claim', 'attribute', 'favorite', 'revision-2',
		        'support', '"value"', 'extraction', '{}')`),
		firstPerson, firstOtherGeneration)
	assert.Error(err, //nolint:testifylint // Independent uniqueness checks must remain nonfatal.
		"claim keys are scoped to a person, not a generation")

	firstResolution := insertPersonFactSchemaResolution(t, st, firstPerson, firstGeneration)
	firstOtherResolution := insertPersonFactSchemaResolution(t, st, firstPerson, firstOtherGeneration)
	secondResolution := insertPersonFactSchemaResolution(t, st, secondPerson, secondGeneration)
	assert.NotEqual(firstResolution, firstOtherResolution)
	assert.NotEqual(firstResolution, secondResolution)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_resolutions
			(person_id, generation_id, target_kind, target_key, target_revision,
			 resolver_version, input_fingerprint, provider_policy_fingerprint, resolved_at)
		VALUES (?, ?, 'attribute', 'favorite', 'revision-2', 'resolver-v1',
		        'input-v1', 'policy-v1', CURRENT_TIMESTAMP)`),
		firstPerson, firstGeneration)
	assert.Error(err, //nolint:testifylint // Independent uniqueness checks must remain nonfatal.
		"resolution identity is scoped to a generation")

	firstDecision := insertPersonFactSchemaDecision(t, st, firstPerson, firstResolution,
		firstClaim, "shared-decision")
	secondDecision := insertPersonFactSchemaDecision(t, st, secondPerson, secondResolution,
		secondClaim, "shared-decision")
	assert.NotEqual(firstDecision, secondDecision)
	firstOtherClaim := insertPersonFactSchemaClaim(t, st, firstPerson, firstOtherGeneration, "other-claim")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_decisions
			(person_id, resolution_id, claim_id, decision_key, action, reason, score_json)
		VALUES (?, ?, ?, 'shared-decision', 'retained', 'below-threshold', '{}')`),
		firstPerson, firstOtherResolution, firstOtherClaim)
	assert.Error(err, "decision keys are scoped to a person, not a resolution")
}

func TestPersonFactClaimSchemaRejectsUnknownRelationAndOriginTokens(t *testing.T) {
	st := testutil.NewTestStore(t)
	personID := personFactSchemaPerson(t, st, "closed-vocabulary@example.com")
	generationID := insertPersonFactSchemaGeneration(t, st, personID, "closed-vocabulary")
	tests := []struct {
		name     string
		claimKey string
		relation string
		origin   string
	}{
		{name: "relation", claimKey: "unknown-relation", relation: "agrees", origin: "extraction"},
		{name: "origin", claimKey: "unknown-origin", relation: "support", origin: "crawler"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
				INSERT INTO person_fact_claims
					(person_id, generation_id, claim_key, target_kind, target_key, target_revision,
					 relation, submitted_value_json, origin, confidence_json)
				VALUES (?, ?, ?, 'attribute', 'favorite', 'revision-1', ?, '"value"', ?, '{}')`),
				personID, generationID, test.claimKey, test.relation, test.origin)
			assert.Error(t, err)
		})
	}
}

func personFactSchemaPerson(t *testing.T, st *store.Store, address string) int64 {
	t.Helper()
	participantID, err := st.EnsureParticipant(address, "Synthetic", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	return person.ID
}

func insertPersonFactSchemaGeneration(t *testing.T, st *store.Store, personID int64, key string) int64 {
	t.Helper()
	var id int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_generations
			(person_id, generation_key, source_cursors_json, program_id, program_version,
			 program_fingerprint, catalog_fingerprint, provider, provider_version,
			 model, model_version, provider_policy_fingerprint, resolved_at)
		VALUES (?, ?, '[]', 'program', 'v1', ?, 'catalog', 'provider', 'v1',
		        'model', 'v1', 'policy', CURRENT_TIMESTAMP)
		RETURNING id`), personID, key, strings.Repeat("a", 64)).Scan(&id)
	require.NoError(t, err)
	return id
}

func insertPersonFactSchemaEvidence(t *testing.T, st *store.Store, personID int64, key string) int64 {
	t.Helper()
	var id int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_evidence
			(person_id, evidence_key, source_class, directness, authority,
			 source_url, subject_person_id, subject_ref, excerpt, source_version,
			 event_time, recorded_time, identity_score)
		VALUES (?, ?, 'public', 'direct-other', 'ordinary', 'https://example.com/evidence',
		        ?, 'subject', 'excerpt', 'source-v1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 900)
		RETURNING id`), personID, key, personID).Scan(&id)
	require.NoError(t, err)
	return id
}

func insertPersonFactSchemaClaim(
	t *testing.T, st *store.Store, personID, generationID int64, key string,
) int64 {
	t.Helper()
	var id int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_claims
			(person_id, generation_id, claim_key, target_kind, target_key, target_revision,
			 relation, submitted_value_json, origin, confidence_json)
		VALUES (?, ?, ?, 'attribute', 'favorite', 'revision-1',
		        'support', '"value"', 'extraction', '{}')
		RETURNING id`), personID, generationID, key).Scan(&id)
	require.NoError(t, err)
	return id
}

func insertPersonFactSchemaResolution(
	t *testing.T, st *store.Store, personID, generationID int64,
) int64 {
	t.Helper()
	var id int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_resolutions
			(person_id, generation_id, target_kind, target_key, target_revision,
			 resolver_version, input_fingerprint, provider_policy_fingerprint, resolved_at)
		VALUES (?, ?, 'attribute', 'favorite', 'revision-1', 'resolver-v1',
		        'input-v1', 'policy-v1', CURRENT_TIMESTAMP)
		RETURNING id`), personID, generationID).Scan(&id)
	require.NoError(t, err)
	return id
}

func insertPersonFactSchemaDecision(
	t *testing.T,
	st *store.Store,
	personID, resolutionID, claimID int64,
	key string,
) int64 {
	t.Helper()
	var id int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_decisions
			(person_id, resolution_id, claim_id, decision_key, action, reason, score_json)
		VALUES (?, ?, ?, ?, 'retained', 'below-threshold', '{}')
		RETURNING id`), personID, resolutionID, claimID, key).Scan(&id)
	require.NoError(t, err)
	return id
}
