package store_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestKnownEvidenceIncludesNonInvalidClaims(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "known-claims@example.com")
	generation := insertPersonFactSchemaGeneration(t, st, person, "known-claims")
	old := insertKnownEvidence(t, st, person, 101, "claim-old", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	contradict := insertKnownEvidence(t, st, person, 102, "claim-contradict", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	supersede := insertKnownEvidence(t, st, person, 103, "claim-supersede", time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))
	invalid := insertKnownEvidence(t, st, person, 104, "claim-invalid", time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
	insertKnownClaim(t, st, person, generation, "old", "support", old)
	insertKnownClaim(t, st, person, generation, "contradict", "contradict", contradict)
	insertKnownClaim(t, st, person, generation, "supersede", "supersede", supersede)
	insertKnownClaim(t, st, person, generation, "invalid", "invalid", invalid)

	got, err := st.ListPersonKnownEvidenceMessages(t.Context(), peoplesweep.KnownEvidenceRequest{
		PersonID: person,
		Limit:    10,
	})
	require.NoError(t, err)
	assert.Equal(t, []int64{103, 102, 101}, got)
}

func TestKnownEvidenceTargetScoped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "known-target@example.com")
	generation := insertPersonFactSchemaGeneration(t, st, person, "known-target")
	employer := insertKnownEvidence(t, st, person, 201, "target-employer", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	city := insertKnownEvidence(t, st, person, 202, "target-city", time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))
	targetedObservation := insertKnownEvidence(t, st, person, 203, "target-observation", time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
	insertKnownClaim(t, st, person, generation, "employer", "support", employer)
	insertKnownClaim(t, st, person, generation, "city", "support", city)
	insertKnownObservation(t, st, person, "active", "attribute", "employer", "", targetedObservation)

	got, err := st.ListPersonKnownEvidenceMessages(t.Context(), peoplesweep.KnownEvidenceRequest{
		PersonID: person, TargetKind: personfacts.TargetAttribute, TargetKey: "employer", Limit: 10,
	})
	require.NoError(err)
	assert.Equal([]int64{203, 201}, got)
	got, err = st.ListPersonKnownEvidenceMessages(t.Context(), peoplesweep.KnownEvidenceRequest{
		PersonID: person, TargetKind: personfacts.TargetEmployment, TargetKey: "employer", Limit: 10,
	})
	require.NoError(err)
	assert.Empty(got)
	got, err = st.ListPersonKnownEvidenceMessages(t.Context(), peoplesweep.KnownEvidenceRequest{PersonID: person, Limit: 10})
	require.NoError(err)
	assert.Equal([]int64{203, 202, 201}, got)
}

func TestKnownEvidenceIncludesObservationCitations(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "known-observations@example.com")
	active := insertKnownEvidence(t, st, person, 301, "observation-active", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	superseded := insertKnownEvidence(t, st, person, 302, "observation-superseded", time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))
	rejected := insertKnownEvidence(t, st, person, 303, "observation-rejected", time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
	unsupported := insertKnownEvidence(t, st, person, 304, "observation-unsupported", time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC))
	hidden := insertKnownEvidence(t, st, person, 305, "observation-hidden", time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	insertKnownObservation(t, st, person, "active", "", "", "", active)
	insertKnownObservation(t, st, person, "superseded", "", "", "", superseded)
	insertKnownObservation(t, st, person, "rejected", "", "", "", rejected)
	insertKnownObservation(t, st, person, "unsupported", "", "", "", unsupported)
	insertKnownObservation(t, st, person, "active", "", "", "hidden_text", hidden)

	got, err := st.ListPersonKnownEvidenceMessages(t.Context(), peoplesweep.KnownEvidenceRequest{PersonID: person, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, []int64{303, 302, 301}, got)
}

func TestKnownEvidenceFollowsSupportChronology(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "known-chronology@example.com")
	g2 := insertPersonFactSchemaGeneration(t, st, person, "known-g2")
	g3 := insertPersonFactSchemaGeneration(t, st, person, "known-g3")
	g4 := insertPersonFactSchemaGeneration(t, st, person, "known-g4")
	evidence := insertKnownEvidence(t, st, person, 401, "chronology", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	insertKnownClaim(t, st, person, g2, "chronology", "support", evidence)
	setGenerationResolvedAt(t, st, g2, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	setGenerationResolvedAt(t, st, g3, time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))
	setGenerationResolvedAt(t, st, g4, time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	insertStatusEvent(t, st, person, g2, evidence, false)
	insertStatusEvent(t, st, person, g3, evidence, true)
	insertStatusEvent(t, st, person, g4, evidence, false)

	got, err := st.ListPersonKnownEvidenceMessages(t.Context(), peoplesweep.KnownEvidenceRequest{PersonID: person, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, []int64{401}, got)
}

func TestKnownEvidenceDedupesMessagesAndPagesPastUnsupported(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "known-pages@example.com")
	generation := insertPersonFactSchemaGeneration(t, st, person, "known-pages")
	for i := range 4 {
		evidence := insertKnownEvidence(t, st, person, int64(500+i), fmt.Sprintf("unsupported-%d", i), time.Date(2026, 10, 25-i, 0, 0, 0, 0, time.UTC))
		insertKnownClaim(t, st, person, generation, fmt.Sprintf("unsupported-%d", i), "support", evidence)
		insertStatusEvent(t, st, person, generation, evidence, false)
	}
	first := insertKnownEvidence(t, st, person, 600, "duplicate-first", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	second := insertKnownEvidence(t, st, person, 600, "duplicate-second", time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC))
	last := insertKnownEvidence(t, st, person, 601, "supported-after-page", time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC))
	insertKnownClaim(t, st, person, generation, "duplicate-first", "support", first)
	insertKnownClaim(t, st, person, generation, "duplicate-second", "support", second)
	insertKnownClaim(t, st, person, generation, "supported-after-page", "support", last)

	got, err := st.ListPersonKnownEvidenceMessages(t.Context(), peoplesweep.KnownEvidenceRequest{PersonID: person, Limit: 1})
	require.NoError(err)
	assert.Equal([]int64{600}, got)
	got, err = st.ListPersonKnownEvidenceMessages(t.Context(), peoplesweep.KnownEvidenceRequest{PersonID: person, Limit: 10})
	require.NoError(err)
	assert.Equal([]int64{600, 601}, got)
}

func TestKnownEvidenceSkipsNullMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "known-source@example.com")
	generation := insertPersonFactSchemaGeneration(t, st, person, "known-source")
	nullMessage := insertKnownEvidence(t, st, person, 0, "null-message", time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))
	insertKnownClaim(t, st, person, generation, "null-message", "support", nullMessage)
	valid := insertKnownEvidence(t, st, person, 701, "valid", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	insertKnownClaim(t, st, person, generation, "valid", "support", valid)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_fact_evidence SET message_id=NULL WHERE id=?`), nullMessage)
	require.NoError(err)

	got, err := st.ListPersonKnownEvidenceMessages(t.Context(), peoplesweep.KnownEvidenceRequest{PersonID: person, Limit: 10})
	require.NoError(err)
	assert.Equal([]int64{701}, got)
}

func insertKnownEvidence(t *testing.T, st *store.Store, personID, messageID int64, key string, eventTime time.Time) int64 {
	t.Helper()
	var id int64
	var message any = messageID
	if messageID == 0 {
		message = nil
	}
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_evidence
			(person_id, evidence_key, source_class, directness, authority, message_id,
			 source_version, event_time, recorded_time, identity_score)
		VALUES (?, ?, 'archive', 'direct-other', 'ordinary', ?, 'source-v1', ?, ?, 900)
		RETURNING id`), personID, key, message, eventTime, eventTime).Scan(&id)
	require.NoError(t, err)
	return id
}

func insertKnownClaim(t *testing.T, st *store.Store, personID, generationID int64, key, relation string, evidenceIDs ...int64) {
	t.Helper()
	var claimID int64
	claimKey := fmt.Sprintf("known-attribute-%s-%s", key, relation)
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_claims
			(person_id, generation_id, claim_key, target_kind, target_key, target_revision,
			 relation, submitted_value_json, origin, confidence_json)
		VALUES (?, ?, ?, 'attribute', ?, 'revision-1', ?, '"value"', 'extraction', '{}')
		RETURNING id`), personID, generationID, claimKey, key, relation).Scan(&claimID)
	require.NoError(t, err)
	for _, evidenceID := range evidenceIDs {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_fact_claim_evidence(claim_id,evidence_id) VALUES(?,?)`), claimID, evidenceID)
		require.NoError(t, err)
	}
}

func insertKnownObservation(t *testing.T, st *store.Store, personID int64, status, kind, key, exclusion string, evidenceID int64) {
	t.Helper()
	sequence := int64(1)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(MAX(sequence),0)+1 FROM person_observations WHERE person_id=?`), personID).Scan(&sequence))
	content := fmt.Sprintf("[PERSON] known observation %d", sequence)
	var observationID int64
	var targetKind, targetKey any
	if kind != "" {
		targetKind, targetKey = kind, key
	}
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_observations
			(person_id, sequence, level, origin, kind, content, content_key, normalization_version,
			 directness, sensitive, confidence_bp, target_kind, target_key, status, stability, stability_source)
		VALUES (?, ?, 'explicit', 'extraction', 'state', ?, ?, 'obs-norm-v1',
		        'direct-other', FALSE, 1000, ?, ?, ?, 'durable', 'program')
		RETURNING id`), personID, sequence, content, personobs.ContentKey(personobs.LevelExplicit, "", content), targetKind, targetKey, status).Scan(&observationID)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_observation_evidence
			(observation_id, evidence_id, quote_start, quote_end, quote_sha256, exclusion_reason,
			 message_id, conversation_id, conversation_kind, authored_by_person, author_is_owner, event_time)
		VALUES (?, ?, 0, 7, ?, ?, 1, 1, 'direct_chat', TRUE, FALSE, CURRENT_TIMESTAMP)`), observationID, evidenceID, strings.Repeat("a", 64), exclusion)
	require.NoError(t, err)
}

func insertStatusEvent(t *testing.T, st *store.Store, personID, generationID, evidenceID int64, supported bool) {
	t.Helper()
	var evidenceKey string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT evidence_key FROM person_fact_evidence WHERE id=?`), evidenceID).Scan(&evidenceKey))
	reason := "source-deleted"
	if supported {
		reason = "source-reimported"
	}
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_evidence_status_events
			(person_id, generation_id, evidence_id, evidence_key, source_version, supported, reason)
		VALUES (?, ?, ?, ?, 'source-v1', ?, ?)`), personID, generationID, evidenceID, evidenceKey, supported, reason)
	require.NoError(t, err)
}

func setGenerationResolvedAt(t *testing.T, st *store.Store, generationID int64, resolvedAt time.Time) {
	t.Helper()
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_fact_generations SET resolved_at=? WHERE id=?`), resolvedAt, generationID)
	require.NoError(t, err)
}
