package store_test

import (
	"context"
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

type observationSupersessionFallbackCase struct {
	fixture           personSweepJournalFixture
	batch             store.PreparedObservationBatch
	generationID      int64
	supersededID      int64
	fallbackID        int64
	fallbackMessageID int64
}

func newObservationSupersessionFallbackCase(t *testing.T) observationSupersessionFallbackCase {
	t.Helper()
	require := require.New(t)
	f, input, generation := observationBatchFixture(t, "I run")
	initial := applyObservationInput(t, f.store, input, generation)
	require.Len(initial.Items, 1)
	supersededID := initial.Items[0].ObservationID

	fallbackSupport, fallbackMessageID := observationSupportForMessage(
		t, f, "observation-fallback-retained", "I swim", "I swim",
		time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
	)
	fallbackProposal := input.Proposals[0]
	fallbackProposal.Content = "[PERSON] swims"
	fallbackProposal.Support = []personobs.SupportInput{fallbackSupport}
	fallbackInput := input
	fallbackInput.Proposals = []personobs.ProposedObservation{fallbackProposal}
	fallback := applyObservationInput(t, f.store, fallbackInput, generation)
	require.Len(fallback.Items, 1)
	fallbackID := fallback.Items[0].ObservationID

	reinforcementSupport, _ := observationSupportForMessage(
		t, f, "observation-fallback-new", "I swim today", "I swim",
		time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
	)
	earlier := input.Proposals[0]
	earlier.Content = "[PERSON] runs every day"
	earlier.SupersedesIDs = []int64{supersededID}
	reinforcement := fallbackProposal
	reinforcement.Support = []personobs.SupportInput{reinforcementSupport}
	reinforcement.ReinforcesID = &supersededID
	batchInput := input
	batchInput.Proposals = []personobs.ProposedObservation{earlier, reinforcement}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), batchInput)
	require.NoError(err)
	require.Len(batch.Items, 2)
	require.Nil(batch.Items[0].Failure)
	require.Nil(batch.Items[1].Failure)

	return observationSupersessionFallbackCase{
		fixture: f, batch: batch, generationID: generation,
		supersededID: supersededID, fallbackID: fallbackID,
		fallbackMessageID: fallbackMessageID,
	}
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

func TestApplyObservationBatchReinforcesEquivalentSubjectReference(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, _, _ := observationBatchFixture(t, "I run")
	base := personobs.BatchInput{PersonID: f.alicePersonID, Proposals: []personobs.ProposedObservation{{
		PersonID: f.alicePersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner,
		Kind: personobs.KindState, Content: "[PERSON] has a daughter", SubjectRef: "daughter", ConfidenceBP: 1000,
	}}}
	first := applyObservationInput(t, f.store, base, 0)
	require.Len(first.Items, 1)
	assert.Equal("created", first.Items[0].Outcome)
	targetID := first.Items[0].ObservationID

	repeat := base
	repeat.Proposals = append([]personobs.ProposedObservation(nil), base.Proposals...)
	repeat.Proposals[0].SubjectRef = " Daughter "
	repeat.Proposals[0].ReinforcesID = &targetID
	result := applyObservationInput(t, f.store, repeat, 0)
	require.Len(result.Items, 1)
	assert.Equal("reinforced", result.Items[0].Outcome)
	assert.Equal(targetID, result.Items[0].ObservationID)
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
func TestApplyObservationBatchMissingSupersessionTargetDoesNotAbortSiblings(t *testing.T) {
	for _, test := range []struct {
		name     string
		targetID int64
	}{
		{name: "zero", targetID: 0},
		{name: "missing", targetID: 9223372036854775807},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, _, _ := observationBatchFixture(t, "I run")
			result := applyObservationInput(t, f.store, personobs.BatchInput{
				PersonID: f.alicePersonID,
				Proposals: []personobs.ProposedObservation{
					{
						PersonID: f.alicePersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner,
						Kind: personobs.KindState, Content: "[PERSON] prefers tea", ConfidenceBP: 1000,
						SupersedesIDs: []int64{test.targetID},
					},
					{
						PersonID: f.alicePersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner,
						Kind: personobs.KindState, Content: "[PERSON] reads books", ConfidenceBP: 1000,
					},
				},
			}, 0)

			require.Len(result.Items, 2)
			assert.Equal("dropped-invalid", result.Items[0].Outcome)
			require.NotNil(result.Items[0].Failure)
			assert.Equal("supersedes_ids", result.Items[0].Failure.Field)
			assert.Equal("target-missing", result.Items[0].Failure.Reason)
			assert.Equal("created", result.Items[1].Outcome)
			assert.Positive(result.Items[1].ObservationID)
			var invalidCount int
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*)
				FROM person_observations WHERE person_id=? AND content=?`), f.alicePersonID,
				"[PERSON] prefers tea").Scan(&invalidCount))
			assert.Zero(invalidCount)
		})
	}
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

func TestApplyMachineObservationBatchAfterUntrackingDoesNotWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)

	_, err = f.store.SetPersonTrackingContext(t.Context(), f.alicePersonID, false)
	require.NoError(err)
	_, err = f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
	require.ErrorIs(err, store.ErrPersonFactPersonNotTracked)
	var count int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_observations`).Scan(&count))
	assert.Zero(count)

	ownerBatch, err := f.store.PreparePersonObservationBatch(t.Context(), personobs.BatchInput{
		PersonID: f.alicePersonID,
		Proposals: []personobs.ProposedObservation{{
			PersonID: f.alicePersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner,
			Kind: personobs.KindState, Content: "[PERSON] prefers tea", ConfidenceBP: 1000,
		}},
	})
	require.NoError(err)
	ownerResult, err := f.store.ApplyPersonObservationBatch(t.Context(), ownerBatch, 0, "owner")
	require.NoError(err)
	require.Len(ownerResult.Items, 1)
	assert.Equal("created", ownerResult.Items[0].Outcome)
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
func TestApplyObservationBatchDropsInvalidDeductiveReinforcementAndKeepsSibling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	first := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	proposal := personobs.ProposedObservation{
		PersonID: in.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
		Kind: personobs.KindState, Content: "[PERSON] exercises", ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{{ID: first, Role: personobs.PremiseRoleSupport}},
	}
	derived := in
	derived.Proposals = []personobs.ProposedObservation{proposal}
	older := applyObservationInput(t, f.store, derived, generation).Items[0].ObservationID

	newerProposal := proposal
	newerProposal.Content = "[PERSON] keeps fit"
	newerProposal.Premises = []personobs.PremiseInput{{ID: older, Role: personobs.PremiseRoleSupport}}
	derived.Proposals = []personobs.ProposedObservation{newerProposal}
	newer := applyObservationInput(t, f.store, derived, generation).Items[0].ObservationID

	invalid := proposal
	invalid.Premises = []personobs.PremiseInput{{ID: newer, Role: personobs.PremiseRoleSupport}}
	validSibling := proposal
	validSibling.Content = "[PERSON] moves regularly"
	in.Proposals = []personobs.ProposedObservation{invalid, validSibling}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "owner")
	require.NoError(err)
	require.Len(result.Items, 2)
	assert.Equal("dropped-invalid", result.Items[0].Outcome)
	require.NotNil(result.Items[0].Failure)
	assert.Equal("premises", result.Items[0].Failure.Field)
	assert.Equal("premise-order", result.Items[0].Failure.Reason)
	assert.Equal("created", result.Items[1].Outcome)

	olderDetail, err := f.store.GetPersonObservation(t.Context(), older)
	require.NoError(err)
	require.Len(olderDetail.Premises, 1)
	assert.Equal(first, olderDetail.Premises[0].ID)
	_, err = f.store.GetPersonObservation(t.Context(), result.Items[1].ObservationID)
	require.NoError(err)
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

func TestApplyObservationBatchEarlierSupersessionFallsBackToLiveContentKey(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	testCase := newObservationSupersessionFallbackCase(t)

	result, err := testCase.fixture.store.ApplyPersonObservationBatch(
		t.Context(), testCase.batch, testCase.generationID, "extractor")
	requirements.NoError(err)
	requirements.Len(result.Items, 2)
	assertions.Equal("superseded-target", result.Items[0].Outcome)
	assertions.Equal([]int64{testCase.supersededID}, result.Items[0].AffectedIDs)
	assertions.Equal("reinforced", result.Items[1].Outcome)
	assertions.Equal(testCase.fallbackID, result.Items[1].ObservationID)
}

func TestApplyObservationBatchPreparedIntegrityAndIsolation(t *testing.T) {
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	in.Generation.PersonID = f.bobPersonID
	in.Proposals[0].Support[0].Evidence.Excerpt = "changed"
	_, err = f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
	require.NoError(err)
	batch.Items[0].Proposal.Content = "[PERSON] altered"
	_, err = f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
	require.ErrorContains(err, "integrity")
}

func TestApplyObservationBatchUsesSingleConnection(t *testing.T) {
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	f.store.DB().SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	result, err := f.store.ApplyPersonObservationBatch(ctx, batch, generation, "extractor")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(t, "created", result.Items[0].Outcome)
}

func TestApplyObservationBatchRejectsSupportChangedAfterPreparation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		update func(t *testing.T, f personSweepJournalFixture, messageID int64)
	}{
		{
			name: "message edited",
			update: func(t *testing.T, f personSweepJournalFixture, messageID int64) {
				t.Helper()
				_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
					`UPDATE message_bodies SET body_text=? WHERE message_id=?`), "I run now", messageID)
				require.NoError(t, err)
			},
		},
		{
			name: "sender reassigned",
			update: func(t *testing.T, f personSweepJournalFixture, messageID int64) {
				t.Helper()
				_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
					`UPDATE messages SET sender_id=? WHERE id=?`), f.bobID, messageID)
				require.NoError(t, err)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			targetID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			candidate := in
			candidate.Proposals = append([]personobs.ProposedObservation(nil), in.Proposals...)
			candidate.Proposals[0].Content = "[PERSON] runs every morning"
			candidate.Proposals[0].SupersedesIDs = []int64{targetID}
			batch, err := f.store.PreparePersonObservationBatch(t.Context(), candidate)
			require.NoError(err)
			require.Nil(batch.Items[0].Failure)
			ref, err := peoplesweep.DecodePersonSweepEvidenceRef(candidate.Proposals[0].Support[0].Evidence.SourceRef)
			require.NoError(err)

			tt.update(t, f, ref.MessageID)

			result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
			require.NoError(err)
			require.Len(result.Items, 1)
			assert.Equal("dropped-invalid", result.Items[0].Outcome)
			require.NotNil(result.Items[0].Failure)
			assert.Equal("support", result.Items[0].Failure.Field)
			assert.Equal("unaligned-evidence", result.Items[0].Failure.Reason)

			target, err := f.store.GetPersonObservation(t.Context(), targetID)
			require.NoError(err)
			assert.Equal(personobs.StatusActive, target.Status)
			assert.Nil(target.SupersededByID)
			page, err := f.store.ListPersonObservations(t.Context(), f.alicePersonID, personobs.Filter{Limit: 20})
			require.NoError(err)
			require.Len(page.Items, 1)
			assert.Equal(targetID, page.Items[0].ID)
		})
	}
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

func TestApplyObservationBatchReinforcementPropagatesClassifications(t *testing.T) {
	tests := []struct {
		name      string
		sensitive bool
		category  personobs.Category
		owner     bool
	}{
		{name: "sensitivity", sensitive: true},
		{name: "category", category: personobs.CategoryHealth},
		{name: "owner sensitivity", sensitive: true, owner: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			created := applyObservationInput(t, f.store, in, generation)
			requirements.Len(created.Items, 1)
			requirements.Equal("created", created.Items[0].Outcome)
			premiseID := created.Items[0].ObservationID

			derivedIDs := make([]int64, 0, 2)
			for i, content := range []string{
				"[PERSON] has a documented status",
				"[PERSON] has a second derived status",
			} {
				derived := personobs.ProposedObservation{
					PersonID:     in.PersonID,
					Level:        personobs.LevelDeductive,
					Origin:       personobs.OriginReasoning,
					Kind:         personobs.KindState,
					Content:      content,
					ConfidenceBP: 800,
					Premises:     []personobs.PremiseInput{{ID: premiseID, Role: personobs.PremiseRoleSupport}},
				}
				derivedInput := in
				derivedInput.Proposals = []personobs.ProposedObservation{derived}
				result := applyObservationInput(t, f.store, derivedInput, generation)
				requirements.Len(result.Items, 1)
				requirements.Equal("created", result.Items[0].Outcome)
				derivedIDs = append(derivedIDs, result.Items[0].ObservationID)
				if i == 0 {
					premiseID = derivedIDs[0]
				}
			}

			before, err := f.store.ListPersonObservations(t.Context(), in.PersonID, personobs.Filter{Limit: 20})
			requirements.NoError(err)
			requirements.Len(before.Items, 3)

			reinforcement := in.Proposals[0]
			reinforcement.ReinforcesID = &created.Items[0].ObservationID
			reinforcement.Sensitive = tt.sensitive
			if tt.category != "" {
				reinforcement.ModelCategories = []personobs.Category{tt.category}
			}
			reinforcementGeneration := generation
			if tt.owner {
				reinforcement = personobs.ProposedObservation{
					PersonID:     in.PersonID,
					Level:        personobs.LevelExplicit,
					Origin:       personobs.OriginOwner,
					Kind:         personobs.KindState,
					Content:      "[PERSON] runs",
					ConfidenceBP: 1000,
					Sensitive:    tt.sensitive,
				}
				in.Generation = nil
				in.ProgramFingerprint = ""
				reinforcementGeneration = 0
			}
			in.Proposals = []personobs.ProposedObservation{reinforcement}
			result := applyObservationInput(t, f.store, in, reinforcementGeneration)
			requirements.Len(result.Items, 1)
			requirements.Equal("reinforced", result.Items[0].Outcome)

			for _, id := range append([]int64{created.Items[0].ObservationID}, derivedIDs...) {
				detail, err := f.store.GetPersonObservation(t.Context(), id)
				requirements.NoError(err)
				assertions.True(detail.Sensitive)
				if tt.category != "" {
					assertions.Contains(detail.Categories, tt.category)
				} else {
					assertions.Empty(detail.Categories)
				}
			}
			page, err := f.store.ListPersonObservations(t.Context(), in.PersonID, personobs.Filter{Limit: 20})
			requirements.NoError(err)
			assertions.Empty(page.Items)
		})
	}
}

func TestApplyObservationBatchReloadsCitationAttributionUnderLock(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	requirements.NoError(err)
	requirements.Len(batch.Items, 1)
	requirements.Nil(batch.Items[0].Failure)
	requirements.Len(batch.Items[0].Citations, 1)
	messageID := batch.Items[0].Citations[0].Leaf.MessageID
	assertions.False(batch.Items[0].Citations[0].Leaf.AuthorIsOwner)

	requirements.NoError(f.store.AddAccountIdentity(f.sourceID, "alice@example.test", "manual"))
	isFromMe, err := f.store.GetMessageIsFromMe(messageID)
	requirements.NoError(err)
	requirements.True(isFromMe)

	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "extractor")
	requirements.NoError(err)
	requirements.Len(result.Items, 1)
	requirements.Equal("created", result.Items[0].Outcome)
	detail, err := f.store.GetPersonObservation(t.Context(), result.Items[0].ObservationID)
	requirements.NoError(err)
	requirements.Len(detail.Evidence, 1)
	assertions.True(detail.Evidence[0].Leaf.AuthorIsOwner)
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
	var messageID int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT message_id FROM person_observation_evidence WHERE observation_id=?`), extractionID).Scan(&messageID))
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET hidden_text_present=? WHERE id=?`), true, messageID)
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

func TestApplyObservationBatchRejectsOwnerPromotionKeyCollisionWithoutAbortingSiblings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	extractionID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID

	ownerInput := personobs.BatchInput{PersonID: in.PersonID, Proposals: []personobs.ProposedObservation{{
		PersonID: in.PersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner,
		Kind: personobs.KindState, Content: "[PERSON] runs every morning", ConfidenceBP: 1000,
	}}}
	ownerID := applyObservationInput(t, f.store, ownerInput, 0).Items[0].ObservationID

	collision := ownerInput.Proposals[0]
	collision.ReinforcesID = &extractionID
	sibling := collision
	sibling.Content = "[PERSON] likes tea"
	sibling.ReinforcesID = nil
	ownerInput.Proposals = []personobs.ProposedObservation{collision, sibling}
	result := applyObservationInput(t, f.store, ownerInput, 0)

	require.Len(result.Items, 2)
	assert.Equal("dropped-invalid", result.Items[0].Outcome)
	require.NotNil(result.Items[0].Failure)
	assert.Equal("live-key-collision", result.Items[0].Failure.Reason)
	assert.Equal("created", result.Items[1].Outcome)
	assert.NotEqual(ownerID, result.Items[1].ObservationID)
	assert.NotEqual(extractionID, result.Items[1].ObservationID)

	extraction, err := f.store.GetPersonObservation(t.Context(), extractionID)
	require.NoError(err)
	assert.Equal(personobs.OriginExtraction, extraction.Origin)
	assert.Equal("[PERSON] runs", extraction.Content)
	owner, err := f.store.GetPersonObservation(t.Context(), ownerID)
	require.NoError(err)
	assert.Equal("[PERSON] runs every morning", owner.Content)
	created, err := f.store.GetPersonObservation(t.Context(), result.Items[1].ObservationID)
	require.NoError(err)
	assert.Equal("[PERSON] likes tea", created.Content)
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

func TestApplyObservationBatchUsesPersistedValidFromWhenReinforcingReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	oldStart := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	in.Proposals[0].ValidFrom = &oldStart
	oldID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID

	replacementStart := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	replacement := in.Proposals[0]
	replacement.Content = "[PERSON] runs every morning"
	replacement.ValidFrom = &replacementStart
	replacementID := applyObservationInput(t, f.store, personobs.BatchInput{
		PersonID: in.PersonID, Generation: in.Generation, ProgramFingerprint: in.ProgramFingerprint,
		Proposals: []personobs.ProposedObservation{replacement},
	}, generation).Items[0].ObservationID

	submittedStart := replacementStart.AddDate(0, 0, 10)
	reinforcement := replacement
	reinforcement.ValidFrom = &submittedStart
	reinforcement.ReinforcesID = &replacementID
	reinforcement.SupersedesIDs = []int64{oldID}
	in.Proposals = []personobs.ProposedObservation{reinforcement}
	result := applyObservationInput(t, f.store, in, generation)

	require.Len(result.Items, 1)
	assert.Equal("superseded-target", result.Items[0].Outcome)
	old, err := f.store.GetPersonObservation(t.Context(), oldID)
	require.NoError(err)
	require.NotNil(old.ValidUntil)
	assert.Equal(replacementStart, *old.ValidUntil)
	current, err := f.store.GetPersonObservation(t.Context(), replacementID)
	require.NoError(err)
	require.NotNil(current.ValidFrom)
	assert.Equal(replacementStart, *current.ValidFrom)
}

func TestApplyObservationBatchNormalizesMixedOffsetCitationTimesBeforeSummary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	firstRef, err := peoplesweep.DecodePersonSweepEvidenceRef(in.Proposals[0].Support[0].Evidence.SourceRef)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET sent_at=? WHERE id=?`), "2026-10-02T09:30:00+03:00", firstRef.MessageID)
	require.NoError(err)
	firstItem := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, firstRef.MessageID)
	firstEvidence := sweepEvidenceInput(t, firstItem)
	firstOffset := strings.Index(firstEvidence.Excerpt, "I run")
	require.NotEqual(-1, firstOffset)
	in.Proposals[0].Support[0].Evidence = firstEvidence
	in.Proposals[0].Support[0].QuoteStart = len([]rune(firstEvidence.Excerpt[:firstOffset]))

	secondMessageID := f.insertMessage(t, "observation-offset", "email", f.aliceID, time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC))
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET sent_at=? WHERE id=?`), "2026-10-02T08:00:00+00:00", secondMessageID)
	require.NoError(err)
	addSweepBody(t, f, secondMessageID, "I run today")
	secondItem := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, secondMessageID)
	secondEvidence := sweepEvidenceInput(t, secondItem)
	secondOffset := strings.Index(secondEvidence.Excerpt, "I run today")
	require.NotEqual(-1, secondOffset)
	secondSupport := personobs.SupportInput{Evidence: secondEvidence, Quote: "I run today", QuoteStart: len([]rune(secondEvidence.Excerpt[:secondOffset]))}
	in.Proposals[0].Support = append(in.Proposals[0].Support, secondSupport)
	result := applyObservationInput(t, f.store, in, generation)

	require.Len(result.Items, 1)
	require.Equal("created", result.Items[0].Outcome)
	detail, err := f.store.GetPersonObservation(t.Context(), result.Items[0].ObservationID)
	require.NoError(err)
	wantFirst := time.Date(2026, 10, 2, 6, 30, 0, 0, time.UTC)
	wantLast := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	require.NotNil(detail.FirstSeenAt)
	require.NotNil(detail.LastSeenAt)
	assert.Equal(wantFirst, *detail.FirstSeenAt)
	assert.Equal(wantLast, *detail.LastSeenAt)
	require.Len(detail.Evidence, 2)
	eventTimes := make(map[int64]time.Time, len(detail.Evidence))
	for _, citation := range detail.Evidence {
		eventTimes[citation.Leaf.MessageID] = citation.Leaf.EventTime
	}
	assert.Equal(wantFirst, eventTimes[firstRef.MessageID])
	assert.Equal(wantLast, eventTimes[secondMessageID])
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

func TestApplyObservationBatchRevalidatesContradictionAfterPremiseExclusionClears(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	supportID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	counterSupport, counterMessageID := observationSupportForMessage(t, f, "observation-counter-excluded", "I do not run", "I do not run", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id='list.example.test' WHERE id=?`), counterMessageID)
	require.NoError(err)
	counter := in.Proposals[0]
	counter.Content = "[PERSON] does not run"
	counter.Support = []personobs.SupportInput{counterSupport}
	counterInput := in
	counterInput.Proposals = []personobs.ProposedObservation{counter}
	counterID := applyObservationInput(t, f.store, counterInput, generation).Items[0].ObservationID

	contradiction := personobs.ProposedObservation{
		PersonID: in.PersonID, Level: personobs.LevelContradiction, Origin: personobs.OriginReasoning,
		Kind: personobs.KindOther, Content: "[PERSON] has conflicting reports", ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{
			{ID: supportID, Role: personobs.PremiseRoleSupport},
			{ID: counterID, Role: personobs.PremiseRoleCounter},
		},
	}
	contradictionInput := in
	contradictionInput.Proposals = []personobs.ProposedObservation{contradiction}

	structural := contradiction
	structural.Premises = []personobs.PremiseInput{{ID: counterID, Role: personobs.PremiseRoleSupport}}
	structuralInput := contradictionInput
	structuralInput.Proposals = []personobs.ProposedObservation{structural}
	structuralBatch, err := f.store.PreparePersonObservationBatch(t.Context(), structuralInput)
	require.NoError(err)
	assert.NotNil(structuralBatch.Items[0].Failure)
	if structuralBatch.Items[0].Failure != nil {
		assert.Equal("premise-not-supported", structuralBatch.Items[0].Failure.Reason)
	}

	batch, err := f.store.PreparePersonObservationBatch(t.Context(), contradictionInput)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id=NULL WHERE id=?`), counterMessageID)
	require.NoError(err)

	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "reasoner")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("created", result.Items[0].Outcome)
	detail, err := f.store.GetPersonObservation(t.Context(), result.Items[0].ObservationID)
	require.NoError(err)
	assert.Len(detail.Premises, 2)
	counterDetail, err := f.store.GetPersonObservation(t.Context(), counterID)
	require.NoError(err)
	assert.Equal(1, counterDetail.SupportedCount)
}

func TestApplyObservationBatchDoesNotSupersedeWithExcludedReplacementEvidence(t *testing.T) {
	for _, exclusion := range []string{"list", "draft", "hidden_text"} {
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
			switch exclusion {
			case "list":
				query = `UPDATE messages SET list_id=? WHERE id=?`
				value = "newsletter.example.test"
			case "draft":
				query = `UPDATE messages SET draft_authored=? WHERE id=?`
				value = true
			case "hidden_text":
				query = `UPDATE messages SET hidden_text_present=? WHERE id=?`
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
			switch exclusion {
			case "draft":
				wantExclusion = personobs.ExclusionReasonDraft
			case "hidden_text":
				wantExclusion = personobs.ExclusionReasonHiddenText
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

func TestApplyObservationBatchRevalidatesSubMicrosecondEvidence(t *testing.T) {
	for _, operation := range []string{"derive", "supersede"} {
		t.Run(operation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			subMicrosecond := time.Date(2026, 10, 4, 12, 0, 0, 123456789, time.UTC)
			support, messageID := observationSupportForMessage(t, f, "observation-nanosecond", "I run daily", "I run daily", subMicrosecond)
			var stored time.Time
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT sent_at FROM messages WHERE id=?`), messageID).Scan(&stored))
			wantTime := subMicrosecond
			if f.store.IsPostgreSQL() {
				wantTime = wantTime.Truncate(time.Microsecond)
			}
			assert.Equal(wantTime.Nanosecond(), stored.Nanosecond())
			assert.Equal(wantTime.Nanosecond(), support.Evidence.EventTime.Nanosecond())
			in.Proposals[0].Support = []personobs.SupportInput{support}
			original := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID

			if operation == "derive" {
				derived := in
				derived.Proposals = []personobs.ProposedObservation{{
					PersonID: in.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
					Kind: personobs.KindOther, Content: "[PERSON] maintains a regular routine", ConfidenceBP: 800,
					Premises: []personobs.PremiseInput{{ID: original, Role: personobs.PremiseRoleSupport}},
				}}
				result := applyObservationInput(t, f.store, derived, generation)
				require.Len(result.Items, 1)
				assert.Equal("created", result.Items[0].Outcome)
				return
			}

			replacement := in.Proposals[0]
			replacement.Content = "[PERSON] runs daily"
			replacement.SupersedesIDs = []int64{original}
			in.Proposals = []personobs.ProposedObservation{replacement}
			result := applyObservationInput(t, f.store, in, generation)
			require.Len(result.Items, 1)
			assert.Equal("superseded-target", result.Items[0].Outcome)
			old, err := f.store.GetPersonObservation(t.Context(), original)
			require.NoError(err)
			assert.Equal(personobs.StatusSuperseded, old.Status)
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

func TestApplyObservationBatchRejectsStoredPremiseAfterParticipantSplit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	messageID := f.insertMessage(t, "observation-split-premise", "email", f.bobID, time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, messageID, "I live in Berlin")

	survivor, err := f.store.GetPersonContext(t.Context(), f.alicePersonID)
	require.NoError(err)
	absorbed, err := f.store.GetPersonContext(t.Context(), f.bobPersonID)
	require.NoError(err)
	merged, err := f.store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "observation-split-premise-merge", Actor: "owner",
	})
	require.NoError(err)

	item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, messageID)
	evidence := sweepEvidenceInput(t, item)
	quote := "I live in Berlin"
	offset := strings.Index(evidence.Excerpt, quote)
	require.NotEqual(-1, offset)
	fingerprint := strings.Repeat("a", 64)
	in := personobs.BatchInput{
		PersonID: merged.Person.ID, ProgramFingerprint: fingerprint,
		Generation: &personfacts.GenerationInput{PersonID: merged.Person.ID, ProgramFingerprint: fingerprint},
		Proposals: []personobs.ProposedObservation{{
			PersonID: merged.Person.ID, Level: personobs.LevelExplicit, Origin: personobs.OriginExtraction,
			Kind: personobs.KindState, Content: "[PERSON] lives in Berlin", Attribution: personobs.AttributionSelf,
			ConfidenceBP: 800, Support: []personobs.SupportInput{{
				Evidence: evidence, Quote: quote, QuoteStart: len([]rune(evidence.Excerpt[:offset])),
			}},
		}},
	}
	generation := insertPersonFactSchemaGeneration(t, f.store, merged.Person.ID, "observation-split-premise")
	premise := applyObservationInput(t, f.store, in, generation)
	require.Len(premise.Items, 1)
	assert.Equal("created", premise.Items[0].Outcome)
	premiseID := premise.Items[0].ObservationID

	currentPerson, err := f.store.GetPersonContext(t.Context(), merged.Person.ID)
	require.NoError(err)
	_, err = f.store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs: []int64{f.bobID}, ExpectedSourceRevision: currentPerson.Revision,
		IdempotencyKey: "observation-split-premise-split", Actor: "owner",
	})
	require.NoError(err)
	stale, err := f.store.GetPersonObservation(t.Context(), premiseID)
	require.NoError(err)
	assert.Equal(1, stale.SupportedCount, "the stored summary remains stale after the split")

	derivedInput := in
	derivedInput.Proposals = []personobs.ProposedObservation{{
		PersonID: merged.Person.ID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
		Kind: personobs.KindOther, Content: "[PERSON] has a home in Berlin", ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{{ID: premiseID, Role: personobs.PremiseRoleSupport}},
	}}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), derivedInput)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "reasoner")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("dropped-invalid", result.Items[0].Outcome)
	require.NotNil(result.Items[0].Failure)
	assert.Equal("premise-not-supported", result.Items[0].Failure.Reason)
	var count int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM person_observations WHERE person_id=? AND content=?`),
		merged.Person.ID, "[PERSON] has a home in Berlin").Scan(&count))
	assert.Zero(count)
}

func TestApplyObservationBatchDropsStaleSupportAfterCardDAVSurvivorSplitAndKeepsOwnerSibling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	messageID := f.insertMessage(t, "observation-carddav-split", "email", f.aliceID,
		time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, messageID, "I enjoy cycling")

	canCreate := true
	account, books, err := f.store.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "observation-owner",
		PrincipalURL: "https://contacts.example/principal/observation-owner/",
		HomeURL:      "https://contacts.example/books/observation-owner/",
		Books: []store.CardDAVDiscoveredBook{{
			CanonicalURL: "https://contacts.example/books/observation-owner/personal/",
			DisplayName:  "Personal", SupportsSyncCollection: true,
			SupportsMultiget: true, CanCreate: &canCreate,
		}},
	})
	require.NoError(err)
	require.NotNil(account)
	require.Len(books, 1)
	book := books[0]
	remote := remoteResource(book.CanonicalURL+"survivor.vcf", "observation-carddav-survivor",
		"CardDAV Survivor", "survivor@example.test", `"one"`)
	_, err = f.store.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, ReplaceAll: true, NextSyncToken: "carddav-observation-split",
		Upserts: []store.CardDAVRemoteResource{remote},
	})
	require.NoError(err)
	resource, err := f.store.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	cardDAVSurvivor, err := f.store.GetPersonContext(t.Context(), *resource.PersonID)
	require.NoError(err)
	assert.Empty(cardDAVSurvivor.ParticipantIDs)

	ownerResult := applyObservationInput(t, f.store, personobs.BatchInput{
		PersonID: cardDAVSurvivor.ID,
		Proposals: []personobs.ProposedObservation{{
			PersonID: cardDAVSurvivor.ID, Level: personobs.LevelExplicit,
			Origin: personobs.OriginOwner, Kind: personobs.KindState,
			Content: "[PERSON] owns a bicycle", ConfidenceBP: 1000,
		}},
	}, 0)
	require.Len(ownerResult.Items, 1)
	require.Equal("created", ownerResult.Items[0].Outcome)
	ownerObservationID := ownerResult.Items[0].ObservationID
	_, err = f.store.SetPersonTrackingContext(t.Context(), cardDAVSurvivor.ID, true)
	require.NoError(err)

	absorbed, err := f.store.GetPersonContext(t.Context(), f.alicePersonID)
	require.NoError(err)
	cardDAVSurvivor, err = f.store.GetPersonContext(t.Context(), *resource.PersonID)
	require.NoError(err)
	merged, err := f.store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: cardDAVSurvivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: cardDAVSurvivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "observation-carddav-split-merge", Actor: "test",
	})
	require.NoError(err)

	window, err := f.store.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{
		PersonID: merged.Person.ID, Lane: peoplesweep.SourceConversationText,
		ThroughSequence: latestPersonSweepSequence(t, f.store), Limit: 100,
	})
	require.NoError(err)
	item := sweepItem(t, window.Seeds, messageID)
	evidence := sweepEvidenceInput(t, item)
	quote := "I enjoy cycling"
	offset := strings.Index(evidence.Excerpt, quote)
	require.NotEqual(-1, offset)
	fingerprint := strings.Repeat("a", 64)
	generation := insertPersonFactSchemaGeneration(t, f.store, merged.Person.ID,
		"observation-carddav-split")
	input := personobs.BatchInput{
		PersonID: merged.Person.ID, ProgramFingerprint: fingerprint,
		Generation: &personfacts.GenerationInput{
			PersonID: merged.Person.ID, ProgramFingerprint: fingerprint,
		},
		Proposals: []personobs.ProposedObservation{
			{
				PersonID: merged.Person.ID, Level: personobs.LevelExplicit,
				Origin: personobs.OriginExtraction, Kind: personobs.KindState,
				Content: "[PERSON] enjoys cycling", Attribution: personobs.AttributionSelf,
				ConfidenceBP: 800, Stability: personobs.StabilityDurable,
				Support: []personobs.SupportInput{{
					Evidence: evidence, Quote: quote,
					QuoteStart: len([]rune(evidence.Excerpt[:offset])),
				}},
			},
			{
				PersonID: merged.Person.ID, Level: personobs.LevelDeductive,
				Origin: personobs.OriginReasoning, Kind: personobs.KindState,
				Content: "[PERSON] has a bicycle", ConfidenceBP: 800,
				Premises: []personobs.PremiseInput{{
					ID: ownerObservationID, Role: personobs.PremiseRoleSupport,
				}},
			},
		},
	}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), input)
	require.NoError(err)
	require.Len(batch.Items, 2)
	require.Nil(batch.Items[0].Failure)
	require.Nil(batch.Items[1].Failure)

	current, err := f.store.GetPersonContext(t.Context(), merged.Person.ID)
	require.NoError(err)
	split, err := f.store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs: absorbed.ParticipantIDs, ExpectedSourceRevision: current.Revision,
		IdempotencyKey: "observation-carddav-split-restore", Actor: "test",
	})
	require.NoError(err)
	require.NotNil(split.SourcePerson)
	assert.Empty(split.SourcePerson.ParticipantIDs)

	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "reasoner")
	require.NoError(err)
	require.Len(result.Items, 2)
	assert.Equal("dropped-invalid", result.Items[0].Outcome)
	require.NotNil(result.Items[0].Failure)
	assert.Equal("support", result.Items[0].Failure.Field)
	assert.Equal("unaligned-evidence", result.Items[0].Failure.Reason)
	assert.Equal("created", result.Items[1].Outcome)
	derived, err := f.store.GetPersonObservation(t.Context(), result.Items[1].ObservationID)
	require.NoError(err)
	assert.Equal("[PERSON] has a bicycle", derived.Content)
	require.Len(derived.Premises, 1)
	assert.Equal(ownerObservationID, derived.Premises[0].ID)
}

func TestApplyObservationBatchRejectsDerivedPremiseWithWithdrawnAncestorBeforeSuperseding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, rootInput, generation := observationBatchFixture(t, "I run")
	root := applyObservationInput(t, f.store, rootInput, generation)
	require.Len(root.Items, 1)
	rootID := root.Items[0].ObservationID

	firstDerivedInput := rootInput
	firstDerivedInput.Proposals = []personobs.ProposedObservation{{
		PersonID: rootInput.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
		Kind: personobs.KindOther, Content: "[PERSON] has a running routine", ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{{ID: rootID, Role: personobs.PremiseRoleSupport}},
	}}
	firstDerived := applyObservationInput(t, f.store, firstDerivedInput, generation)
	require.Len(firstDerived.Items, 1)
	firstDerivedID := firstDerived.Items[0].ObservationID
	secondDerivedInput := rootInput
	secondDerivedInput.Proposals = []personobs.ProposedObservation{{
		PersonID: rootInput.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
		Kind: personobs.KindOther, Content: "[PERSON] follows a regular exercise routine", ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{{ID: firstDerivedID, Role: personobs.PremiseRoleSupport}},
	}}
	secondDerived := applyObservationInput(t, f.store, secondDerivedInput, generation)
	require.Len(secondDerived.Items, 1)
	secondDerivedID := secondDerived.Items[0].ObservationID

	owner := applyObservationInput(t, f.store, personobs.BatchInput{
		PersonID: rootInput.PersonID, Proposals: []personobs.ProposedObservation{{
			PersonID: rootInput.PersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner,
			Kind: personobs.KindState, Content: "[PERSON] enjoys walking", ConfidenceBP: 1000,
		}},
	}, 0)
	require.Len(owner.Items, 1)
	ownerID := owner.Items[0].ObservationID
	targetInput := rootInput
	targetInput.Proposals = []personobs.ProposedObservation{{
		PersonID: rootInput.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
		Kind: personobs.KindOther, Content: "[PERSON] takes regular walks", ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{{ID: ownerID, Role: personobs.PremiseRoleSupport}},
	}}
	targetResult := applyObservationInput(t, f.store, targetInput, generation)
	require.Len(targetResult.Items, 1)
	targetID := targetResult.Items[0].ObservationID

	ref, err := peoplesweep.DecodePersonSweepEvidenceRef(rootInput.Proposals[0].Support[0].Evidence.SourceRef)
	require.NoError(err)
	withdrawObservationEvidence(t, f.store, rootInput.PersonID, rootID, ref.MessageID)

	candidateInput := rootInput
	candidateInput.Proposals = []personobs.ProposedObservation{{
		PersonID: rootInput.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
		Kind: personobs.KindOther, Content: "[PERSON] trains every morning", ConfidenceBP: 800,
		Premises:      []personobs.PremiseInput{{ID: secondDerivedID, Role: personobs.PremiseRoleSupport}},
		SupersedesIDs: []int64{targetID},
	}}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), candidateInput)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "reasoner")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("dropped-invalid", result.Items[0].Outcome)
	require.NotNil(result.Items[0].Failure)
	assert.Equal("premise-not-supported", result.Items[0].Failure.Reason)
	target, err := f.store.GetPersonObservation(t.Context(), targetID)
	require.NoError(err)
	assert.Equal(personobs.StatusActive, target.Status)
	assert.Nil(target.SupersededByID)
	var created int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM person_observations WHERE person_id=? AND content=?`),
		rootInput.PersonID, "[PERSON] trains every morning").Scan(&created))
	assert.Zero(created)
}

func TestApplyObservationBatchDropsSupersededCounterPremiseAndKeepsSibling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")

	counter := applyObservationInput(t, f.store, personobs.BatchInput{PersonID: in.PersonID, Proposals: []personobs.ProposedObservation{{
		PersonID: in.PersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner,
		Kind: personobs.KindState, Content: "[PERSON] dislikes coffee", ConfidenceBP: 1000,
	}}}, 0)
	require.Len(counter.Items, 1)
	counterID := counter.Items[0].ObservationID
	support := applyObservationInput(t, f.store, personobs.BatchInput{PersonID: in.PersonID, Proposals: []personobs.ProposedObservation{{
		PersonID: in.PersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner,
		Kind: personobs.KindState, Content: "[PERSON] likes tea", ConfidenceBP: 1000,
	}}}, 0)
	require.Len(support.Items, 1)
	supportID := support.Items[0].ObservationID

	in.Proposals = []personobs.ProposedObservation{
		{
			PersonID: in.PersonID, Level: personobs.LevelContradiction, Origin: personobs.OriginReasoning,
			Kind: personobs.KindOther, Content: "[PERSON] both dislikes coffee and likes it", ConfidenceBP: 800,
			Premises: []personobs.PremiseInput{
				{ID: counterID, Role: personobs.PremiseRoleCounter},
				{ID: supportID, Role: personobs.PremiseRoleSupport},
			},
		},
		{
			PersonID: in.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
			Kind: personobs.KindOther, Content: "[PERSON] chooses tea", ConfidenceBP: 800,
			Premises: []personobs.PremiseInput{{ID: supportID, Role: personobs.PremiseRoleSupport}},
		},
	}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Len(batch.Items, 2)
	require.Nil(batch.Items[0].Failure)
	require.Nil(batch.Items[1].Failure)

	superseding := personobs.BatchInput{PersonID: in.PersonID, Proposals: []personobs.ProposedObservation{{
		PersonID: in.PersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner,
		Kind: personobs.KindState, Content: "[PERSON] prefers herbal tea", ConfidenceBP: 1000,
		SupersedesIDs: []int64{counterID},
	}}}
	superseded := applyObservationInput(t, f.store, superseding, 0)
	require.Len(superseded.Items, 1)
	assert.Equal("superseded-target", superseded.Items[0].Outcome)

	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "reasoner")
	require.NoError(err)
	require.Len(result.Items, 2)
	assert.Equal("dropped-invalid", result.Items[0].Outcome)
	require.NotNil(result.Items[0].Failure)
	assert.Equal("premise-not-live", result.Items[0].Failure.Reason)
	assert.Equal("created", result.Items[1].Outcome)

	counterDetail, err := f.store.GetPersonObservation(t.Context(), counterID)
	require.NoError(err)
	assert.Equal(personobs.StatusSuperseded, counterDetail.Status)
	createdDetail, err := f.store.GetPersonObservation(t.Context(), result.Items[1].ObservationID)
	require.NoError(err)
	assert.Equal("[PERSON] chooses tea", createdDetail.Content)
}

func TestApplyObservationBatchDoesNotSupersedeFromWithdrawnRetainedPremise(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")

	firstInput := in
	firstInput.Proposals = append([]personobs.ProposedObservation(nil), in.Proposals...)
	firstInput.Proposals[0].Content = "[PERSON] exercises outdoors"
	first := applyObservationInput(t, f.store, firstInput, generation)
	require.Len(first.Items, 1)
	firstID := first.Items[0].ObservationID

	secondSupport, _ := observationSupportForMessage(t, f, "retained-premise-second", "I swim", "I swim", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	secondInput := in
	secondInput.Proposals = append([]personobs.ProposedObservation(nil), in.Proposals...)
	secondInput.Proposals[0].Content = "[PERSON] swims regularly"
	secondInput.Proposals[0].Support = []personobs.SupportInput{secondSupport}
	second := applyObservationInput(t, f.store, secondInput, generation)
	require.Len(second.Items, 1)
	secondID := second.Items[0].ObservationID

	thirdSupport, _ := observationSupportForMessage(t, f, "retained-premise-third", "I hike", "I hike", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	thirdInput := in
	thirdInput.Proposals = append([]personobs.ProposedObservation(nil), in.Proposals...)
	thirdInput.Proposals[0].Content = "[PERSON] hikes regularly"
	thirdInput.Proposals[0].Support = []personobs.SupportInput{thirdSupport}
	third := applyObservationInput(t, f.store, thirdInput, generation)
	require.Len(third.Items, 1)
	thirdID := third.Items[0].ObservationID

	oldSupport, _ := observationSupportForMessage(t, f, "retained-premise-old-target", "I stretch", "I stretch", time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC))
	oldInput := in
	oldInput.Proposals = append([]personobs.ProposedObservation(nil), in.Proposals...)
	oldInput.Proposals[0].Content = "[PERSON] stretches every morning"
	oldInput.Proposals[0].Support = []personobs.SupportInput{oldSupport}
	old := applyObservationInput(t, f.store, oldInput, generation)
	require.Len(old.Items, 1)
	oldID := old.Items[0].ObservationID

	validFrom := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	derivedInput := in
	derivedInput.Proposals = []personobs.ProposedObservation{{
		PersonID: in.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
		Kind: personobs.KindOther, Content: "[PERSON] enjoys regular exercise", ConfidenceBP: 800,
		ValidFrom: &validFrom,
		Premises: []personobs.PremiseInput{
			{ID: firstID, Role: personobs.PremiseRoleSupport},
			{ID: secondID, Role: personobs.PremiseRoleSupport},
		},
	}}
	derived := applyObservationInput(t, f.store, derivedInput, generation)
	require.Len(derived.Items, 1)
	derivedID := derived.Items[0].ObservationID

	reinforcement := derivedInput.Proposals[0]
	reinforcement.ReinforcesID = &derivedID
	reinforcement.Premises = []personobs.PremiseInput{{ID: thirdID, Role: personobs.PremiseRoleSupport}}
	reinforcement.SupersedesIDs = []int64{oldID}
	reinforcement.ValidFrom = nil
	derivedInput.Proposals = []personobs.ProposedObservation{reinforcement}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), derivedInput)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)

	ref, err := peoplesweep.DecodePersonSweepEvidenceRef(firstInput.Proposals[0].Support[0].Evidence.SourceRef)
	require.NoError(err)
	withdrawObservationEvidence(t, f.store, in.PersonID, firstID, ref.MessageID)
	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "reasoner")
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("reinforced", result.Items[0].Outcome)
	assert.Empty(result.Items[0].AffectedIDs)

	oldDetail, err := f.store.GetPersonObservation(t.Context(), oldID)
	require.NoError(err)
	assert.Equal(personobs.StatusActive, oldDetail.Status)
	derivedDetail, err := f.store.GetPersonObservation(t.Context(), derivedID)
	require.NoError(err)
	require.NotNil(derivedDetail.ValidFrom)
	assert.Equal(validFrom, *derivedDetail.ValidFrom)
}

func TestApplyObservationBatchBlocksSupersedingSupportAncestor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		transitive bool
	}{
		{name: "direct premise"},
		{name: "transitive ancestor", transitive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, rootInput, generation := observationBatchFixture(t, "I run")
			root := applyObservationInput(t, f.store, rootInput, generation)
			require.Len(root.Items, 1)
			rootID := root.Items[0].ObservationID

			premiseID := rootID
			if tc.transitive {
				derived := rootInput
				derived.Proposals = []personobs.ProposedObservation{{
					PersonID: rootInput.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
					Kind: personobs.KindOther, Content: "[PERSON] keeps a regular routine", ConfidenceBP: 800,
					Premises: []personobs.PremiseInput{{ID: rootID, Role: personobs.PremiseRoleSupport}},
				}}
				derivedResult := applyObservationInput(t, f.store, derived, generation)
				require.Len(derivedResult.Items, 1)
				premiseID = derivedResult.Items[0].ObservationID
			}

			candidate := rootInput
			candidate.Proposals = []personobs.ProposedObservation{{
				PersonID: rootInput.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
				Kind: personobs.KindOther, Content: "[PERSON] trains every morning", ConfidenceBP: 800,
				Premises:      []personobs.PremiseInput{{ID: premiseID, Role: personobs.PremiseRoleSupport}},
				SupersedesIDs: []int64{rootID},
			}}
			result := applyObservationInput(t, f.store, candidate, generation)
			require.Len(result.Items, 1)
			assert.Equal("supersede-blocked", result.Items[0].Outcome)
			assert.Contains(result.Items[0].AffectedIDs, rootID)
			candidateID := result.Items[0].ObservationID

			rootDetail, err := f.store.GetPersonObservation(t.Context(), rootID)
			require.NoError(err)
			assert.Equal(personobs.StatusActive, rootDetail.Status)
			candidateDetail, err := f.store.GetPersonObservation(t.Context(), candidateID)
			require.NoError(err)
			assert.Equal(personobs.StatusActive, candidateDetail.Status)

			next := rootInput
			next.Proposals = []personobs.ProposedObservation{{
				PersonID: rootInput.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
				Kind: personobs.KindOther, Content: "[PERSON] plans a morning walk", ConfidenceBP: 800,
				Premises: []personobs.PremiseInput{{ID: candidateID, Role: personobs.PremiseRoleSupport}},
			}}
			nextResult := applyObservationInput(t, f.store, next, generation)
			require.Len(nextResult.Items, 1)
			assert.Equal("created", nextResult.Items[0].Outcome)
		})
	}
}

func TestApplyObservationBatchInvalidSupersessionIntervalKeepsValidSiblingAndSuppression(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	targetStart := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	targetResult := applyObservationInput(t, f.store, personobs.BatchInput{
		PersonID: in.PersonID,
		Proposals: []personobs.ProposedObservation{{
			PersonID: in.PersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner,
			Kind: personobs.KindState, Content: "[PERSON] starts a later schedule", ConfidenceBP: 1000,
			ValidFrom: &targetStart,
		}},
	}, 0)
	require.Len(targetResult.Items, 1)
	targetID := targetResult.Items[0].ObservationID

	earlyStart := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	invalid := in.Proposals[0]
	invalid.Content = "[PERSON] runs before the later schedule"
	invalid.ValidFrom = &earlyStart
	invalid.SupersedesIDs = []int64{targetID}
	suppressionKey := personobs.ContentKey(invalid.Level, invalid.SubjectRef, invalid.Content)
	rejectedAt := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at) VALUES(?,?,'outdated','owner',?)`),
		in.PersonID, suppressionKey, rejectedAt)
	require.NoError(err)
	var suppressionID int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT id FROM person_observation_suppressions WHERE person_id=? AND content_key=?`),
		in.PersonID, suppressionKey).Scan(&suppressionID))
	valid := in.Proposals[0]
	valid.Content = "[PERSON] enjoys morning exercise"
	in.Proposals = []personobs.ProposedObservation{invalid, valid}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Len(batch.Items, 2)
	require.Nil(batch.Items[0].Failure)
	require.Nil(batch.Items[1].Failure)

	result, err := f.store.ApplyPersonObservationBatch(t.Context(), batch, generation, "reasoner")
	require.NoError(err)
	require.Len(result.Items, 2)
	assert.Equal("dropped-invalid", result.Items[0].Outcome)
	require.NotNil(result.Items[0].Failure)
	assert.Equal("supersedes_ids", result.Items[0].Failure.Field)
	assert.Equal("target-validity", result.Items[0].Failure.Reason)
	assert.Equal("created", result.Items[1].Outcome)

	target, err := f.store.GetPersonObservation(t.Context(), targetID)
	require.NoError(err)
	assert.Equal(personobs.StatusActive, target.Status)
	assert.Nil(target.SupersededByID)
	var invalidRows int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM person_observations WHERE person_id=? AND content=?`),
		in.PersonID, "[PERSON] runs before the later schedule").Scan(&invalidRows))
	assert.Zero(invalidRows)
	var activeSuppressions int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM person_observation_suppressions WHERE id=? AND cleared_at IS NULL`), suppressionID).Scan(&activeSuppressions))
	assert.Equal(1, activeSuppressions)
	var liftEvents int
	detail := "outdated-new-support:suppression:" + strconv.FormatInt(suppressionID, 10)
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM person_observation_events WHERE person_id=? AND event='suppression-lifted' AND detail=?`),
		in.PersonID, detail).Scan(&liftEvents))
	assert.Zero(liftEvents)
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
