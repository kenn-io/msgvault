package store_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
)

func insertMachineObservationForMerge(
	t *testing.T,
	st *store.Store,
	personID int64,
	generationKey, evidenceKey string,
) int64 {
	t.Helper()
	return insertMachineObservationForMergeContent(t, st, personID, generationKey, evidenceKey, "[PERSON] remembers")
}

func insertMachineObservationForMergeContent(
	t *testing.T,
	st *store.Store,
	personID int64,
	generationKey, evidenceKey, content string,
) int64 {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)

	id := insertObservationRow(t, st, personID, 1, personobs.LevelExplicit, content)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_observation_clocks(person_id,last_sequence) VALUES(?,1)
		ON CONFLICT(person_id) DO UPDATE SET last_sequence=CASE
			WHEN person_observation_clocks.last_sequence < excluded.last_sequence THEN excluded.last_sequence
			ELSE person_observation_clocks.last_sequence END`), personID)
	require.NoError(err)
	generationID := insertPersonFactSchemaGeneration(t, st, personID, generationKey)
	evidenceID := insertPersonFactSchemaEvidence(t, st, personID, evidenceKey)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE person_observations
		SET origin='extraction', generation_id=?, program_fingerprint=?, directness='direct-self', confidence_bp=800
		WHERE id=?`), generationID, strings.Repeat("a", 64), id)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_observation_evidence
			(observation_id,evidence_id,generation_id,quote_start,quote_end,quote_sha256,
			 exclusion_reason,message_id,conversation_id,conversation_kind,authored_by_person,author_is_owner,event_time)
		VALUES(?,?,?,0,1,?,'',1,1,'email',FALSE,FALSE,CURRENT_TIMESTAMP)`),
		id, evidenceID, generationID, strings.Repeat("a", 64))
	require.NoError(err)
	assert.Equal(1, observationGet(t, st, id).SupportedCount)
	return id
}

func insertUnsupportedObservationDependent(
	t *testing.T,
	st *store.Store,
	personID, sequence, premiseID int64,
	content string,
) int64 {
	t.Helper()
	require := require.New(t)

	id := insertObservationRow(t, st, personID, sequence, personobs.LevelDeductive, content)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE person_observations
		SET origin='reasoning', directness='indirect', confidence_bp=800,
			status='unsupported', status_reason='premises-withdrawn'
		WHERE id=?`), id)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_observation_premises(derived_id,premise_id,role,ordinal)
		VALUES(?,?,'support',0)`), id, premiseID)
	require.NoError(err)
	return id
}

func TestObservationMergeCollisionPromotesSurvivorMachineRowToOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, _, _ := observationBatchFixture(t, "I run")
	kept := insertMachineObservationForMerge(
		t, f.store, f.bobPersonID, "merge-survivor", "merge-survivor-evidence",
	)
	dependent := insertUnsupportedObservationDependent(
		t, f.store, f.bobPersonID, 2, kept, "[PERSON] has a memory",
	)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_clocks(person_id,last_sequence) VALUES(?,2)
		ON CONFLICT(person_id) DO UPDATE SET last_sequence=CASE
			WHEN person_observation_clocks.last_sequence < excluded.last_sequence THEN excluded.last_sequence
			ELSE person_observation_clocks.last_sequence END`), f.bobPersonID)
	require.NoError(err)
	incoming := observationOwnerRow(t, f.store, f.alicePersonID, "[PERSON] remembers")
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE,owner_state='confirmed' WHERE id=?`), incoming)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_categories(observation_id,category,source)
		VALUES(?,'health','owner')`), incoming)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET sensitive=TRUE WHERE id=?`), incoming)
	require.NoError(err)

	merge := observationMerge(t, f.store, f.bobPersonID, f.alicePersonID)
	moved := observationGet(t, f.store, kept)
	assert.Equal(personobs.OriginOwner, moved.Origin)
	assert.Nil(moved.GenerationID)
	assert.Empty(moved.ProgramFingerprint)
	assert.Equal(personfacts.DirectOther, moved.Directness)
	assert.Empty(moved.SupportFlag)
	assert.Empty(moved.Evidence)
	assert.True(moved.Pinned)
	assert.Equal(personobs.OwnerStateConfirmed, moved.OwnerState)
	assert.True(moved.Sensitive)
	assert.Contains(moved.Categories, personobs.CategoryHealth)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, dependent).Status)
	assert.True(observationGet(t, f.store, dependent).Sensitive)
	assert.Contains(observationGet(t, f.store, dependent).Categories, personobs.CategoryHealth)
	assert.Equal(personobs.StatusSuperseded, observationGet(t, f.store, incoming).Status)

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, kept)
	assert.Equal(personobs.OriginExtraction, restored.Origin)
	assert.False(restored.Sensitive)
	assert.NotContains(restored.Categories, personobs.CategoryHealth)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, dependent).Status)
	assert.False(observationGet(t, f.store, dependent).Sensitive)
	assert.NotContains(observationGet(t, f.store, dependent).Categories, personobs.CategoryHealth)
	assert.Len(restored.Evidence, 1)
	assert.Equal(newPersonID, observationGet(t, f.store, incoming).PersonID)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, incoming).Status)
	assert.True(observationGet(t, f.store, incoming).Sensitive)
	assert.Contains(observationGet(t, f.store, incoming).Categories, personobs.CategoryHealth)
}

func TestObservationProtectedCopyCollisionPromotesSurvivorMachineRow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	derived := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] remembers", root)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET owner_state='confirmed',sensitive=TRUE WHERE id=?`), derived)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_categories(observation_id,category,source)
		VALUES(?,'health','owner')`), derived)
	require.NoError(err)
	kept := insertMachineObservationForMerge(
		t, f.store, f.bobPersonID, "protected-merge-survivor", "protected-merge-survivor-evidence",
	)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_clocks(person_id,last_sequence) VALUES(?,1)
		ON CONFLICT(person_id) DO UPDATE SET last_sequence=CASE
			WHEN person_observation_clocks.last_sequence < excluded.last_sequence THEN excluded.last_sequence
			ELSE person_observation_clocks.last_sequence END`), f.bobPersonID)
	require.NoError(err)

	merge := observationMerge(t, f.store, f.bobPersonID, f.alicePersonID)
	var copyID int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT observation_id FROM person_observation_events
		WHERE event='merge-converted' AND related_observation_id=?`), derived).Scan(&copyID))
	moved := observationGet(t, f.store, kept)
	assert.Equal(personobs.OriginOwner, moved.Origin)
	assert.Nil(moved.GenerationID)
	assert.Empty(moved.Evidence)
	assert.True(moved.Sensitive)
	assert.Contains(moved.Categories, personobs.CategoryHealth)
	assert.Equal(personobs.StatusSuperseded, observationGet(t, f.store, copyID).Status)

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, kept)
	assert.Equal(personobs.OriginExtraction, restored.Origin)
	assert.Len(restored.Evidence, 1)
	assert.NotContains(restored.Categories, personobs.CategoryHealth)
	assert.Equal(newPersonID, observationGet(t, f.store, copyID).PersonID)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, copyID).Status)
}

func TestObservationSplitRestoresConvertedCopyRejectionSuppression(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	derived := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] follows a routine", root)
	original := observationGet(t, f.store, derived)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_observations SET owner_state='confirmed' WHERE id=?`), derived)
	require.NoError(err)
	merge := observationMerge(t, f.store, f.bobPersonID, f.alicePersonID)

	var copyID int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT observation_id FROM person_observation_events
		WHERE event='merge-converted' AND related_observation_id=?`), derived).Scan(&copyID))
	converted := observationGet(t, f.store, copyID)
	assert.Equal(personobs.LevelExplicit, converted.Level)
	assert.NotEqual(original.ContentKey, converted.ContentKey)
	require.NoError(f.store.RejectObservationForTest(t.Context(), copyID))

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, copyID)
	assert.Equal(newPersonID, restored.PersonID)
	assert.Equal(personobs.StatusRejected, restored.Status)
	var absorbedSuppressions, survivorSuppressions int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), newPersonID, converted.ContentKey).Scan(&absorbedSuppressions))
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.bobPersonID, converted.ContentKey).Scan(&survivorSuppressions))
	assert.Equal(1, absorbedSuppressions)
	assert.Zero(survivorSuppressions)
}

func TestObservationSplitPreservesMergeAddedCategoryAfterReinforcement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	content := "[PERSON] remembers"
	absorbed := observationOwnerRow(t, f.store, in.PersonID, content)
	kept := observationOwnerRow(t, f.store, f.bobPersonID, content)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_categories(observation_id,category,source)
		VALUES(?,'health','owner')`), absorbed)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_observations SET sensitive=TRUE WHERE id=?`), absorbed)
	require.NoError(err)

	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	reinforced := personobs.BatchInput{PersonID: f.bobPersonID, Proposals: []personobs.ProposedObservation{{
		Level: personobs.LevelExplicit, Origin: personobs.OriginOwner, Kind: personobs.KindState,
		Content: content, ConfidenceBP: 1000, ModelCategories: []personobs.Category{personobs.CategoryHealth},
	}}}
	result := applyObservationInput(t, f.store, reinforced, 0)
	require.Len(result.Items, 1)
	require.Equal("reinforced", result.Items[0].Outcome)
	require.Equal(kept, result.Items[0].ObservationID)

	observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, kept)
	assert.Contains(restored.Categories, personobs.CategoryHealth)
	assert.True(restored.Sensitive)
}

func TestObservationSplitPreservesOwnerReaffirmation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	content := "[PERSON] remembers"
	kept := insertMachineObservationForMergeContent(t, f.store, f.bobPersonID, "owner-reaffirm-survivor", "owner-reaffirm-survivor-evidence", content)
	absorbed := observationOwnerRow(t, f.store, f.alicePersonID, content)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	assert.Equal(personobs.OriginOwner, observationGet(t, f.store, kept).Origin)

	reaffirmation := personobs.BatchInput{PersonID: f.bobPersonID, Proposals: []personobs.ProposedObservation{{
		Level: personobs.LevelExplicit, Origin: personobs.OriginOwner, Kind: personobs.KindState,
		Content: content, ConfidenceBP: 1000,
	}}}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), reaffirmation)
	require.NoError(err)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, 0, "")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("reinforced", result.Items[0].Outcome)
	assert.Equal(kept, result.Items[0].ObservationID)
	var detail string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT detail FROM person_observation_events
		WHERE observation_id=? AND event='reinforced' ORDER BY id DESC LIMIT 1`), kept).Scan(&detail))
	assert.Contains(detail, `"owner_assertion":true`)

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, kept)
	assert.Equal(f.bobPersonID, restored.PersonID)
	assert.Equal(personobs.OriginOwner, restored.Origin)
	assert.Nil(restored.GenerationID)
	assert.Empty(restored.ProgramFingerprint)
	restoredAbsorbed := observationGet(t, f.store, absorbed)
	assert.Equal(newPersonID, restoredAbsorbed.PersonID)
	assert.Equal(personobs.StatusActive, restoredAbsorbed.Status)
	assert.NotZero(newPersonID)
}

func TestObservationSplitPreservesMachineCategoryAssertion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepJournalFixture(t, true, true)
	content := "[PERSON] likes the yellow mug"
	absorbed := observationOwnerRow(t, f.store, f.alicePersonID, content)
	kept := insertMachineObservationForMergeContent(t, f.store, f.bobPersonID, "machine-sensitive-survivor", "machine-sensitive-survivor-evidence", content)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_observations SET sensitive=TRUE WHERE id=?`), absorbed)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_categories(observation_id,category,source)
		VALUES(?,'health','owner')`), absorbed)
	require.NoError(err)
	merge := observationMerge(t, f.store, f.bobPersonID, f.alicePersonID)
	assert.True(observationGet(t, f.store, kept).Sensitive)

	body := "I like the yellow mug"
	messageID := f.insertMessage(t, "machine-sensitive-reaffirmation", "email", f.bobID,
		time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, messageID, body)
	window, err := f.store.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{
		PersonID: f.bobPersonID, Lane: peoplesweep.SourceConversationText,
		ThroughSequence: latestPersonSweepSequence(t, f.store), Limit: 100,
	})
	require.NoError(err)
	evidence := sweepEvidenceInput(t, sweepItem(t, window.Seeds, messageID))
	offset := strings.Index(evidence.Excerpt, body)
	require.NotEqual(-1, offset)
	fingerprint := strings.Repeat("a", 64)
	generationID := insertPersonFactSchemaGeneration(t, f.store, f.bobPersonID, "machine-sensitive-reaffirmation")
	in := personobs.BatchInput{
		PersonID: f.bobPersonID, ProgramFingerprint: fingerprint,
		Generation: &personfacts.GenerationInput{PersonID: f.bobPersonID, ProgramFingerprint: fingerprint},
		Proposals: []personobs.ProposedObservation{{
			Level: personobs.LevelExplicit, Origin: personobs.OriginExtraction, Kind: personobs.KindPreference,
			Content: content, Attribution: personobs.AttributionSelf, ConfidenceBP: 800,
			ModelCategories: []personobs.Category{personobs.CategoryHealth},
			Support:         []personobs.SupportInput{{Evidence: evidence, Quote: body, QuoteStart: len(evidence.Excerpt[:offset])}},
		}},
	}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	assert.False(batch.Items[0].SensitiveAssertion)
	assert.True(batch.Items[0].Proposal.Sensitive)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generationID, "extractor")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("reinforced", result.Items[0].Outcome)
	assert.Equal(kept, result.Items[0].ObservationID)
	var detail string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT detail FROM person_observation_events
		WHERE observation_id=? AND event='reinforced' ORDER BY id DESC LIMIT 1`), kept).Scan(&detail))
	assert.Contains(detail, `"asserted_categories":["health"]`)
	var sensitivityJournalRows int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_merge_rows
		WHERE merge_id=? AND table_name='person_observations' AND snapshot_path='merge-added-observation-sensitivity'
		AND current_row_id=?`), merge.Merge.ID, kept).Scan(&sensitivityJournalRows))
	require.Equal(1, sensitivityJournalRows)

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	restored := observationGet(t, f.store, kept)
	assert.Equal(f.bobPersonID, restored.PersonID)
	assert.True(restored.Sensitive)
	assert.Contains(restored.Categories, personobs.CategoryHealth)
	assert.NotZero(newPersonID)
}

func TestObservationSplitPreservesPostMergeOwnerEndDecision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	content := "[PERSON] runs"
	kept := observationOwnerRow(t, f.store, f.bobPersonID, content)
	absorbed := observationOwnerRow(t, f.store, in.PersonID, content)
	key := personobs.ContentKey(personobs.LevelExplicit, "", content)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)

	end := time.Date(2026, time.July, 4, 0, 0, 0, 0, time.UTC)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_observations
		SET status='superseded', status_reason='owner-superseded', valid_until=?
		WHERE id=?`), end, absorbed)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_events(person_id,observation_id,event,actor)
		VALUES(?,?,'ended','owner')`), f.bobPersonID, absorbed)
	require.NoError(err)

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	ended := observationGet(t, f.store, absorbed)
	assert.Equal(newPersonID, ended.PersonID)
	assert.Equal(personobs.StatusSuperseded, ended.Status)
	assert.Equal(personobs.StatusReasonOwnerSuperseded, ended.StatusReason)
	require.NotNil(ended.ValidUntil)
	assert.True(end.Equal(*ended.ValidUntil))
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, kept).Status)
	var absorbedSuppressions, survivorSuppressions int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), newPersonID, key).Scan(&absorbedSuppressions))
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.bobPersonID, key).Scan(&survivorSuppressions))
	assert.Zero(absorbedSuppressions)
	assert.Zero(survivorSuppressions)
}

func TestObservationSplitRestoresPostMergeRejectionSuppressionToAbsorbedPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	content := "[PERSON] swims"
	observation := observationOwnerRow(t, f.store, in.PersonID, content)
	key := personobs.ContentKey(personobs.LevelExplicit, "", content)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	require.NoError(f.store.RejectObservationForTest(t.Context(), observation))

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	rejected := observationGet(t, f.store, observation)
	assert.Equal(newPersonID, rejected.PersonID)
	assert.Equal(personobs.StatusRejected, rejected.Status)
	assert.Equal(personobs.StatusReasonOwnerRejected, rejected.StatusReason)
	var absorbedSuppressions, survivorSuppressions int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), newPersonID, key).Scan(&absorbedSuppressions))
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.bobPersonID, key).Scan(&survivorSuppressions))
	assert.Equal(1, absorbedSuppressions)
	assert.Zero(survivorSuppressions)
}

func TestObservationSplitPreservesSurvivorSuppressionAfterPostMergeRejection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	content := "[PERSON] swims"
	observation := observationOwnerRow(t, f.store, in.PersonID, content)
	key := personobs.ContentKey(personobs.LevelExplicit, "", content)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor)
		VALUES(?,?,'owner-forgotten','owner')`), f.bobPersonID, key)
	require.NoError(err)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	require.NoError(f.store.RejectObservationForTest(t.Context(), observation))

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	rejected := observationGet(t, f.store, observation)
	assert.Equal(newPersonID, rejected.PersonID)
	assert.Equal(personobs.StatusRejected, rejected.Status)
	assert.Equal(personobs.StatusReasonOwnerRejected, rejected.StatusReason)
	var survivorReason, absorbedReason string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT reason FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.bobPersonID, key).Scan(&survivorReason))
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT reason FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), newPersonID, key).Scan(&absorbedReason))
	assert.Equal("owner-forgotten", survivorReason)
	assert.Equal("owner-rejected", absorbedReason)
}

func TestObservationSplitPreservesSameSecondPostMergeRejection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	content := "[PERSON] swims"
	survivorObservation := observationOwnerRow(t, f.store, f.bobPersonID, content)
	absorbedObservation := observationOwnerRow(t, f.store, in.PersonID, content)
	key := personobs.ContentKey(personobs.LevelExplicit, "", content)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor)
		VALUES(?,?,'owner-rejected','owner')`), f.bobPersonID, key)
	require.NoError(err)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	var suppressionID int64
	var baselineRejectedAt string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT id,rejected_at FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.bobPersonID, key).
		Scan(&suppressionID, &baselineRejectedAt))
	require.NoError(f.store.RejectObservationForTest(t.Context(), absorbedObservation))
	// Keep the database-clock field identical to the merge baseline while retaining
	// the real post-merge rejection event, as when CURRENT_TIMESTAMP has one-second
	// resolution and both decisions happen in the same second.
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_observation_suppressions SET rejected_at=? WHERE id=?`), baselineRejectedAt, suppressionID)
	require.NoError(err)

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	rejected := observationGet(t, f.store, absorbedObservation)
	assert.Equal(newPersonID, rejected.PersonID)
	assert.Equal(personobs.StatusRejected, rejected.Status)
	assert.Equal(personobs.StatusReasonOwnerRejected, rejected.StatusReason)
	for _, personID := range []int64{f.bobPersonID, newPersonID} {
		var activeSuppressions int
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT COUNT(*) FROM person_observation_suppressions
			WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), personID, key).Scan(&activeSuppressions))
		assert.Equalf(1, activeSuppressions, "person %d", personID)
	}
	assert.Equal(f.bobPersonID, observationGet(t, f.store, survivorObservation).PersonID)
}

func TestObservationSplitPreservesSurvivorSuppressionLift(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepJournalFixture(t, true, true)
	content := "[PERSON] runs"
	survivor := observationOwnerRow(t, f.store, f.bobPersonID, content)
	absorbed := observationOwnerRow(t, f.store, f.alicePersonID, content)
	key := personobs.ContentKey(personobs.LevelExplicit, "", content)
	rejectedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at)
		VALUES(?,?,'outdated','owner',?)`), f.bobPersonID, key, rejectedAt)
	require.NoError(err)
	var survivorSuppressionID int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT id FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.bobPersonID, key).Scan(&survivorSuppressionID))
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at)
		VALUES(?,?,'outdated','owner',?)`), f.alicePersonID, key, rejectedAt)
	require.NoError(err)
	var absorbedSuppressionID int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT id FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.alicePersonID, key).Scan(&absorbedSuppressionID))
	assert.Less(survivorSuppressionID, absorbedSuppressionID)
	merge := observationMerge(t, f.store, f.bobPersonID, f.alicePersonID)

	body := "I run"
	messageID := f.insertMessage(t, "survivor-suppression-lift", "email", f.bobID,
		time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, messageID, body)
	window, err := f.store.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{
		PersonID: f.bobPersonID, Lane: peoplesweep.SourceConversationText,
		ThroughSequence: latestPersonSweepSequence(t, f.store), Limit: 100,
	})
	require.NoError(err)
	evidence := sweepEvidenceInput(t, sweepItem(t, window.Seeds, messageID))
	offset := strings.Index(evidence.Excerpt, body)
	require.NotEqual(-1, offset)
	fingerprint := strings.Repeat("a", 64)
	generationID := insertPersonFactSchemaGeneration(t, f.store, f.bobPersonID, "survivor-suppression-lift")
	input := personobs.BatchInput{
		PersonID: f.bobPersonID, ProgramFingerprint: fingerprint,
		Generation: &personfacts.GenerationInput{PersonID: f.bobPersonID, ProgramFingerprint: fingerprint},
		Proposals: []personobs.ProposedObservation{{
			Level: personobs.LevelExplicit, Origin: personobs.OriginExtraction, Kind: personobs.KindState,
			Content: content, Attribution: personobs.AttributionSelf, ConfidenceBP: 800,
			Support: []personobs.SupportInput{{Evidence: evidence, Quote: body, QuoteStart: len(evidence.Excerpt[:offset])}},
		}},
	}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), input)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generationID, "extractor")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("reinforced", result.Items[0].Outcome)
	assert.Equal(survivor, result.Items[0].ObservationID)
	var liftedObservationID int64
	var liftDetail string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT observation_id,detail FROM person_observation_events
		WHERE event='suppression-lifted' ORDER BY id DESC LIMIT 1`)).
		Scan(&liftedObservationID, &liftDetail))
	assert.Equal(survivor, liftedObservationID)
	assert.Equal("outdated-new-support:suppression:"+strconv.FormatInt(survivorSuppressionID, 10), liftDetail)

	var activeSurvivorSuppressions int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.bobPersonID, key).Scan(&activeSurvivorSuppressions))
	assert.Zero(activeSurvivorSuppressions)

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	assert.Equal(f.bobPersonID, observationGet(t, f.store, survivor).PersonID)
	assert.Equal(newPersonID, observationGet(t, f.store, absorbed).PersonID)
	var clearedSurvivorSuppressions, activeRestoredSuppressions int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NOT NULL`), f.bobPersonID, key).Scan(&clearedSurvivorSuppressions))
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), f.bobPersonID, key).Scan(&activeSurvivorSuppressions))
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_suppressions
		WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), newPersonID, key).Scan(&activeRestoredSuppressions))
	assert.Equal(1, clearedSurvivorSuppressions)
	assert.Zero(activeSurvivorSuppressions)
	assert.Equal(1, activeRestoredSuppressions)
}

func TestObservationSplitRemovesPostMergeCrossPersonSupportAndCitations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	premise := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] likes tea")
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)

	generationID := insertPersonFactSchemaGeneration(t, f.store, f.bobPersonID, "post-merge-observation-generation")
	evidenceID := insertPersonFactSchemaEvidence(t, f.store, f.bobPersonID, "post-merge-observation-evidence")
	messageID := f.insertMessage(t, "post-merge-evidence", "email", f.bobID, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_evidence
			(observation_id,evidence_id,generation_id,quote_start,quote_end,quote_sha256,
			 exclusion_reason,message_id,conversation_id,conversation_kind,authored_by_person,author_is_owner,event_time)
		VALUES(?,?,?,0,1,?,'',?,?,'email',TRUE,FALSE,CURRENT_TIMESTAMP)`),
		premise, evidenceID, generationID, strings.Repeat("a", 64), messageID, f.conversationID)
	require.NoError(err)

	in.PersonID = f.bobPersonID
	in.Generation.PersonID = f.bobPersonID
	dependent := observationDerived(t, f.store, in, generationID, personobs.LevelDeductive, "[PERSON] enjoys a warm drink", premise)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, dependent).Status)
	assert.Len(observationGet(t, f.store, premise).Evidence, 1)

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	restoredPremise := observationGet(t, f.store, premise)
	assert.Equal(newPersonID, restoredPremise.PersonID)
	assert.Empty(restoredPremise.Evidence)
	assert.Zero(restoredPremise.MentionCount)
	assert.Equal(personobs.StatusUnsupported, observationGet(t, f.store, dependent).Status)

	var crossingPremises, crossPersonCitations int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_premises edge
		JOIN person_observations derived ON derived.id=edge.derived_id
		JOIN person_observations premise ON premise.id=edge.premise_id
		WHERE derived.person_id<>premise.person_id`)).Scan(&crossingPremises))
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_evidence citation
		JOIN person_observations observation ON observation.id=citation.observation_id
		JOIN person_fact_evidence evidence ON evidence.id=citation.evidence_id
		WHERE observation.person_id<>evidence.person_id`)).Scan(&crossPersonCitations))
	assert.Zero(crossingPremises)
	assert.Zero(crossPersonCitations)
}

func TestObservationSplitMarksInductiveRootWhenSupportPremiseLeaves(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	absorbedPremise := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] runs before work")
	survivorPremiseOne := observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] runs after work")
	survivorPremiseTwo := observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] runs on weekends")
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	generationID := insertPersonFactSchemaGeneration(t, f.store, f.bobPersonID, "split-inductive-premises")
	in.PersonID = f.bobPersonID
	in.Generation.PersonID = f.bobPersonID
	derived := observationDerived(t, f.store, in, generationID, personobs.LevelInductive,
		"[PERSON] follows a steady running routine", absorbedPremise, survivorPremiseOne, survivorPremiseTwo)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, derived).Status)
	assert.Nil(observationGet(t, f.store, derived).PremiseChangedAt)

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	assert.Equal(newPersonID, observationGet(t, f.store, absorbedPremise).PersonID)
	updated := observationGet(t, f.store, derived)
	assert.Equal(personobs.StatusActive, updated.Status)
	assert.NotNil(updated.PremiseChangedAt)
	var premiseCount, recheckEvents int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_premises WHERE derived_id=?`), derived).Scan(&premiseCount))
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_events
		WHERE observation_id=? AND event='premise-changed' AND detail='person-split'`), derived).Scan(&recheckEvents))
	assert.Equal(2, premiseCount)
	assert.Equal(1, recheckEvents)
}

func TestObservationSplitReconcilesReferencesForPartialAncestorRestoration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	premise := observationOwnerRow(t, f.store, f.bobPersonID, "[PERSON] likes tea")
	firstMerge := observationMerge(t, f.store, f.alicePersonID, f.bobPersonID)

	current := mustPromotedPerson(t, f.store,
		"split-ancestor-current@example.com", "Current")
	absorbed, err := f.store.GetPersonContext(t.Context(), firstMerge.Person.ID)
	require.NoError(err)
	secondMerge, err := f.store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: current.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: current.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "observation-ancestor-partial-merge",
		Actor:                    "owner",
	})
	require.NoError(err)

	in.PersonID = secondMerge.Person.ID
	in.Generation.PersonID = secondMerge.Person.ID
	generation := insertPersonFactSchemaGeneration(
		t, f.store, secondMerge.Person.ID, "observation-ancestor-partial-generation",
	)
	var nextSequence int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COALESCE(MAX(sequence),0)+1 FROM person_observations WHERE person_id=?`),
		secondMerge.Person.ID).Scan(&nextSequence))
	evidenceObservation := insertObservationRow(
		t, f.store, secondMerge.Person.ID, nextSequence,
		personobs.LevelExplicit, "[PERSON] keeps a journal",
	)
	evidenceGeneration := insertPersonFactSchemaGeneration(
		t, f.store, secondMerge.Person.ID, "observation-ancestor-partial-evidence-generation",
	)
	evidence := insertPersonFactSchemaEvidence(
		t, f.store, secondMerge.Person.ID, "observation-ancestor-partial-evidence",
	)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_observations
		SET origin='extraction', generation_id=?, program_fingerprint=?,
			directness='direct-self', confidence_bp=800 WHERE id=?`),
		evidenceGeneration, strings.Repeat("b", 64), evidenceObservation)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_evidence
			(observation_id,evidence_id,generation_id,quote_start,quote_end,quote_sha256,
			 exclusion_reason,message_id,conversation_id,conversation_kind,
			 authored_by_person,author_is_owner,event_time)
		VALUES(?,?,?,0,1,?,'',1,1,'email',FALSE,FALSE,CURRENT_TIMESTAMP)`),
		evidenceObservation, evidence, evidenceGeneration, strings.Repeat("c", 64))
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_clocks(person_id,last_sequence) VALUES(?,?)
		ON CONFLICT(person_id) DO UPDATE SET last_sequence=CASE
			WHEN person_observation_clocks.last_sequence < excluded.last_sequence THEN excluded.last_sequence
			ELSE person_observation_clocks.last_sequence END`),
		secondMerge.Person.ID, nextSequence)
	require.NoError(err)
	derived := observationDerived(t, f.store, in, generation,
		personobs.LevelDeductive, "[PERSON] enjoys tea", premise)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, derived).Status)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_evidence
			(observation_id,evidence_id,generation_id,quote_start,quote_end,quote_sha256,
			 exclusion_reason,message_id,conversation_id,conversation_kind,
			 authored_by_person,author_is_owner,event_time)
		SELECT ?,evidence_id,generation_id,quote_start,quote_end,quote_sha256,
			exclusion_reason,message_id,conversation_id,conversation_kind,
			authored_by_person,author_is_owner,event_time
		FROM person_observation_evidence WHERE observation_id=?`), premise, evidenceObservation)
	require.NoError(err)
	assert.Len(observationGet(t, f.store, premise).Evidence, 1)

	source, err := f.store.GetPersonContext(t.Context(), secondMerge.Person.ID)
	require.NoError(err)
	split, err := f.store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID:         source.ID,
		MergeID:                secondMerge.Merge.ID,
		ParticipantIDs:         []int64{f.bobID},
		ExpectedSourceRevision: source.Revision,
		IdempotencyKey:         "observation-ancestor-partial-split",
		Actor:                  "owner",
	})
	require.NoError(err)
	assert.False(split.ExactReversal)
	assert.Equal(split.NewPerson.ID, observationGet(t, f.store, premise).PersonID)
	assert.Empty(observationGet(t, f.store, premise).Evidence)
	assert.Equal(personobs.StatusUnsupported, observationGet(t, f.store, derived).Status)
	assert.Empty(observationGet(t, f.store, derived).Premises)

	var crossingPremises, crossPersonCitations int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_premises edge
		JOIN person_observations derived ON derived.id=edge.derived_id
		JOIN person_observations premise ON premise.id=edge.premise_id
		WHERE derived.person_id<>premise.person_id`)).Scan(&crossingPremises))
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_evidence citation
		JOIN person_observations observation ON observation.id=citation.observation_id
		JOIN person_fact_evidence evidence ON evidence.id=citation.evidence_id
		WHERE observation.person_id<>evidence.person_id`)).Scan(&crossPersonCitations))
	assert.Zero(crossingPremises)
	assert.Zero(crossPersonCitations)
}

func TestObservationSplitRefreshesEvidenceSupportAfterCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	kept := insertMachineObservationForMerge(
		t, f.store, f.bobPersonID, "split-collision", "split-collision-evidence",
	)
	dependent := insertUnsupportedObservationDependent(
		t, f.store, f.bobPersonID, 2, kept, "[PERSON] has a memory",
	)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_clocks(person_id,last_sequence) VALUES(?,2)
		ON CONFLICT(person_id) DO UPDATE SET last_sequence=excluded.last_sequence`), f.bobPersonID)
	require.NoError(err)
	incoming := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] remembers")
	statusUpdate := observationStatusInput(t, f.store, f.bobPersonID, kept, false, 7)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)

	assert.Equal(personobs.StatusActive, observationGet(t, f.store, kept).Status)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, dependent).Status)
	_, err = f.store.ApplyPersonFactGenerationContext(t.Context(), statusUpdate, nil)
	require.NoError(err)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, kept).Status)

	observationSplit(t, f.store, merge, f.aliceID)
	withdrawn := observationGet(t, f.store, kept)
	assert.Equal(personobs.OriginExtraction, withdrawn.Origin)
	assert.Zero(withdrawn.SupportedCount)
	assert.Equal(personobs.StatusUnsupported, withdrawn.Status)
	assert.Equal(personobs.StatusReasonEvidenceUnsupported, withdrawn.StatusReason)
	assert.Equal(personobs.StatusUnsupported, observationGet(t, f.store, dependent).Status)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, incoming).Status)
}

func TestObservationSplitDoesNotRestoreCitationForDeletedObservation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	kept := insertMachineObservationForMerge(
		t, f.store, f.bobPersonID, "deleted-citation-merge", "deleted-citation-evidence",
	)
	content := "[PERSON] remembers"
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_observation_clocks(person_id,last_sequence) VALUES(?,1)
		ON CONFLICT(person_id) DO UPDATE SET last_sequence=CASE
			WHEN person_observation_clocks.last_sequence < excluded.last_sequence THEN excluded.last_sequence
			ELSE person_observation_clocks.last_sequence END`), f.bobPersonID)
	require.NoError(err)
	incoming := observationOwnerRow(t, f.store, in.PersonID, content)
	merge := observationMerge(t, f.store, f.bobPersonID, in.PersonID)
	assert.Equal(personobs.OriginOwner, observationGet(t, f.store, kept).Origin)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM person_observations WHERE id=?`), kept)
	require.NoError(err)

	newPersonID := observationSplit(t, f.store, merge, f.aliceID)
	assert.Equal(newPersonID, observationGet(t, f.store, incoming).PersonID)
	var citationCount int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_observation_evidence WHERE observation_id=?`), kept).Scan(&citationCount))
	assert.Zero(citationCount)
}
