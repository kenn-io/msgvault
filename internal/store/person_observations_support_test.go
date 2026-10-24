package store_test

import (
	"context"
	"fmt"
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

func observationStatusGeneration(t *testing.T, st *store.Store, personID, id int64, supported bool, sequence int) {
	t.Helper()
	observationStatusGenerationContext(t.Context(), t, st, personID, id, supported, sequence)
}
func observationStatusGenerationContext(ctx context.Context, t *testing.T, st *store.Store, personID, id int64, supported bool, sequence int) {
	t.Helper()
	_, err := st.ApplyPersonFactGenerationContext(ctx, observationStatusInput(t, st, personID, id, supported, sequence), nil)
	require.NoError(t, err)
}
func observationStatusInput(t *testing.T, st *store.Store, personID, id int64, supported bool, sequence int) personfacts.GenerationInput {
	t.Helper()
	detail, err := st.GetPersonObservation(t.Context(), id)
	require.NoError(t, err)
	require.NotEmpty(t, detail.Evidence)
	var key, version string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT evidence_key,source_version FROM person_fact_evidence WHERE id=?`), detail.Evidence[0].EvidenceID).Scan(&key, &version))
	reason := personfacts.EvidenceStatusSourceDeleted
	if supported {
		reason = personfacts.EvidenceStatusSourceReimported
	}
	return personfacts.GenerationInput{
		PersonID: personID, SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: strconv.Itoa(sequence), End: fmt.Sprintf("%d-end", sequence)}},
		ProgramID: "observation-status", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "catalog", Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1", ResolvedAt: time.Date(2026, 10, 10, 0, 0, sequence, 0, time.UTC), Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "policy"},
		EvidenceStatusChanges: []personfacts.EvidenceStatusChange{{EvidenceKey: key, SourceVersion: version, Supported: supported, Reason: reason}},
	}
}
func observationDerived(t *testing.T, st *store.Store, in personobs.BatchInput, generation int64, level personobs.Level, content string, premises ...int64) int64 {
	t.Helper()
	kind := personobs.KindState
	if level == personobs.LevelInductive {
		kind = personobs.KindPattern
	}
	proposal := personobs.ProposedObservation{PersonID: in.PersonID, Level: level, Origin: personobs.OriginReasoning, Kind: kind, Content: content, ConfidenceBP: 800}
	for _, id := range premises {
		proposal.Premises = append(proposal.Premises, personobs.PremiseInput{ID: id, Role: personobs.PremiseRoleSupport})
	}
	in.Proposals = []personobs.ProposedObservation{proposal}
	result := applyObservationInput(t, st, in, generation)
	require.Len(t, result.Items, 1)
	require.Positive(t, result.Items[0].ObservationID)
	return result.Items[0].ObservationID
}
func observationGet(t *testing.T, st *store.Store, id int64) personobs.Detail {
	t.Helper()
	detail, err := st.GetPersonObservation(t.Context(), id)
	require.NoError(t, err)
	return detail
}
func TestObservationHealingUnsupportsOnSourceDeleted(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	observationStatusGeneration(t, f.store, in.PersonID, id, false, 1)
	detail := observationGet(t, f.store, id)
	assert.Equal(t, personobs.StatusUnsupported, detail.Status)
	assert.Equal(t, personobs.StatusReasonEvidenceUnsupported, detail.StatusReason)
	assert.Zero(t, detail.SupportedCount)
}
func TestObservationHealingRestoresOnReimport(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	observationStatusGeneration(t, f.store, in.PersonID, id, false, 1)
	observationStatusGeneration(t, f.store, in.PersonID, id, true, 2)
	detail := observationGet(t, f.store, id)
	assert.Equal(t, personobs.StatusActive, detail.Status)
	assert.Empty(t, detail.StatusReason)
	assert.Equal(t, 1, detail.SupportedCount)
}
func TestObservationHealingPinnedRowFlagsAndNotifies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	derived := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", id)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_observations SET pinned=TRUE WHERE id=?`), id)
	require.NoError(err)
	observationStatusGeneration(t, f.store, in.PersonID, id, false, 1)
	root := observationGet(t, f.store, id)
	assert.Equal(personobs.StatusActive, root.Status)
	assert.Equal(personobs.SupportFlagLapsed, root.SupportFlag)
	child := observationGet(t, f.store, derived)
	assert.Equal(personobs.StatusUnsupported, child.Status)
	assert.NotNil(child.PremiseChangedAt)
	observationStatusGeneration(t, f.store, in.PersonID, id, true, 2)
	assert.Empty(observationGet(t, f.store, id).SupportFlag)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, derived).Status)
}
func TestObservationHealingRestoresProtectedDerivedRows(t *testing.T) {
	for _, tc := range []struct {
		name      string
		updateSQL string
		pinned    bool
		owner     personobs.OwnerState
	}{
		{name: "pinned", updateSQL: `UPDATE person_observations SET pinned=TRUE WHERE id=?`, pinned: true, owner: personobs.OwnerStateNone},
		{name: "confirmed", updateSQL: `UPDATE person_observations SET owner_state='confirmed' WHERE id=?`, owner: personobs.OwnerStateConfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			root := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			child := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", root)
			grandchild := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] stays active", child)

			observationStatusGeneration(t, f.store, in.PersonID, root, false, 1)
			withdrawnChild := observationGet(t, f.store, child)
			require.Equal(personobs.StatusUnsupported, withdrawnChild.Status)
			require.Equal(personobs.StatusReasonPremisesWithdrawn, withdrawnChild.StatusReason)
			require.Equal(personobs.StatusUnsupported, observationGet(t, f.store, grandchild).Status)

			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(tc.updateSQL), child)
			require.NoError(err)
			protected := observationGet(t, f.store, child)
			require.Equal(tc.pinned, protected.Pinned)
			require.Equal(tc.owner, protected.OwnerState)
			observationStatusGeneration(t, f.store, in.PersonID, root, true, 2)

			recoveredChild := observationGet(t, f.store, child)
			assert.Equal(personobs.StatusActive, recoveredChild.Status)
			assert.Empty(recoveredChild.StatusReason)
			assert.Equal(personobs.StatusActive, observationGet(t, f.store, grandchild).Status)
		})
	}
}
func TestObservationDeductiveConjunctiveWithdrawal(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	first := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	owner := personobs.BatchInput{PersonID: in.PersonID, Proposals: []personobs.ProposedObservation{{Level: personobs.LevelExplicit, Origin: personobs.OriginOwner, Kind: personobs.KindState, Content: "[PERSON] swims", ConfidenceBP: 1000}}}
	second := applyObservationInput(t, f.store, owner, 0).Items[0].ObservationID
	child := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] does both", first, second)
	observationStatusGeneration(t, f.store, in.PersonID, first, false, 1)
	assert.Equal(t, personobs.StatusUnsupported, observationGet(t, f.store, child).Status)
	observationStatusGeneration(t, f.store, in.PersonID, first, true, 2)
	assert.Equal(t, personobs.StatusActive, observationGet(t, f.store, child).Status)
}
func TestObservationOwnerPremiseSupportsDeduction(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	owner := personobs.BatchInput{PersonID: in.PersonID, Proposals: []personobs.ProposedObservation{{Level: personobs.LevelExplicit, Origin: personobs.OriginOwner, Kind: personobs.KindState, Content: "[PERSON] swims", ConfidenceBP: 1000}}}
	id := applyObservationInput(t, f.store, owner, 0).Items[0].ObservationID
	child := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", id)
	assert.Equal(t, personobs.StatusActive, observationGet(t, f.store, child).Status)
}
func TestObservationTraversalDeeperThanTwo(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	chain := []int64{id}
	for i := range 5 {
		chain = append(chain, observationDerived(t, f.store, in, generation, personobs.LevelDeductive, fmt.Sprintf("[PERSON] has consequence %d", i), chain[len(chain)-1]))
	}
	observationStatusGeneration(t, f.store, in.PersonID, id, false, 1)
	for _, child := range chain[1:] {
		assert.Equal(t, personobs.StatusUnsupported, observationGet(t, f.store, child).Status)
	}
	observationStatusGeneration(t, f.store, in.PersonID, id, true, 2)
	for _, child := range chain[1:] {
		assert.Equal(t, personobs.StatusActive, observationGet(t, f.store, child).Status)
	}
}
func TestObservationLaterExclusionUnsupports(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	root := observationGet(t, f.store, id)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id='list.example.test' WHERE id=?`), root.Evidence[0].Leaf.MessageID)
	require.NoError(err)
	observationStatusGeneration(t, f.store, in.PersonID, id, true, 1)
	root = observationGet(t, f.store, id)
	assert.Zero(root.SupportedCount)
	assert.Equal(personobs.StatusUnsupported, root.Status)
	assert.Equal(personobs.ExclusionReasonList, root.Evidence[0].Exclusion)
	assert.Zero(root.MentionCount)
	assert.Nil(root.FirstSeenAt)
	assert.Nil(root.LastSeenAt)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id='' WHERE id=?`), root.Evidence[0].Leaf.MessageID)
	require.NoError(err)
	observationStatusGeneration(t, f.store, in.PersonID, id, true, 2)
	root = observationGet(t, f.store, id)
	assert.Equal(1, root.MentionCount)
	require.NotNil(root.FirstSeenAt)
	require.NotNil(root.LastSeenAt)
	assert.Equal(root.Evidence[0].Leaf.EventTime, *root.FirstSeenAt)
	assert.Equal(root.Evidence[0].Leaf.EventTime, *root.LastSeenAt)
}

func TestObservationCitationExclusionKeepsExplicitStatusAndWithdrawsDependents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	premiseID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	childID := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", premiseID)

	root := observationGet(t, f.store, premiseID)
	messageID := root.Evidence[0].Leaf.MessageID
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id=? WHERE id=?`), "list.example.test", messageID)
	require.NoError(err)

	derived := in
	derived.Proposals = []personobs.ProposedObservation{{
		PersonID: in.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
		Kind: personobs.KindOther, Content: "[PERSON] has a running routine", ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{{ID: premiseID, Role: personobs.PremiseRoleSupport}},
	}}
	result := applyObservationInput(t, f.store, derived, generation)
	require.Len(result.Items, 1)
	assert.Equal("dropped-invalid", result.Items[0].Outcome)
	require.NotNil(result.Items[0].Failure)
	assert.Equal("premise-not-supported", result.Items[0].Failure.Reason)

	root = observationGet(t, f.store, premiseID)
	assert.Equal(personobs.StatusActive, root.Status)
	assert.Zero(root.SupportedCount)
	assert.Equal(personobs.ExclusionReasonList, root.Evidence[0].Exclusion)
	assert.Equal(personobs.StatusUnsupported, observationGet(t, f.store, childID).Status)
}

func TestObservationHardDeletedMessageRetainsExclusion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		listID    string
		draft     bool
		exclusion personobs.ExclusionReason
	}{
		{name: "list", listID: "list.example.test", exclusion: personobs.ExclusionReasonList},
		{name: "draft", draft: true, exclusion: personobs.ExclusionReasonDraft},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			in.Proposals[0].Support = append(in.Proposals[0].Support, observationNewSupport(t, f, "support-survivor", "I still run"))
			applyObservationInput(t, f.store, in, generation)
			root := observationGet(t, f.store, id)
			require.Len(root.Evidence, 2)
			messageID := root.Evidence[0].Leaf.MessageID
			var survivingEventTime time.Time
			for _, citation := range root.Evidence {
				if citation.Leaf.MessageID != messageID {
					survivingEventTime = citation.Leaf.EventTime
				}
			}

			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id=?,draft_authored=? WHERE id=?`), tc.listID, tc.draft, messageID)
			require.NoError(err)
			observationStatusGeneration(t, f.store, in.PersonID, id, true, 1)
			assert.Equal(tc.exclusion, observationGet(t, f.store, id).Evidence[0].Exclusion)

			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`DELETE FROM messages WHERE id=?`), messageID)
			require.NoError(err)
			observationStatusGeneration(t, f.store, in.PersonID, id, false, 2)

			root = observationGet(t, f.store, id)
			assert.Equal(tc.exclusion, root.Evidence[0].Exclusion)
			assert.Equal(1, root.SupportedCount)
			assert.Equal(1, root.MentionCount)
			assert.Equal(personobs.StatusActive, root.Status)
			require.NotNil(root.FirstSeenAt)
			require.NotNil(root.LastSeenAt)
			assert.Equal(survivingEventTime, *root.FirstSeenAt)
			assert.Equal(survivingEventTime, *root.LastSeenAt)
		})
	}
}

func TestObservationReinforcementAddsCategoryAndPropagates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	first := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	second := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", first)
	third := observationDerived(t, f.store, in, generation, personobs.LevelInductive, "[PERSON] often exercises", first, second)
	in.Proposals[0].ModelCategories = []personobs.Category{personobs.CategoryHealth}
	applyObservationInput(t, f.store, in, generation)
	for _, id := range []int64{first, second, third} {
		detail := observationGet(t, f.store, id)
		assert.Contains(detail.Categories, personobs.CategoryHealth)
		assert.True(detail.Sensitive)
	}
	var events int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_events WHERE observation_id=? AND event='sensitivity-changed'`), first).Scan(&events))
	assert.Equal(1, events)
}

func TestObservationCounterPremiseCategoryPropagatesToExistingContradiction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	supportID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID

	counter := in.Proposals[0]
	counter.Content = "[PERSON] does not run"
	counter.Support = []personobs.SupportInput{observationNewSupport(t, f, "counter-run", "I do not run")}
	counterInput := in
	counterInput.Proposals = []personobs.ProposedObservation{counter}
	counterID := applyObservationInput(t, f.store, counterInput, generation).Items[0].ObservationID

	contradictionInput := in
	contradictionInput.Proposals = []personobs.ProposedObservation{{
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
	contradictionID := applyObservationInput(t, f.store, contradictionInput, generation).Items[0].ObservationID
	contradiction := observationGet(t, f.store, contradictionID)
	assert.Empty(contradiction.Categories)
	assert.False(contradiction.Sensitive)

	counter.ModelCategories = []personobs.Category{personobs.CategoryHealth}
	counter.ReinforcesID = &counterID
	counterInput.Proposals = []personobs.ProposedObservation{counter}
	result := applyObservationInput(t, f.store, counterInput, generation)
	require.Len(result.Items, 1)
	assert.Equal("reinforced", result.Items[0].Outcome)

	contradiction = observationGet(t, f.store, contradictionID)
	assert.Contains(contradiction.Categories, personobs.CategoryHealth)
	assert.True(contradiction.Sensitive)
	var events int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_events WHERE observation_id=? AND event='sensitivity-changed'`), contradictionID).Scan(&events))
	assert.Equal(1, events)

	page, err := f.store.ListPersonObservations(t.Context(), in.PersonID, personobs.Filter{})
	require.NoError(err)
	assert.NotContains(observationIDs(page.Items), contradictionID)
	page, err = f.store.ListPersonObservations(t.Context(), in.PersonID, personobs.Filter{IncludeSensitive: true})
	require.NoError(err)
	assert.Contains(observationIDs(page.Items), contradictionID)
}

func TestObservationReinforcementPropagatesStandaloneSensitivity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	rootID := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	childID := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", rootID)
	grandchildID := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] has an active routine", childID)

	in.Proposals[0].Sensitive = true
	in.Proposals[0].ReinforcesID = &rootID
	result := applyObservationInput(t, f.store, in, generation)
	require.Len(result.Items, 1)
	assert.Equal("reinforced", result.Items[0].Outcome)

	for _, id := range []int64{childID, grandchildID} {
		detail := observationGet(t, f.store, id)
		assert.True(detail.Sensitive)
		assert.Empty(detail.Categories)
		var events int
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_events WHERE observation_id=? AND event='sensitivity-changed'`), id).Scan(&events))
		assert.Equal(1, events)
	}

	page, err := f.store.ListPersonObservations(t.Context(), in.PersonID, personobs.Filter{})
	require.NoError(err)
	assert.NotContains(observationIDs(page.Items), childID)
	assert.NotContains(observationIDs(page.Items), grandchildID)
	page, err = f.store.ListPersonObservations(t.Context(), in.PersonID, personobs.Filter{IncludeSensitive: true})
	require.NoError(err)
	assert.Contains(observationIDs(page.Items), childID)
	assert.Contains(observationIDs(page.Items), grandchildID)
}

func observationNewSupport(t *testing.T, f personSweepJournalFixture, name, text string) personobs.SupportInput {
	t.Helper()
	id := f.insertMessage(t, name, "email", f.aliceID, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, id, text)
	item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, id)
	evidence := sweepEvidenceInput(t, item)
	offset := strings.Index(evidence.Excerpt, text)
	require.NotEqual(t, -1, offset)
	return personobs.SupportInput{Evidence: evidence, Quote: text, QuoteStart: len([]rune(evidence.Excerpt[:offset]))}
}
func TestObservationHealingPartialLossNotifiesTrait(t *testing.T) {
	assert := assert.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	first := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	in.Proposals[0].Support = []personobs.SupportInput{observationNewSupport(t, f, "partial-extra", "I still run")}
	applyObservationInput(t, f.store, in, generation)
	in.Proposals[0].Content = "[PERSON] swims"
	second := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	child := observationDerived(t, f.store, in, generation, personobs.LevelInductive, "[PERSON] enjoys exercise", first, second)
	observationStatusGeneration(t, f.store, in.PersonID, first, false, 1)
	root := observationGet(t, f.store, first)
	assert.Equal(1, root.SupportedCount)
	assert.Equal(personobs.StatusActive, root.Status)
	detail := observationGet(t, f.store, child)
	assert.Equal(personobs.StatusActive, detail.Status)
	assert.NotNil(detail.PremiseChangedAt)
}
func TestObservationReinforcementRestoresUnsupported(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	observationStatusGeneration(t, f.store, in.PersonID, id, false, 1)
	in.Proposals[0].Support = []personobs.SupportInput{observationNewSupport(t, f, "reimport-extra", "I run again")}
	result := applyObservationInput(t, f.store, in, generation)
	assert.Equal(t, id, result.Items[0].ObservationID)
	assert.Equal(t, personobs.StatusActive, observationGet(t, f.store, id).Status)
}

func TestObservationTraversalChunkDrainsDependents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	const childCount = 501
	proposals := make([]personobs.ProposedObservation, childCount)
	for i := range proposals {
		proposals[i] = personobs.ProposedObservation{
			PersonID: in.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
			Kind: personobs.KindState, Content: fmt.Sprintf("[PERSON] has consequence %d", i), ConfidenceBP: 800,
			Premises: []personobs.PremiseInput{{ID: id, Role: personobs.PremiseRoleSupport}},
		}
	}
	childInput := in
	childInput.Proposals = proposals
	children := applyObservationInput(t, f.store, childInput, generation)
	require.Len(children.Items, childCount)
	// Drain several refresh chunks, including descendants beyond the former 500-row cap.
	observationStatusGenerationContext(f.store.ObservationTraversalBatchSizeContextForTest(t.Context(), 200), t, f.store, in.PersonID, id, false, 1)
	lastChild := observationGet(t, f.store, children.Items[len(children.Items)-1].ObservationID)
	assert.Equal(personobs.StatusUnsupported, lastChild.Status)
	assert.NotNil(lastChild.PremiseChangedAt)
}

func TestObservationHealingEventsUseStatusGeneration(t *testing.T) {
	f, in, creationGeneration := observationBatchFixture(t, "I run")
	root := applyObservationInput(t, f.store, in, creationGeneration).Items[0].ObservationID
	child := observationDerived(t, f.store, in, creationGeneration, personobs.LevelDeductive, "[PERSON] exercises", root)
	statusGeneration, err := f.store.ApplyPersonFactGenerationContext(t.Context(), observationStatusInput(t, f.store, in.PersonID, root, false, 1), nil)
	require.NoError(t, err)
	for _, id := range []int64{root, child} {
		var generation *int64
		require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT generation_id FROM person_observation_events WHERE observation_id=? AND event='unsupported' ORDER BY id DESC LIMIT 1`), id).Scan(&generation))
		assert.Equal(t, &statusGeneration.GenerationID, generation)
	}
}

func TestObservationExclusionRefreshesSeenRange(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	first := observationGet(t, f.store, id).Evidence[0].Leaf
	in.Proposals[0].Support = []personobs.SupportInput{observationNewSupport(t, f, "seen-range", "I still run")}
	applyObservationInput(t, f.store, in, generation)
	detail := observationGet(t, f.store, id)
	require.Len(t, detail.Evidence, 2)
	last := detail.Evidence[1].Leaf
	require.True(t, last.EventTime.After(first.EventTime))
	for index, step := range []struct {
		name       string
		excludedID int64
		listID     string
		want       time.Time
	}{
		{"exclude-earliest", first.MessageID, "list.example.test", last.EventTime},
		{"restore-earliest", first.MessageID, "", first.EventTime},
		{"exclude-latest", last.MessageID, "list.example.test", first.EventTime},
	} {
		t.Run(step.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id=? WHERE id=?`), step.listID, step.excludedID)
			require.NoError(err)
			observationStatusGeneration(t, f.store, in.PersonID, id, true, index+1)
			got := observationGet(t, f.store, id)
			require.NotNil(got.FirstSeenAt)
			require.NotNil(got.LastSeenAt)
			if step.listID == "" {
				assert.Equal(2, got.MentionCount)
				assert.Equal(first.EventTime, *got.FirstSeenAt)
				assert.Equal(last.EventTime, *got.LastSeenAt)
			} else {
				assert.Equal(1, got.MentionCount)
				assert.Equal(step.want, *got.FirstSeenAt)
				assert.Equal(step.want, *got.LastSeenAt)
			}
		})
	}
}
