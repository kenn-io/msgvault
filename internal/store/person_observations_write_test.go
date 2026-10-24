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

func applyObservationInput(t *testing.T, st *store.Store, in personobs.BatchInput, generation int64) personobs.BatchResult {
	t.Helper()
	require := require.New(t)
	batch, err := st.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	result, err := st.ApplyPersonObservationBatch(t.Context(), batch, generation, "owner")
	require.NoError(err)
	return result
}

func observationSupportForMessage(t *testing.T, f personSweepJournalFixture, key, body, quote string, at time.Time) (personobs.SupportInput, int64) {
	t.Helper()
	require := require.New(t)
	messageID := f.insertMessage(t, key, "email", f.aliceID, at)
	addSweepBody(t, f, messageID, body)
	item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, messageID)
	evidence := sweepEvidenceInput(t, item)
	offset := strings.Index(evidence.Excerpt, quote)
	require.NotEqual(-1, offset)
	return personobs.SupportInput{Evidence: evidence, Quote: quote, QuoteStart: len([]rune(evidence.Excerpt[:offset]))}, messageID
}

func withdrawObservationEvidence(t *testing.T, st *store.Store, personID, observationID, messageID int64) {
	t.Helper()
	require := require.New(t)
	generationID := insertPersonFactSchemaGeneration(t, st, personID, "withdrawn-observation-evidence-"+strconv.FormatInt(messageID, 10))
	var evidenceID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT evidence_id FROM person_observation_evidence WHERE observation_id=? AND message_id=?`), observationID, messageID).Scan(&evidenceID))
	var evidenceKey, sourceVersion string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT evidence_key,source_version FROM person_fact_evidence WHERE id=?`), evidenceID).Scan(&evidenceKey, &sourceVersion))
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_fact_evidence_status_events(person_id,generation_id,evidence_id,evidence_key,source_version,supported,reason) VALUES(?,?,?,?,?,FALSE,?)`), personID, generationID, evidenceID, evidenceKey, sourceVersion, personfacts.EvidenceStatusSourceDeleted)
	require.NoError(err)
}

func TestApplyObservationBatchCreatesWithSequenceAndLeaf(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	result := applyObservationInput(t, f.store, in, generation)
	require.Len(result.Items, 1)
	assert.Equal("created", result.Items[0].Outcome)
	detail, err := f.store.GetPersonObservation(t.Context(), result.Items[0].ObservationID)
	require.NoError(err)
	assert.Equal(int64(1), detail.Sequence)
	assert.Equal(personobs.StatusActive, detail.Status)
	assert.Equal(1, detail.MentionCount)
	assert.Equal(1, detail.SupportedCount)
	require.Len(detail.Evidence, 1)
	assert.Equal(f.conversationID, detail.Evidence[0].Leaf.ConversationID)
	assert.Equal(&generation, detail.GenerationID)
}
func TestApplyObservationBatchReinforcesNormalizedDuplicate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	first := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	in.Proposals[0].Content = "[person]  RUNS."
	repeat := applyObservationInput(t, f.store, in, generation)
	assert.Equal("reinforced", repeat.Items[0].Outcome)
	assert.Equal(first, repeat.Items[0].ObservationID)
	detail, err := f.store.GetPersonObservation(t.Context(), first)
	require.NoError(err)
	assert.Equal(1, detail.MentionCount)
	assert.Equal("[PERSON] runs", detail.Content)
	assert.Contains(detail.Events[len(detail.Events)-1].Detail, "quote")
	second := f.insertMessage(t, "observation-second", "email", f.aliceID, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, second, "I run too")
	item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, second)
	evidence := sweepEvidenceInput(t, item)
	offset := strings.Index(evidence.Excerpt, "I run too")
	in.Proposals[0].Support = []personobs.SupportInput{{Evidence: evidence, Quote: "I run too", QuoteStart: len([]rune(evidence.Excerpt[:offset]))}}
	applyObservationInput(t, f.store, in, generation)
	detail, err = f.store.GetPersonObservation(t.Context(), first)
	require.NoError(err)
	assert.Equal(2, detail.MentionCount)
	assert.Equal("[PERSON] runs", detail.Content)
}
func TestApplyObservationBatchSupersedeBlockedByPinOrConfirmed(t *testing.T) {
	for _, column := range []string{"pinned", "owner_state"} {
		t.Run(column, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			first := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			value := any(true)
			if column == "owner_state" {
				value = "confirmed"
			}
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET `+column+`=? WHERE id=?`), value, first)
			require.NoError(err)
			in.Proposals[0].Content = "[PERSON] runs every morning"
			in.Proposals[0].SupersedesIDs = []int64{first}
			result := applyObservationInput(t, f.store, in, generation)
			require.Len(result.Items, 1)
			assert.Contains(result.Items[0].AffectedIDs, first)
			detail, err := f.store.GetPersonObservation(t.Context(), first)
			require.NoError(err)
			assert.Equal(personobs.StatusActive, detail.Status)
			var blocked int
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_events WHERE event='supersede-blocked' AND observation_id=?`), first).Scan(&blocked))
			assert.Equal(1, blocked)
		})
	}
}
func TestApplyObservationBatchDropsInvalidWithoutAbort(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	invalid := in.Proposals[0]
	invalid.Content = "no person token"
	in.Proposals = append(in.Proposals, invalid)
	result := applyObservationInput(t, f.store, in, generation)
	require.Len(t, result.Items, 2)
	assert.Equal(t, "created", result.Items[0].Outcome)
	assert.Equal(t, "dropped-invalid", result.Items[1].Outcome)
}
func TestApplyObservationBatchRejectsGenerationOfOtherPerson(t *testing.T) {
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	other := insertPersonFactSchemaGeneration(t, f.store, f.bobPersonID, "other")
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	_, err = f.store.ApplyPersonObservationBatch(t.Context(), batch, other, "owner")
	require.Error(err)
	var count int
	require.NoError(f.store.DB().QueryRow(`SELECT COUNT(*) FROM person_observations`).Scan(&count))
	assert.Zero(t, count)
}
func TestApplyObservationBatchAppendsPremiseToOlderDerived(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	first := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	derived := in
	derived.Proposals = []personobs.ProposedObservation{{PersonID: in.PersonID, Level: personobs.LevelInductive, Origin: personobs.OriginReasoning, Kind: personobs.KindPattern, Content: "[PERSON] runs often", ConfidenceBP: 800, Premises: []personobs.PremiseInput{{ID: first, Role: personobs.PremiseRoleSupport}}}}
	in.Proposals[0].Content = "[PERSON] swims"
	second := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	derived.Proposals[0].Premises = append(derived.Proposals[0].Premises, personobs.PremiseInput{ID: second, Role: personobs.PremiseRoleSupport})
	id := applyObservationInput(t, f.store, derived, generation).Items[0].ObservationID
	in.Proposals[0].Content = "[PERSON] cycles"
	third := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	assert.Greater(third, id)
	derived.Proposals[0].Premises = append(derived.Proposals[0].Premises, personobs.PremiseInput{ID: third, Role: personobs.PremiseRoleSupport})
	derived.Proposals[0].ReinforcesID = &id
	result := applyObservationInput(t, f.store, derived, generation)
	assert.Equal("reinforced", result.Items[0].Outcome)
	detail, err := f.store.GetPersonObservation(t.Context(), id)
	require.NoError(err)
	assert.Len(detail.Premises, 3)
}
func TestAppendPremiseRejectsDeductiveCycle(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	first := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	derived := in
	derived.Proposals = []personobs.ProposedObservation{{PersonID: in.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning, Kind: personobs.KindState, Content: "[PERSON] exercises", ConfidenceBP: 800, Premises: []personobs.PremiseInput{{ID: first, Role: personobs.PremiseRoleSupport}}}}
	older := applyObservationInput(t, f.store, derived, generation).Items[0].ObservationID
	derived.Proposals[0].Content = "[PERSON] keeps fit"
	derived.Proposals[0].Premises = []personobs.PremiseInput{{ID: older, Role: personobs.PremiseRoleSupport}}
	newer := applyObservationInput(t, f.store, derived, generation).Items[0].ObservationID
	derived.Proposals[0].Content = "[PERSON] exercises"
	derived.Proposals[0].Premises = []personobs.PremiseInput{{ID: newer, Role: personobs.PremiseRoleSupport}}
	derived.Proposals[0].ReinforcesID = &older
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), derived)
	require.NoError(t, err)
	_, err = f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "owner")
	require.ErrorContains(t, err, "premise-order")
}
func TestObservationSequenceNeverReusedAfterForget(t *testing.T) {
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	first := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	detail, err := f.store.GetPersonObservation(t.Context(), first)
	require.NoError(err)
	require.NoError(f.store.DeletePersonObservationForTest(t.Context(), first))
	in.Proposals[0].Content = "[PERSON] swims"
	next := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	later, err := f.store.GetPersonObservation(t.Context(), next)
	require.NoError(err)
	assert.Greater(t, later.Sequence, detail.Sequence)
}
func TestDecideSuppressionOutdatedLiftsOnNewSupport(t *testing.T) {
	testObservationSuppression(t, "outdated", personobs.OriginExtraction, 3, 3, "lifted")
}
func TestDecideSuppressionWrongNeverLifts(t *testing.T) {
	testObservationSuppression(t, "wrong", personobs.OriginExtraction, 100, 3, "suppressed")
}
func TestDecideSuppressionOwnerAllows(t *testing.T) {
	testObservationSuppression(t, "wrong", personobs.OriginOwner, 0, 3, "allow")
}
func testObservationSuppression(t *testing.T, reason string, origin personobs.Origin, support, floor int, want string) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	key := personobs.ContentKey(personobs.LevelExplicit, "", in.Proposals[0].Content)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor) VALUES(?,?,?,'owner')`), in.PersonID, key, reason)
	require.NoError(err)
	got, err := f.store.DecideObservationSuppressionForTest(t.Context(), in.PersonID, key, store.SuppressionCandidate{Origin: origin, PostRejectionSupport: support, Floor: floor})
	require.NoError(err)
	assert.Equal(want, string(got))
	var active int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND cleared_at IS NULL`), in.PersonID).Scan(&active))
	if want == "lifted" {
		assert.Zero(active)
	} else {
		assert.Equal(1, active)
	}
}
func TestApplyObservationBatchNeverWritesClaims(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	applyObservationInput(t, f.store, in, generation)
	var claims int
	require.NoError(t, f.store.DB().QueryRow(`SELECT COUNT(*) FROM person_fact_claims`).Scan(&claims))
	assert.Zero(t, claims)
}

func TestApplyObservationBatchRejectsGenerationOfOtherProgram(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(t, err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_fact_generations SET program_fingerprint=? WHERE id=?`), strings.Repeat("b", 64), generation)
	require.NoError(t, err)
	_, err = f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "owner")
	require.ErrorContains(t, err, "program")
}

func TestApplyObservationBatchInvalidEvidencePreservesSiblings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	bad := in.Proposals[0]
	bad.Support = append([]personobs.SupportInput(nil), bad.Support...)
	bad.Support[0].Evidence.SourceURL = "http://example.com"
	bad.Content = "[PERSON] swims"
	in.Proposals = append(in.Proposals, bad)
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	assert.NotNil(batch.Items[1].Failure)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
	require.NoError(err)
	require.Len(result.Items, 2)
	assert.Equal("created", result.Items[0].Outcome)
	assert.Equal("dropped-invalid", result.Items[1].Outcome)
}
func TestApplyObservationBatchInvalidTargetPreservesSuppression(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	key := personobs.ContentKey(personobs.LevelExplicit, "", in.Proposals[0].Content)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at) VALUES(?,?,'outdated','owner',?)`), in.PersonID, key, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(err)
	missing := int64(999999)
	in.Proposals[0].ReinforcesID = &missing
	result := applyObservationInput(t, f.store, in, generation)
	assert.Equal("dropped-invalid", result.Items[0].Outcome)
	var active int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND cleared_at IS NULL`), in.PersonID).Scan(&active))
	assert.Equal(1, active)
}
func TestApplyObservationBatchEventsUseTriggeringGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, firstGeneration := observationBatchFixture(t, "I run")
	first := applyObservationInput(t, f.store, in, firstGeneration).Items[0].ObservationID
	secondGeneration := insertPersonFactSchemaGeneration(t, f.store, in.PersonID, "second-observation-generation")
	applyObservationInput(t, f.store, in, secondGeneration)
	in.Proposals[0].Content = "[PERSON] swims"
	in.Proposals[0].SupersedesIDs = []int64{first}
	applyObservationInput(t, f.store, in, secondGeneration)
	for _, event := range []string{"reinforced", "superseded"} {
		var generation int64
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT generation_id FROM person_observation_events WHERE observation_id=? AND event=?`), first, event).Scan(&generation))
		assert.Equal(secondGeneration, generation)
	}
	require.NoError(f.store.RejectObservationForTest(t.Context(), first))
	var ownerGeneration *int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT generation_id FROM person_observation_events WHERE observation_id=? AND event='rejected'`), first).Scan(&ownerGeneration))
	assert.Nil(ownerGeneration)
}
func TestApplyObservationBatchSupersededTargetFallsThrough(t *testing.T) {
	for _, withLive := range []bool{false, true} {
		t.Run(strconv.FormatBool(withLive), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			old := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			in.Proposals[0].ReinforcesID = &old
			batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
			require.NoError(err)
			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET status='superseded' WHERE id=?`), old)
			require.NoError(err)
			var live int64
			if withLive {
				replacement := in
				replacement.Proposals = append([]personobs.ProposedObservation(nil), in.Proposals...)
				replacement.Proposals[0].ReinforcesID = nil
				live = applyObservationInput(t, f.store, replacement, generation).Items[0].ObservationID
			}
			result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
			require.NoError(err)
			if withLive {
				assert.Equal("reinforced", result.Items[0].Outcome)
				assert.Equal(live, result.Items[0].ObservationID)
			} else {
				assert.Equal("created", result.Items[0].Outcome)
				assert.NotEqual(old, result.Items[0].ObservationID)
			}
		})
	}
}
func TestApplyObservationBatchPreparedIntegrityAndIsolation(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(t, err)
	in.Generation.PersonID = f.bobPersonID
	in.Proposals[0].Support[0].Evidence.Excerpt = "changed"
	_, err = f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
	require.NoError(t, err)
	batch.Items[0].Proposal.Content = "[PERSON] altered"
	_, err = f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
	require.ErrorContains(t, err, "integrity")
}

func TestApplyObservationBatchPreservesExplicitSensitivity(t *testing.T) {
	t.Run("insert", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f, in, generation := observationBatchFixture(t, "I run")
		in.Proposals[0].Sensitive = true

		result := applyObservationInput(t, f.store, in, generation)
		detail, err := f.store.GetPersonObservation(t.Context(), result.Items[0].ObservationID)
		require.NoError(err)
		assert.True(detail.Sensitive)
		page, err := f.store.ListPersonObservations(t.Context(), in.PersonID, personobs.Filter{Limit: 20})
		require.NoError(err)
		assert.Empty(page.Items)
	})

	t.Run("reinforce", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f, in, generation := observationBatchFixture(t, "I run")
		id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
		in.Proposals[0].Sensitive = true

		result := applyObservationInput(t, f.store, in, generation)
		assert.Equal("reinforced", result.Items[0].Outcome)
		detail, err := f.store.GetPersonObservation(t.Context(), id)
		require.NoError(err)
		assert.True(detail.Sensitive)
		page, err := f.store.ListPersonObservations(t.Context(), in.PersonID, personobs.Filter{Limit: 20})
		require.NoError(err)
		assert.Empty(page.Items)
	})
}

func TestApplyObservationBatchUsesSupportCategoriesReloadedUnderLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	premiseID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID

	reinforcement := in.Proposals[0]
	reinforcement.ModelCategories = []personobs.Category{personobs.CategoryHealth}
	derived := personobs.ProposedObservation{
		PersonID:     in.PersonID,
		Level:        personobs.LevelDeductive,
		Origin:       personobs.OriginReasoning,
		Kind:         personobs.KindState,
		Content:      "[PERSON] has a documented status",
		ConfidenceBP: 800,
		Premises:     []personobs.PremiseInput{{ID: premiseID, Role: personobs.PremiseRoleSupport}},
	}
	in.Proposals = []personobs.ProposedObservation{reinforcement, derived}

	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Len(batch.Items, 2)
	assert.Empty(batch.Items[1].Categories)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
	require.NoError(err)
	require.Len(result.Items, 2)
	derivedDetail, err := f.store.GetPersonObservation(t.Context(), result.Items[1].ObservationID)
	require.NoError(err)
	assert.Contains(derivedDetail.Categories, personobs.CategoryHealth)
	assert.True(derivedDetail.Sensitive)
}

func TestPrepareObservationInheritsCounterPremiseSensitivity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	supportID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	counterSupport, _ := observationSupportForMessage(t, f, "observation-counter", "I do not run", "I do not run", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	counter := in.Proposals[0]
	counter.Content = "[PERSON] does not run"
	counter.Support = []personobs.SupportInput{counterSupport}
	counter.ModelCategories = []personobs.Category{personobs.CategoryHealth}
	counterInput := in
	counterInput.Proposals = []personobs.ProposedObservation{counter}
	counterID := applyObservationInput(t, f.store, counterInput, generation).Items[0].ObservationID

	in.Proposals = []personobs.ProposedObservation{{
		PersonID:     in.PersonID,
		Level:        personobs.LevelContradiction,
		Origin:       personobs.OriginReasoning,
		Kind:         personobs.KindOther,
		Content:      "[PERSON] has conflicting reports",
		ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{
			{ID: supportID, Role: personobs.PremiseRoleSupport},
			{ID: counterID, Role: personobs.PremiseRoleCounter},
		},
	}}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Len(batch.Items, 1)
	require.Nil(batch.Items[0].Failure)
	assert.Contains(batch.Items[0].Categories, personobs.CategoryHealth)
	assert.True(batch.Items[0].Proposal.Sensitive)
}

func TestApplyObservationBatchUsesCounterCategoriesReloadedUnderLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	supportID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	counterSupport, _ := observationSupportForMessage(t, f, "observation-counter", "I do not run", "I do not run", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	counter := in.Proposals[0]
	counter.Content = "[PERSON] does not run"
	counter.Support = []personobs.SupportInput{counterSupport}
	counterInput := in
	counterInput.Proposals = []personobs.ProposedObservation{counter}
	counterID := applyObservationInput(t, f.store, counterInput, generation).Items[0].ObservationID
	counter.ModelCategories = []personobs.Category{personobs.CategoryHealth}
	counter.ReinforcesID = &counterID

	derived := personobs.ProposedObservation{
		PersonID:     in.PersonID,
		Level:        personobs.LevelContradiction,
		Origin:       personobs.OriginReasoning,
		Kind:         personobs.KindOther,
		Content:      "[PERSON] has conflicting reports",
		ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{
			{ID: supportID, Role: personobs.PremiseRoleSupport},
			{ID: counterID, Role: personobs.PremiseRoleCounter},
		},
	}
	in.Proposals = []personobs.ProposedObservation{counter, derived}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Len(batch.Items, 2)
	require.Nil(batch.Items[1].Failure)
	assert.Empty(batch.Items[1].Categories)
	assert.False(batch.Items[1].Proposal.Sensitive)

	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "reasoner")
	require.NoError(err)
	require.Len(result.Items, 2)
	assert.Equal("reinforced", result.Items[0].Outcome)
	assert.Equal("created", result.Items[1].Outcome)
	detail, err := f.store.GetPersonObservation(t.Context(), result.Items[1].ObservationID)
	require.NoError(err)
	assert.Contains(detail.Categories, personobs.CategoryHealth)
	assert.True(detail.Sensitive)
}

func TestApplyObservationBatchOwnerAssertionPromotesUnsupportedExtraction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	extractionID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET status='unsupported',status_reason='evidence-unsupported' WHERE id=?`), extractionID)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observation_evidence SET exclusion_reason='hidden_text' WHERE observation_id=?`), extractionID)
	require.NoError(err)

	ownerInput := personobs.BatchInput{
		PersonID: in.PersonID,
		Proposals: []personobs.ProposedObservation{{
			PersonID:     in.PersonID,
			Level:        personobs.LevelExplicit,
			Origin:       personobs.OriginOwner,
			Kind:         personobs.KindState,
			Content:      in.Proposals[0].Content,
			ConfidenceBP: 1000,
		}},
	}
	ownerBatch, err := f.store.PreparePersonObservationBatch(t.Context(), ownerInput)
	require.NoError(err)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), ownerBatch, 0, "owner")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("reinforced", result.Items[0].Outcome)
	assert.Equal(extractionID, result.Items[0].ObservationID)

	detail, err := f.store.GetPersonObservation(t.Context(), extractionID)
	require.NoError(err)
	assert.Equal(personobs.OriginOwner, detail.Origin)
	assert.Equal(personobs.StatusActive, detail.Status)
	assert.Empty(detail.StatusReason)
	assert.Empty(detail.SupportFlag)
	assert.Equal(personfacts.DirectOther, detail.Directness)
	assert.Nil(detail.GenerationID)
	assert.Empty(detail.ProgramFingerprint)
	assert.Zero(detail.SupportedCount)
	assert.True(personobs.PremiseSupported(detail.Observation))
	require.Len(detail.Evidence, 1)
	assert.Equal("created", detail.Events[0].Event)
	assert.Contains(detail.Events[len(detail.Events)-1].Detail, "owner_assertion")
}

func TestApplyObservationBatchNormalizesValidityForAsOfReads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	west := time.FixedZone("west", -7*60*60)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, west)
	in.Proposals[0].ValidFrom = &from

	result := applyObservationInput(t, f.store, in, generation)
	id := result.Items[0].ObservationID
	detail, err := f.store.GetPersonObservation(t.Context(), id)
	require.NoError(err)
	require.NotNil(detail.ValidFrom)
	assert.Equal(from.UTC().Truncate(time.Microsecond), *detail.ValidFrom)

	beforeStart := time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)
	page, err := f.store.ListPersonObservations(t.Context(), in.PersonID, personobs.Filter{AsOf: &beforeStart, Limit: 20})
	require.NoError(err)
	assert.Empty(page.Items)
	afterStart := time.Date(2026, 10, 1, 7, 30, 0, 0, time.UTC)
	page, err = f.store.ListPersonObservations(t.Context(), in.PersonID, personobs.Filter{AsOf: &afterStart, Limit: 20})
	require.NoError(err)
	require.Len(page.Items, 1)
	assert.Equal(id, page.Items[0].ID)
}

func TestApplyObservationBatchRetainsEarlierValidityEndOnSupersede(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	existingEnd := from.Add(10 * 24 * time.Hour)
	in.Proposals[0].ValidFrom = &from
	in.Proposals[0].ValidUntil = &existingEnd
	oldID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID

	replacement := in.Proposals[0]
	replacement.Content = "[PERSON] runs every morning"
	replacementFrom := existingEnd.Add(10 * 24 * time.Hour)
	replacement.ValidFrom = &replacementFrom
	replacement.ValidUntil = nil
	replacement.SupersedesIDs = []int64{oldID}
	in.Proposals = []personobs.ProposedObservation{replacement}
	result := applyObservationInput(t, f.store, in, generation)
	require.Len(result.Items, 1)
	assert.Equal("superseded-target", result.Items[0].Outcome)

	oldDetail, err := f.store.GetPersonObservation(t.Context(), oldID)
	require.NoError(err)
	require.NotNil(oldDetail.ValidUntil)
	assert.Equal(existingEnd, *oldDetail.ValidUntil)
}

func TestApplyObservationBatchLiftsOutdatedContradictionWithCounterEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	supportID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	counterInput := in
	counterInput.Proposals = append([]personobs.ProposedObservation(nil), in.Proposals...)
	counterInput.Proposals[0].Content = "[PERSON] does not run"
	counterID := applyObservationInput(t, f.store, counterInput, generation).Items[0].ObservationID

	content := "[PERSON] has conflicting reports"
	key := personobs.ContentKey(personobs.LevelContradiction, "", content)
	rejectedAt := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at) VALUES(?,?,'outdated','owner',?)`), in.PersonID, key, rejectedAt)
	require.NoError(err)

	contradictionInput := in
	contradictionInput.Proposals = []personobs.ProposedObservation{{
		PersonID:     in.PersonID,
		Level:        personobs.LevelContradiction,
		Origin:       personobs.OriginReasoning,
		Kind:         personobs.KindOther,
		Content:      content,
		ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{
			{ID: supportID, Role: personobs.PremiseRoleSupport},
			{ID: counterID, Role: personobs.PremiseRoleCounter},
		},
	}}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), contradictionInput)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "reasoner")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("created", result.Items[0].Outcome)

	var active int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), in.PersonID, key).Scan(&active))
	assert.Zero(active)
}

func TestApplyObservationBatchDoesNotSupersedeWithExcludedReplacementEvidence(t *testing.T) {
	for _, exclusion := range []string{"list", "draft"} {
		t.Run(exclusion, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			oldID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			replacement := in.Proposals[0]
			replacement.Content = "[PERSON] runs every morning"
			replacement.SupersedesIDs = []int64{oldID}
			in.Proposals = []personobs.ProposedObservation{replacement}
			batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
			require.NoError(err)

			ref, err := peoplesweep.DecodePersonSweepEvidenceRef(replacement.Support[0].Evidence.SourceRef)
			require.NoError(err)
			var query string
			var value any
			if exclusion == "list" {
				query = `UPDATE messages SET list_id=? WHERE id=?`
				value = "newsletter.example.test"
			} else {
				query = `UPDATE messages SET draft_authored=? WHERE id=?`
				value = true
			}
			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(query), value, ref.MessageID)
			require.NoError(err)

			result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
			require.NoError(err)
			require.Len(result.Items, 1)
			assert.Equal("created", result.Items[0].Outcome)
			assert.Empty(result.Items[0].AffectedIDs)
			old, err := f.store.GetPersonObservation(t.Context(), oldID)
			require.NoError(err)
			assert.Equal(personobs.StatusActive, old.Status)
			assert.Nil(old.SupersededByID)
			replacementDetail, err := f.store.GetPersonObservation(t.Context(), result.Items[0].ObservationID)
			require.NoError(err)
			assert.Zero(replacementDetail.SupportedCount)
			require.Len(replacementDetail.Evidence, 1)
			wantExclusion := personobs.ExclusionReasonList
			if exclusion == "draft" {
				wantExclusion = personobs.ExclusionReasonDraft
			}
			assert.Equal(wantExclusion, replacementDetail.Evidence[0].Exclusion)
		})
	}
}

func TestApplyObservationBatchUsesSupportedEvidenceForSupersessionDate(t *testing.T) {
	for _, exclusion := range []string{"list", "draft", "withdrawn"} {
		t.Run(exclusion, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			oldID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID

			eligibleAt := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
			excludedAt := eligibleAt.Add(time.Hour)
			eligible, _ := observationSupportForMessage(t, f, "observation-eligible", "I run today", "I run today", eligibleAt)
			excluded, excludedMessageID := observationSupportForMessage(t, f, "observation-excluded", "I run tomorrow", "I run tomorrow", excludedAt)
			var evidenceHolderID int64
			if exclusion == "withdrawn" {
				holderInput := in
				holderInput.Proposals = append([]personobs.ProposedObservation(nil), in.Proposals...)
				holderInput.Proposals[0].Content = "[PERSON] runs tomorrow"
				holderInput.Proposals[0].Support = []personobs.SupportInput{excluded}
				evidenceHolderID = applyObservationInput(t, f.store, holderInput, generation).Items[0].ObservationID
			}
			replacement := in.Proposals[0]
			replacement.Content = "[PERSON] runs every morning"
			replacement.Support = []personobs.SupportInput{eligible, excluded}
			replacement.SupersedesIDs = []int64{oldID}
			in.Proposals = []personobs.ProposedObservation{replacement}
			batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
			require.NoError(err)

			if exclusion == "withdrawn" {
				withdrawObservationEvidence(t, f.store, in.PersonID, evidenceHolderID, excludedMessageID)
			} else {
				var query string
				var value any
				if exclusion == "list" {
					query = `UPDATE messages SET list_id=? WHERE id=?`
					value = "newsletter.example.test"
				} else {
					query = `UPDATE messages SET draft_authored=? WHERE id=?`
					value = true
				}
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(query), value, excludedMessageID)
				require.NoError(err)
			}

			result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
			require.NoError(err)
			require.Len(result.Items, 1)
			assert.Equal("superseded-target", result.Items[0].Outcome)

			old, err := f.store.GetPersonObservation(t.Context(), oldID)
			require.NoError(err)
			require.NotNil(old.ValidUntil)
			assert.Equal(eligibleAt, *old.ValidUntil)
			replacementDetail, err := f.store.GetPersonObservation(t.Context(), result.Items[0].ObservationID)
			require.NoError(err)
			require.Len(replacementDetail.Evidence, 2)
			assert.Equal(personobs.ExclusionReasonNone, replacementDetail.Evidence[0].Exclusion)
			wantExclusion := personobs.ExclusionReasonNone
			switch exclusion {
			case "list":
				wantExclusion = personobs.ExclusionReasonList
			case "draft":
				wantExclusion = personobs.ExclusionReasonDraft
			}
			assert.Equal(wantExclusion, replacementDetail.Evidence[1].Exclusion)
		})
	}
}

func TestApplyObservationBatchRepairsExistingCitationExclusion(t *testing.T) {
	for _, exclusion := range []string{"list", "draft"} {
		t.Run(exclusion, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
			require.NoError(err)

			ref, err := peoplesweep.DecodePersonSweepEvidenceRef(in.Proposals[0].Support[0].Evidence.SourceRef)
			require.NoError(err)
			var query string
			var value any
			if exclusion == "list" {
				query = `UPDATE messages SET list_id=? WHERE id=?`
				value = "newsletter.example.test"
			} else {
				query = `UPDATE messages SET draft_authored=? WHERE id=?`
				value = true
			}
			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(query), value, ref.MessageID)
			require.NoError(err)

			result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
			require.NoError(err)
			require.Len(result.Items, 1)
			assert.Equal("reinforced", result.Items[0].Outcome)
			assert.Equal(id, result.Items[0].ObservationID)

			detail, err := f.store.GetPersonObservation(t.Context(), id)
			require.NoError(err)
			assert.Zero(detail.MentionCount)
			assert.Zero(detail.SupportedCount)
			assert.Nil(detail.LastSeenAt)
			require.Len(detail.Evidence, 1)
			wantExclusion := personobs.ExclusionReasonList
			if exclusion == "draft" {
				wantExclusion = personobs.ExclusionReasonDraft
			}
			assert.Equal(wantExclusion, detail.Evidence[0].Exclusion)
		})
	}
}

func TestApplyObservationBatchRefreshesExclusionBeforeLiftingSuppression(t *testing.T) {
	for _, exclusion := range []string{"list", "draft"} {
		t.Run(exclusion, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			ref, err := peoplesweep.DecodePersonSweepEvidenceRef(in.Proposals[0].Support[0].Evidence.SourceRef)
			require.NoError(err)
			setExcluded := func(excluded bool) {
				if exclusion == "list" {
					listID := ""
					if excluded {
						listID = "newsletter.example.test"
					}
					_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id=? WHERE id=?`), listID, ref.MessageID)
				} else {
					_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET draft_authored=? WHERE id=?`), excluded, ref.MessageID)
				}
				require.NoError(err)
			}
			setExcluded(true)

			proposal := in.Proposals[0]
			key := personobs.ContentKey(proposal.Level, proposal.SubjectRef, proposal.Content)
			rejectedAt := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at) VALUES(?,?,'outdated','owner',?)`), in.PersonID, key, rejectedAt)
			require.NoError(err)

			batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
			require.NoError(err)
			require.Len(batch.Items, 1)
			wantExclusion := personobs.ExclusionReasonList
			if exclusion == "draft" {
				wantExclusion = personobs.ExclusionReasonDraft
			}
			require.Equal(wantExclusion, batch.Items[0].Citations[0].Exclusion)

			setExcluded(false)
			result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
			require.NoError(err)
			require.Len(result.Items, 1)
			require.Equal("created", result.Items[0].Outcome)
			detail, err := f.store.GetPersonObservation(t.Context(), result.Items[0].ObservationID)
			require.NoError(err)
			require.Len(detail.Evidence, 1)
			assert.Equal(personobs.ExclusionReasonNone, detail.Evidence[0].Exclusion)
			assert.Equal(1, detail.MentionCount)
			assert.Equal(1, detail.SupportedCount)
			assert.NotNil(detail.LastSeenAt)
		})
	}
}

func TestApplyObservationBatchDoesNotLiftSuppressionOnEvidenceExcludedAfterPreparation(t *testing.T) {
	for _, exclusion := range []string{"list", "draft"} {
		t.Run(exclusion, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			ref, err := peoplesweep.DecodePersonSweepEvidenceRef(in.Proposals[0].Support[0].Evidence.SourceRef)
			require.NoError(err)
			key := personobs.ContentKey(in.Proposals[0].Level, in.Proposals[0].SubjectRef, in.Proposals[0].Content)
			rejectedAt := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at) VALUES(?,?,'outdated','owner',?)`), in.PersonID, key, rejectedAt)
			require.NoError(err)

			batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
			require.NoError(err)
			if exclusion == "list" {
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id=? WHERE id=?`), "newsletter.example.test", ref.MessageID)
			} else {
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET draft_authored=TRUE WHERE id=?`), ref.MessageID)
			}
			require.NoError(err)

			result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
			require.NoError(err)
			require.Len(result.Items, 1)
			assert.Equal("suppressed", result.Items[0].Outcome)

			var active int
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), in.PersonID, key).Scan(&active))
			assert.Equal(1, active)
			detail, err := f.store.GetPersonObservation(t.Context(), id)
			require.NoError(err)
			assert.Zero(detail.MentionCount)
			require.Len(detail.Evidence, 1)
			wantExclusion := personobs.ExclusionReasonList
			if exclusion == "draft" {
				wantExclusion = personobs.ExclusionReasonDraft
			}
			assert.Equal(wantExclusion, detail.Evidence[0].Exclusion)
		})
	}
}

func TestApplyObservationBatchRejectsPremiseWhenCitationBecomesExcluded(t *testing.T) {
	for _, exclusion := range []string{"list", "draft"} {
		t.Run(exclusion, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			premiseID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID

			targetInput := in
			targetInput.Proposals = append([]personobs.ProposedObservation(nil), in.Proposals...)
			targetInput.Proposals[0].Content = "[PERSON] runs every morning"
			targetID := applyObservationInput(t, f.store, targetInput, generation).Items[0].ObservationID

			derivedInput := in
			derivedInput.Proposals = []personobs.ProposedObservation{{
				PersonID:      in.PersonID,
				Level:         personobs.LevelDeductive,
				Origin:        personobs.OriginReasoning,
				Kind:          personobs.KindOther,
				Content:       "[PERSON] has a running routine",
				ConfidenceBP:  800,
				Premises:      []personobs.PremiseInput{{ID: premiseID, Role: personobs.PremiseRoleSupport}},
				SupersedesIDs: []int64{targetID},
			}}
			batch, err := f.store.PreparePersonObservationBatch(t.Context(), derivedInput)
			require.NoError(err)
			require.Nil(batch.Items[0].Failure)

			ref, err := peoplesweep.DecodePersonSweepEvidenceRef(in.Proposals[0].Support[0].Evidence.SourceRef)
			require.NoError(err)
			if exclusion == "list" {
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id=? WHERE id=?`), "newsletter.example.test", ref.MessageID)
			} else {
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET draft_authored=TRUE WHERE id=?`), ref.MessageID)
			}
			require.NoError(err)

			result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
			require.NoError(err)
			require.Len(result.Items, 1)
			assert.Equal("dropped-invalid", result.Items[0].Outcome)
			require.NotNil(result.Items[0].Failure)
			assert.Equal("premise-not-supported", result.Items[0].Failure.Reason)

			premise, err := f.store.GetPersonObservation(t.Context(), premiseID)
			require.NoError(err)
			require.Len(premise.Evidence, 1)
			assert.Equal(personobs.StatusActive, premise.Status)
			assert.Zero(premise.SupportedCount)
			wantExclusion := personobs.ExclusionReasonList
			if exclusion == "draft" {
				wantExclusion = personobs.ExclusionReasonDraft
			}
			assert.Equal(wantExclusion, premise.Evidence[0].Exclusion)
			target, err := f.store.GetPersonObservation(t.Context(), targetID)
			require.NoError(err)
			assert.Equal(personobs.StatusActive, target.Status)
			assert.Nil(target.SupersededByID)
		})
	}
}

func TestApplyObservationBatchRevalidatesPremiseAfterListExclusionRemoved(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	premiseID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	ref, err := peoplesweep.DecodePersonSweepEvidenceRef(in.Proposals[0].Support[0].Evidence.SourceRef)
	require.NoError(err)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id=? WHERE id=?`), "newsletter.example.test", ref.MessageID)
	require.NoError(err)
	refreshed := applyObservationInput(t, f.store, in, generation)
	require.Len(refreshed.Items, 1)
	assert.Equal("reinforced", refreshed.Items[0].Outcome)
	detail, err := f.store.GetPersonObservation(t.Context(), premiseID)
	require.NoError(err)
	assert.Zero(detail.SupportedCount)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id=NULL WHERE id=?`), ref.MessageID)
	require.NoError(err)
	derived := in
	derived.Proposals = []personobs.ProposedObservation{{
		PersonID:     in.PersonID,
		Level:        personobs.LevelDeductive,
		Origin:       personobs.OriginReasoning,
		Kind:         personobs.KindOther,
		Content:      "[PERSON] has a running routine",
		ConfidenceBP: 800,
		Premises:     []personobs.PremiseInput{{ID: premiseID, Role: personobs.PremiseRoleSupport}},
	}}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), derived)
	require.NoError(err)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "reasoner")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("created", result.Items[0].Outcome)
	assert.Nil(result.Items[0].Failure)

	detail, err = f.store.GetPersonObservation(t.Context(), premiseID)
	require.NoError(err)
	assert.Equal(1, detail.SupportedCount)
}

func TestApplyObservationBatchDoesNotLiftSuppressionOnUnsupportedExplicitEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	holderID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	ref, err := peoplesweep.DecodePersonSweepEvidenceRef(in.Proposals[0].Support[0].Evidence.SourceRef)
	require.NoError(err)

	content := "[PERSON] runs every morning"
	key := personobs.ContentKey(personobs.LevelExplicit, "", content)
	rejectedAt := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at) VALUES(?,?,'outdated','owner',?)`), in.PersonID, key, rejectedAt)
	require.NoError(err)

	candidateInput := in
	candidateInput.Proposals = append([]personobs.ProposedObservation(nil), in.Proposals...)
	candidateInput.Proposals[0].Content = content
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), candidateInput)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	withdrawObservationEvidence(t, f.store, in.PersonID, holderID, ref.MessageID)

	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("suppressed", result.Items[0].Outcome)
	var active, observations int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), in.PersonID, key).Scan(&active))
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observations WHERE person_id=? AND content_key=?`), in.PersonID, key).Scan(&observations))
	assert.Equal(1, active)
	assert.Zero(observations)
}

func TestApplyObservationBatchDoesNotLiftSuppressionOnUnsupportedPremiseEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, runInput, generation := observationBatchFixture(t, "I run")
	runID := applyObservationInput(t, f.store, runInput, generation).Items[0].ObservationID

	supportForMessage := func(key, body, quote string, at time.Time) (personobs.SupportInput, int64) {
		messageID := f.insertMessage(t, key, "email", f.aliceID, at)
		addSweepBody(t, f, messageID, body)
		item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, messageID)
		evidence := sweepEvidenceInput(t, item)
		offset := strings.Index(evidence.Excerpt, quote)
		require.NotEqual(-1, offset)
		return personobs.SupportInput{Evidence: evidence, Quote: quote, QuoteStart: len([]rune(evidence.Excerpt[:offset]))}, messageID
	}

	swimInput := runInput
	swimInput.Proposals = append([]personobs.ProposedObservation(nil), runInput.Proposals...)
	swimInput.Proposals[0].Content = "[PERSON] swims"
	swimSupport, _ := supportForMessage("observation-swim", "I swim", "I swim", time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC))
	swimInput.Proposals[0].Support = []personobs.SupportInput{
		swimSupport,
	}
	swimID := applyObservationInput(t, f.store, swimInput, generation).Items[0].ObservationID

	rejectedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	runLaterSupport, runLaterMessageID := supportForMessage("observation-run-later", "I run still", "I run still", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	runInput.Proposals[0].Support = []personobs.SupportInput{
		runLaterSupport,
	}
	applyObservationInput(t, f.store, runInput, generation)
	swimLaterSupport, swimLaterMessageID := supportForMessage("observation-swim-later", "I swim still", "I swim still", time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC))
	swimInput.Proposals[0].Support = []personobs.SupportInput{
		swimLaterSupport,
	}
	applyObservationInput(t, f.store, swimInput, generation)

	content := "[PERSON] has two supported reports"
	key := personobs.ContentKey(personobs.LevelContradiction, "", content)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at) VALUES(?,?,'outdated','owner',?)`), runInput.PersonID, key, rejectedAt)
	require.NoError(err)

	contradictionInput := runInput
	contradictionInput.Proposals = []personobs.ProposedObservation{{
		PersonID:     runInput.PersonID,
		Level:        personobs.LevelContradiction,
		Origin:       personobs.OriginReasoning,
		Kind:         personobs.KindOther,
		Content:      content,
		ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{
			{ID: runID, Role: personobs.PremiseRoleSupport},
			{ID: swimID, Role: personobs.PremiseRoleSupport},
		},
	}}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), contradictionInput)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)

	statusGeneration := insertPersonFactSchemaGeneration(t, f.store, runInput.PersonID, "withdrawn-observation-evidence")
	for _, observation := range []struct {
		id        int64
		messageID int64
	}{{runID, runLaterMessageID}, {swimID, swimLaterMessageID}} {
		var evidenceID int64
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT evidence_id FROM person_observation_evidence WHERE observation_id=? AND message_id=?`), observation.id, observation.messageID).Scan(&evidenceID))
		var evidenceKey, sourceVersion string
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT evidence_key,source_version FROM person_fact_evidence WHERE id=?`), evidenceID).Scan(&evidenceKey, &sourceVersion))
		_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_fact_evidence_status_events(person_id,generation_id,evidence_id,evidence_key,source_version,supported,reason) VALUES(?,?,?,?,?,FALSE,?)`), runInput.PersonID, statusGeneration, evidenceID, evidenceKey, sourceVersion, personfacts.EvidenceStatusSourceDeleted)
		require.NoError(err)
	}

	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "reasoner")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("suppressed", result.Items[0].Outcome)
	var active int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), runInput.PersonID, key).Scan(&active))
	assert.Equal(1, active)

	applyObservationInput(t, f.store, runInput, generation)
	refreshed, err := f.store.GetPersonObservation(t.Context(), runID)
	require.NoError(err)
	require.NotNil(refreshed.LastSeenAt)
	assert.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), *refreshed.LastSeenAt)
}

func TestApplyObservationBatchLiftsSuppressionThroughDerivedPremise(t *testing.T) {
	for _, tt := range []struct {
		name              string
		withdrawFreshLeaf bool
		wantOutcome       string
		wantActive        int
	}{
		{name: "supported", wantOutcome: "created"},
		{name: "withdrawn", withdrawFreshLeaf: true, wantOutcome: "suppressed", wantActive: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			leafID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			firstDerivedInput := in
			firstDerivedInput.Proposals = []personobs.ProposedObservation{{
				PersonID:     in.PersonID,
				Level:        personobs.LevelDeductive,
				Origin:       personobs.OriginReasoning,
				Kind:         personobs.KindOther,
				Content:      "[PERSON] has a running routine",
				ConfidenceBP: 800,
				Premises:     []personobs.PremiseInput{{ID: leafID, Role: personobs.PremiseRoleSupport}},
			}}
			firstDerivedID := applyObservationInput(t, f.store, firstDerivedInput, generation).Items[0].ObservationID
			secondDerivedInput := in
			secondDerivedInput.Proposals = []personobs.ProposedObservation{{
				PersonID:     in.PersonID,
				Level:        personobs.LevelDeductive,
				Origin:       personobs.OriginReasoning,
				Kind:         personobs.KindOther,
				Content:      "[PERSON] follows a running routine",
				ConfidenceBP: 800,
				Premises:     []personobs.PremiseInput{{ID: firstDerivedID, Role: personobs.PremiseRoleSupport}},
			}}
			secondDerivedID := applyObservationInput(t, f.store, secondDerivedInput, generation).Items[0].ObservationID
			derived, err := f.store.GetPersonObservation(t.Context(), secondDerivedID)
			require.NoError(err)
			assert.Nil(derived.LastSeenAt)

			content := "[PERSON] maintains a running routine"
			key := personobs.ContentKey(personobs.LevelDeductive, "", content)
			rejectedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at) VALUES(?,?,'outdated','owner',?)`), in.PersonID, key, rejectedAt)
			require.NoError(err)

			freshAt := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
			freshSupport, freshMessageID := observationSupportForMessage(t, f, "observation-fresh-leaf", "I run today", "I run today", freshAt)
			in.Proposals[0].Support = []personobs.SupportInput{freshSupport}
			applyObservationInput(t, f.store, in, generation)

			candidateInput := in
			candidateInput.Proposals = []personobs.ProposedObservation{{
				PersonID:     in.PersonID,
				Level:        personobs.LevelDeductive,
				Origin:       personobs.OriginReasoning,
				Kind:         personobs.KindOther,
				Content:      content,
				ConfidenceBP: 800,
				Premises:     []personobs.PremiseInput{{ID: secondDerivedID, Role: personobs.PremiseRoleSupport}},
			}}
			batch, err := f.store.PreparePersonObservationBatch(t.Context(), candidateInput)
			require.NoError(err)
			require.Nil(batch.Items[0].Failure)
			if tt.withdrawFreshLeaf {
				withdrawObservationEvidence(t, f.store, in.PersonID, leafID, freshMessageID)
			}

			result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "reasoner")
			require.NoError(err)
			require.Len(result.Items, 1)
			assert.Equal(tt.wantOutcome, result.Items[0].Outcome)
			var active int
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), in.PersonID, key).Scan(&active))
			assert.Equal(tt.wantActive, active)
			if tt.wantOutcome == "created" {
				candidate, err := f.store.GetPersonObservation(t.Context(), result.Items[0].ObservationID)
				require.NoError(err)
				assert.Nil(candidate.LastSeenAt)
			}
		})
	}
}

func TestApplyObservationBatchDoesNotLiftSuppressionOnExcludedPremiseEvidence(t *testing.T) {
	for _, exclusion := range []string{"list", "draft"} {
		t.Run(exclusion, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			supportID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			counterInput := in
			counterInput.Proposals = append([]personobs.ProposedObservation(nil), in.Proposals...)
			counterInput.Proposals[0].Content = "[PERSON] does not run"
			counterID := applyObservationInput(t, f.store, counterInput, generation).Items[0].ObservationID
			ref, err := peoplesweep.DecodePersonSweepEvidenceRef(in.Proposals[0].Support[0].Evidence.SourceRef)
			require.NoError(err)

			content := "[PERSON] has conflicting reports"
			key := personobs.ContentKey(personobs.LevelContradiction, "", content)
			rejectedAt := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at) VALUES(?,?,'outdated','owner',?)`), in.PersonID, key, rejectedAt)
			require.NoError(err)
			if exclusion == "list" {
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id=? WHERE id=?`), "newsletter.example.test", ref.MessageID)
			} else {
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET draft_authored=TRUE WHERE id=?`), ref.MessageID)
			}
			require.NoError(err)

			contradictionInput := in
			contradictionInput.Proposals = []personobs.ProposedObservation{{
				PersonID:     in.PersonID,
				Level:        personobs.LevelContradiction,
				Origin:       personobs.OriginReasoning,
				Kind:         personobs.KindOther,
				Content:      content,
				ConfidenceBP: 800,
				Premises: []personobs.PremiseInput{
					{ID: supportID, Role: personobs.PremiseRoleSupport},
					{ID: counterID, Role: personobs.PremiseRoleCounter},
				},
			}}
			result := applyObservationInput(t, f.store, contradictionInput, generation)
			require.Len(result.Items, 1)
			assert.Equal("dropped-invalid", result.Items[0].Outcome)
			require.NotNil(result.Items[0].Failure)
			assert.Equal("premise-not-supported", result.Items[0].Failure.Reason)

			var active int
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`), in.PersonID, key).Scan(&active))
			assert.Equal(1, active)
			for _, id := range []int64{supportID, counterID} {
				detail, err := f.store.GetPersonObservation(t.Context(), id)
				require.NoError(err)
				require.Len(detail.Evidence, 1)
				wantExclusion := personobs.ExclusionReasonList
				if exclusion == "draft" {
					wantExclusion = personobs.ExclusionReasonDraft
				}
				assert.Equal(wantExclusion, detail.Evidence[0].Exclusion)
				assert.Zero(detail.MentionCount)
			}
		})
	}
}
