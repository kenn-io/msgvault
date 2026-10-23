package store_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
)

func applyObservationInput(t *testing.T, st *store.Store, in personobs.BatchInput, generation int64) personobs.BatchResult {
	t.Helper()
	batch, err := st.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(t, err)
	result, err := st.ApplyPersonObservationBatch(t.Context(), batch, generation, "owner")
	require.NoError(t, err)
	return result
}
func TestApplyObservationBatchCreatesWithSequenceAndLeaf(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	result := applyObservationInput(t, f.store, in, generation)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "created", result.Items[0].Outcome)
	detail, err := f.store.GetPersonObservation(t.Context(), result.Items[0].ObservationID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), detail.Sequence)
	assert.Equal(t, personobs.StatusActive, detail.Status)
	assert.Equal(t, 1, detail.MentionCount)
	assert.Equal(t, 1, detail.SupportedCount)
	require.Len(t, detail.Evidence, 1)
	assert.Equal(t, f.conversationID, detail.Evidence[0].Leaf.ConversationID)
	assert.Equal(t, &generation, detail.GenerationID)
}
func TestApplyObservationBatchReinforcesNormalizedDuplicate(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	first := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	in.Proposals[0].Content = "[person]  RUNS."
	repeat := applyObservationInput(t, f.store, in, generation)
	assert.Equal(t, "reinforced", repeat.Items[0].Outcome)
	assert.Equal(t, first, repeat.Items[0].ObservationID)
	detail, err := f.store.GetPersonObservation(t.Context(), first)
	require.NoError(t, err)
	assert.Equal(t, 1, detail.MentionCount)
	assert.Equal(t, "[PERSON] runs", detail.Content)
	assert.Contains(t, detail.Events[len(detail.Events)-1].Detail, "quote")
	second := f.insertMessage(t, "observation-second", "email", f.aliceID, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, second, "I run too")
	item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, second)
	evidence := sweepEvidenceInput(t, item)
	offset := strings.Index(evidence.Excerpt, "I run too")
	in.Proposals[0].Support = []personobs.SupportInput{{Evidence: evidence, Quote: "I run too", QuoteStart: len([]rune(evidence.Excerpt[:offset]))}}
	applyObservationInput(t, f.store, in, generation)
	detail, err = f.store.GetPersonObservation(t.Context(), first)
	require.NoError(t, err)
	assert.Equal(t, 2, detail.MentionCount)
	assert.Equal(t, "[PERSON] runs", detail.Content)
}
func TestApplyObservationBatchSupersedeBlockedByPinOrConfirmed(t *testing.T) {
	for _, column := range []string{"pinned", "owner_state"} {
		t.Run(column, func(t *testing.T) {
			f, in, generation := observationBatchFixture(t, "I run")
			first := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			value := any(true)
			if column == "owner_state" {
				value = "confirmed"
			}
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET `+column+`=? WHERE id=?`), value, first)
			require.NoError(t, err)
			in.Proposals[0].Content = "[PERSON] runs every morning"
			in.Proposals[0].SupersedesIDs = []int64{first}
			result := applyObservationInput(t, f.store, in, generation)
			require.Len(t, result.Items, 1)
			assert.Contains(t, result.Items[0].AffectedIDs, first)
			detail, err := f.store.GetPersonObservation(t.Context(), first)
			require.NoError(t, err)
			assert.Equal(t, personobs.StatusActive, detail.Status)
			var blocked int
			require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_events WHERE event='supersede-blocked' AND observation_id=?`), first).Scan(&blocked))
			assert.Equal(t, 1, blocked)
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
	f, in, _ := observationBatchFixture(t, "I run")
	other := insertPersonFactSchemaGeneration(t, f.store, f.bobPersonID, "other")
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(t, err)
	_, err = f.store.ApplyPersonObservationBatch(t.Context(), batch, other, "owner")
	require.Error(t, err)
	var count int
	require.NoError(t, f.store.DB().QueryRow(`SELECT COUNT(*) FROM person_observations`).Scan(&count))
	assert.Zero(t, count)
}
func TestApplyObservationBatchAppendsPremiseToOlderDerived(t *testing.T) {
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
	assert.Greater(t, third, id)
	derived.Proposals[0].Premises = append(derived.Proposals[0].Premises, personobs.PremiseInput{ID: third, Role: personobs.PremiseRoleSupport})
	derived.Proposals[0].ReinforcesID = &id
	result := applyObservationInput(t, f.store, derived, generation)
	assert.Equal(t, "reinforced", result.Items[0].Outcome)
	detail, err := f.store.GetPersonObservation(t.Context(), id)
	require.NoError(t, err)
	assert.Len(t, detail.Premises, 3)
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
	f, in, generation := observationBatchFixture(t, "I run")
	first := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	detail, err := f.store.GetPersonObservation(t.Context(), first)
	require.NoError(t, err)
	require.NoError(t, f.store.DeletePersonObservationForTest(t.Context(), first))
	in.Proposals[0].Content = "[PERSON] swims"
	next := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	later, err := f.store.GetPersonObservation(t.Context(), next)
	require.NoError(t, err)
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
	f, in, _ := observationBatchFixture(t, "I run")
	key := personobs.ContentKey(personobs.LevelExplicit, "", in.Proposals[0].Content)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor) VALUES(?,?,?,'owner')`), in.PersonID, key, reason)
	require.NoError(t, err)
	got, err := f.store.DecideObservationSuppressionForTest(t.Context(), in.PersonID, key, store.SuppressionCandidate{Origin: origin, PostRejectionSupport: support, Floor: floor})
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
	var active int
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND cleared_at IS NULL`), in.PersonID).Scan(&active))
	if want == "lifted" {
		assert.Zero(t, active)
	} else {
		assert.Equal(t, 1, active)
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
	f, in, generation := observationBatchFixture(t, "I run")
	bad := in.Proposals[0]
	bad.Support = append([]personobs.SupportInput(nil), bad.Support...)
	bad.Support[0].Evidence.SourceURL = "http://example.com"
	bad.Content = "[PERSON] swims"
	in.Proposals = append(in.Proposals, bad)
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(t, err)
	assert.NotNil(t, batch.Items[1].Failure)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	assert.Equal(t, "created", result.Items[0].Outcome)
	assert.Equal(t, "dropped-invalid", result.Items[1].Outcome)
}
func TestApplyObservationBatchInvalidTargetPreservesSuppression(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	key := personobs.ContentKey(personobs.LevelExplicit, "", in.Proposals[0].Content)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at) VALUES(?,?,'outdated','owner',?)`), in.PersonID, key, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	missing := int64(999999)
	in.Proposals[0].ReinforcesID = &missing
	result := applyObservationInput(t, f.store, in, generation)
	assert.Equal(t, "dropped-invalid", result.Items[0].Outcome)
	var active int
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_suppressions WHERE person_id=? AND cleared_at IS NULL`), in.PersonID).Scan(&active))
	assert.Equal(t, 1, active)
}
func TestApplyObservationBatchEventsUseTriggeringGeneration(t *testing.T) {
	f, in, firstGeneration := observationBatchFixture(t, "I run")
	first := applyObservationInput(t, f.store, in, firstGeneration).Items[0].ObservationID
	secondGeneration := insertPersonFactSchemaGeneration(t, f.store, in.PersonID, "second-observation-generation")
	applyObservationInput(t, f.store, in, secondGeneration)
	in.Proposals[0].Content = "[PERSON] swims"
	in.Proposals[0].SupersedesIDs = []int64{first}
	applyObservationInput(t, f.store, in, secondGeneration)
	for _, event := range []string{"reinforced", "superseded"} {
		var generation int64
		require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT generation_id FROM person_observation_events WHERE observation_id=? AND event=?`), first, event).Scan(&generation))
		assert.Equal(t, secondGeneration, generation)
	}
	require.NoError(t, f.store.RejectObservationForTest(t.Context(), first))
	var ownerGeneration *int64
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT generation_id FROM person_observation_events WHERE observation_id=? AND event='rejected'`), first).Scan(&ownerGeneration))
	assert.Nil(t, ownerGeneration)
}
func TestApplyObservationBatchSupersededTargetFallsThrough(t *testing.T) {
	for _, withLive := range []bool{false, true} {
		t.Run(fmt.Sprint(withLive), func(t *testing.T) {
			f, in, generation := observationBatchFixture(t, "I run")
			old := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			in.Proposals[0].ReinforcesID = &old
			batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
			require.NoError(t, err)
			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET status='superseded' WHERE id=?`), old)
			require.NoError(t, err)
			var live int64
			if withLive {
				replacement := in
				replacement.Proposals = append([]personobs.ProposedObservation(nil), in.Proposals...)
				replacement.Proposals[0].ReinforcesID = nil
				live = applyObservationInput(t, f.store, replacement, generation).Items[0].ObservationID
			}
			result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
			require.NoError(t, err)
			if withLive {
				assert.Equal(t, "reinforced", result.Items[0].Outcome)
				assert.Equal(t, live, result.Items[0].ObservationID)
			} else {
				assert.Equal(t, "created", result.Items[0].Outcome)
				assert.NotEqual(t, old, result.Items[0].ObservationID)
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
