package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
)

func TestObservationConcurrentDuplicateOnPostgres(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL advisory-lock contract")
	}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type outcome struct {
		result personobs.BatchResult
		err    error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			result, err := f.store.ApplyPersonObservationBatch(ctx, batch, generation, "test")
			results <- outcome{result, err}
		}()
	}
	close(start)
	var outcomes []string
	var ids []int64
	for range 2 {
		result := <-results
		require.NoError(result.err)
		require.Len(result.result.Items, 1)
		outcomes = append(outcomes, result.result.Items[0].Outcome)
		ids = append(ids, result.result.Items[0].ObservationID)
	}
	assert.ElementsMatch([]string{"created", "reinforced"}, outcomes)
	assert.Equal(ids[0], ids[1])
	detail := observationGetConcurrency(t, f.store, ids[0])
	assert.Len(detail.Events, 2)
}

func TestObservationAndEvidenceStatusWritesSharePersonLockOnPostgres(t *testing.T) {
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL observation/evidence status lock regression")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	created := applyObservationInput(t, f.store, in, generation)
	require.Len(created.Items, 1)
	targetID := created.Items[0].ObservationID
	ownerInput := personobs.BatchInput{PersonID: f.alicePersonID, Proposals: []personobs.ProposedObservation{{
		PersonID: f.alicePersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner,
		Kind: personobs.KindState, Content: in.Proposals[0].Content, ConfidenceBP: 1000, ReinforcesID: &targetID,
	}}}
	batch, err := f.store.PreparePersonObservationBatch(ctx, ownerInput)
	require.NoError(err)
	require.Len(batch.Items, 1)

	sourcesRevalidated := make(chan struct{})
	releaseApply := make(chan struct{})
	var signalOnce, releaseOnce sync.Once
	restoreBarrier := f.store.SetPersonObservationTxBarrierForTest(func(phase string) {
		if phase == "sources-revalidated" {
			signalOnce.Do(func() {
				close(sourcesRevalidated)
				<-releaseApply
			})
		}
	})
	t.Cleanup(restoreBarrier)
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseApply) }) })

	type applyOutcome struct {
		result personobs.BatchResult
		err    error
	}
	applyDone := make(chan applyOutcome, 1)
	go func() {
		result, err := f.store.ApplyPersonObservationBatch(ctx, batch, 0, "owner")
		applyDone <- applyOutcome{result: result, err: err}
	}()
	requireChannelSignal(t, sourcesRevalidated, "owner observation apply did not reach its locked person state")

	key, err := personfacts.EvidenceKey(in.Proposals[0].Support[0].Evidence)
	require.NoError(err)
	statusInput := personfacts.GenerationInput{
		PersonID:      f.alicePersonID,
		SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "observation-status-lock", End: "observation-status-lock-end"}},
		ProgramID:     "observation-status-lock", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("b", 64), CatalogFingerprint: "catalog-fixture",
		Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1",
		ResolvedAt: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
		Policy:     personfacts.PolicyContext{ProviderPolicyFingerprint: "policy-v1"},
		EvidenceStatusChanges: []personfacts.EvidenceStatusChange{{
			EvidenceKey: key, SourceVersion: in.Proposals[0].Support[0].Evidence.SourceVersion,
			Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted,
		}},
	}
	statusDone := make(chan error, 1)
	go func() {
		_, err := f.store.ApplyPersonFactGenerationContext(ctx, statusInput, nil)
		statusDone <- err
	}()
	waitForLockWait(t, f.store, "pg_advisory_xact_lock",
		"evidence status apply must wait for observation support validation")

	releaseOnce.Do(func() { close(releaseApply) })
	apply := <-applyDone
	require.NoError(apply.err)
	require.Len(apply.result.Items, 1)
	require.Equal("reinforced", apply.result.Items[0].Outcome)
	require.NoError(<-statusDone)
}

func observationGetConcurrency(t *testing.T, st *store.Store, id int64) personobs.Detail {
	t.Helper()
	require := require.New(t)
	detail, err := st.GetPersonObservation(t.Context(), id)
	require.NoError(err)
	return detail
}

func TestObservationApplyWaitsForPersonSplitIdentityLockOnPostgres(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL person split lock regression")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	messageID := f.insertMessage(t, "split-race", "email", f.bobID, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, messageID, "I run on weekends")
	survivor, err := f.store.GetPersonContext(ctx, f.alicePersonID)
	requirements.NoError(err)
	absorbed, err := f.store.GetPersonContext(ctx, f.bobPersonID)
	requirements.NoError(err)
	merged, err := f.store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "observation-split-lock-merge", Actor: "test",
	})
	requirements.NoError(err)

	item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, messageID)
	evidence := sweepEvidenceInput(t, item)
	quote := "I run on weekends"
	offset := strings.Index(evidence.Excerpt, quote)
	requirements.NotEqual(-1, offset)
	in.Proposals[0].Support = []personobs.SupportInput{{
		Evidence: evidence, Quote: quote, QuoteStart: len([]rune(evidence.Excerpt[:offset])),
	}}
	batch, err := f.store.PreparePersonObservationBatch(ctx, in)
	requirements.NoError(err)

	splitHasIdentityLock := make(chan struct{})
	releaseSplit := make(chan struct{})
	var splitOnce, releaseOnce sync.Once
	restoreHook := f.store.SetPersonOperationAfterIdentityLockHookForTest(func() {
		splitOnce.Do(func() { close(splitHasIdentityLock) })
		<-releaseSplit
	})
	t.Cleanup(restoreHook)
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseSplit) }) })

	type splitOutcome struct {
		result *store.PersonSplitResult
		err    error
	}
	splitDone := make(chan splitOutcome, 1)
	go func() {
		result, err := f.store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
			SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
			ParticipantIDs: []int64{f.bobID}, ExpectedSourceRevision: merged.Person.Revision,
			IdempotencyKey: "observation-split-lock-split", Actor: "test",
		})
		splitDone <- splitOutcome{result: result, err: err}
	}()
	requireChannelSignal(t, splitHasIdentityLock, "split did not take the identity lock")

	type applyOutcome struct {
		result personobs.BatchResult
		err    error
	}
	applyDone := make(chan applyOutcome, 1)
	go func() {
		result, err := f.store.ApplyPersonObservationBatch(ctx, batch, generation, "test")
		applyDone <- applyOutcome{result: result, err: err}
	}()
	waitForLockWait(t, f.store, "archive_metadata", "observation apply must wait for the split identity lock")

	releaseOnce.Do(func() { close(releaseSplit) })
	split := <-splitDone
	requirements.NoError(split.err)
	requirements.NotNil(split.result)
	apply := <-applyDone
	requirements.NoError(apply.err)
	requirements.Len(apply.result.Items, 1)
	assertions.Equal("dropped-invalid", apply.result.Items[0].Outcome)
	requirements.NotNil(apply.result.Items[0].Failure)
	assertions.Equal("unaligned-evidence", apply.result.Items[0].Failure.Reason)

	var observations int
	requirements.NoError(f.store.DB().QueryRowContext(ctx, f.store.Rebind(`SELECT COUNT(*)
		FROM person_observations WHERE person_id=? AND content=?`),
		f.alicePersonID, in.Proposals[0].Content).Scan(&observations))
	assertions.Zero(observations)
}

func TestObservationApplySharesIdentityFenceForDifferentPeopleOnPostgres(t *testing.T) {
	requirements := require.New(t)
	f, aliceInput, aliceGeneration := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL shared identity fence regression")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err := f.store.SetPersonTrackingContext(ctx, f.bobPersonID, true)
	requirements.NoError(err)

	aliceBatch, err := f.store.PreparePersonObservationBatch(ctx, aliceInput)
	requirements.NoError(err)
	bobMessageID := f.insertMessage(t, "identity-share-bob", "email", f.bobID, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, bobMessageID, "I travel")
	bobWindow, err := f.store.LoadPersonSweepWindow(ctx, peoplesweep.WindowRequest{
		PersonID: f.bobPersonID, Lane: peoplesweep.SourceConversationText,
		AfterSequence: 0, ThroughSequence: latestPersonSweepSequence(t, f.store), Limit: 100,
	})
	requirements.NoError(err)
	bobItem := sweepItem(t, bobWindow.Seeds, bobMessageID)
	bobEvidence := sweepEvidenceInput(t, bobItem)
	bobQuote := "I travel"
	bobOffset := strings.Index(bobEvidence.Excerpt, bobQuote)
	requirements.NotEqual(-1, bobOffset)
	bobSupport := personobs.SupportInput{
		Evidence: bobEvidence, Quote: bobQuote,
		QuoteStart: utf8.RuneCountInString(bobEvidence.Excerpt[:bobOffset]),
	}
	bobProposal := aliceInput.Proposals[0]
	bobProposal.PersonID = f.bobPersonID
	bobProposal.Content = "[PERSON] travels regularly"
	bobProposal.Support = []personobs.SupportInput{bobSupport}
	bobGenerationInput := *aliceInput.Generation
	bobGenerationInput.PersonID = f.bobPersonID
	bobInput := aliceInput
	bobInput.PersonID = f.bobPersonID
	bobInput.Generation = &bobGenerationInput
	bobInput.Proposals = []personobs.ProposedObservation{bobProposal}
	bobGeneration := insertPersonFactSchemaGeneration(t, f.store, f.bobPersonID, "identity-share-bob-generation")
	bobBatch, err := f.store.PreparePersonObservationBatch(ctx, bobInput)
	requirements.NoError(err)

	firstAtFence := make(chan struct{})
	secondAtFence := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	var countMu sync.Mutex
	lockCount := 0
	restoreBarrier := f.store.SetPersonObservationTxBarrierForTest(func(phase string) {
		if phase != "identity-locked" {
			return
		}
		countMu.Lock()
		lockCount++
		current := lockCount
		countMu.Unlock()
		switch current {
		case 1:
			close(firstAtFence)
			<-releaseFirst
		case 2:
			close(secondAtFence)
		}
	})
	t.Cleanup(restoreBarrier)
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseFirst) }) })

	type applyOutcome struct {
		result personobs.BatchResult
		err    error
	}
	results := make(chan applyOutcome, 2)
	go func() {
		result, err := f.store.ApplyPersonObservationBatch(ctx, aliceBatch, aliceGeneration, "test")
		results <- applyOutcome{result: result, err: err}
	}()
	requireChannelSignal(t, firstAtFence, "first observation apply did not take the shared identity fence")
	go func() {
		result, err := f.store.ApplyPersonObservationBatch(ctx, bobBatch, bobGeneration, "test")
		results <- applyOutcome{result: result, err: err}
	}()
	requireChannelSignal(t, secondAtFence, "different-person observation apply waited for the first identity reader")

	releaseOnce.Do(func() { close(releaseFirst) })
	for range 2 {
		outcome := <-results
		requirements.NoError(outcome.err)
		requirements.Len(outcome.result.Items, 1)
		requirements.Equal("created", outcome.result.Items[0].Outcome)
	}
}

func TestObservationApplyRechecksNestedSupportAfterClosingPremiseRowsOnPostgres(t *testing.T) {
	requirements := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL nested observation support query regression")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	root := applyObservationInput(t, f.store, in, generation)
	requirements.Len(root.Items, 1)
	rootID := root.Items[0].ObservationID
	firstInput := in
	firstInput.Proposals = []personobs.ProposedObservation{{
		PersonID: in.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
		Kind: personobs.KindOther, Content: "[PERSON] keeps a routine", ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{{ID: rootID, Role: personobs.PremiseRoleSupport}},
	}}
	first := applyObservationInput(t, f.store, firstInput, generation)
	requirements.Len(first.Items, 1)
	firstID := first.Items[0].ObservationID
	secondInput := in
	secondInput.Proposals = []personobs.ProposedObservation{{
		PersonID: in.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
		Kind: personobs.KindOther, Content: "[PERSON] exercises routinely", ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{{ID: firstID, Role: personobs.PremiseRoleSupport}},
	}}
	second := applyObservationInput(t, f.store, secondInput, generation)
	requirements.Len(second.Items, 1)

	thirdInput := in
	thirdInput.Proposals = []personobs.ProposedObservation{{
		PersonID: in.PersonID, Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning,
		Kind: personobs.KindOther, Content: "[PERSON] stays active", ConfidenceBP: 800,
		Premises: []personobs.PremiseInput{{ID: second.Items[0].ObservationID, Role: personobs.PremiseRoleSupport}},
	}}
	thirdBatch, err := f.store.PreparePersonObservationBatch(ctx, thirdInput)
	requirements.NoError(err)
	result, err := f.store.ApplyPersonObservationBatch(ctx, thirdBatch, generation, "reasoner")
	requirements.NoError(err)
	requirements.Len(result.Items, 1)
	requirements.Equal("created", result.Items[0].Outcome)
}

func TestObservationApplyHoldsReinforcedSourceLocksThroughCommitOnPostgres(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, reinforcementInput, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL observation source lock regression")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	ownerTarget := personobs.BatchInput{PersonID: f.alicePersonID, Proposals: []personobs.ProposedObservation{{
		PersonID: f.alicePersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner,
		Kind: personobs.KindState, Content: "[PERSON] runs", ConfidenceBP: 1000,
	}}}
	target := applyObservationInput(t, f.store, ownerTarget, 0)
	requirements.Len(target.Items, 1)
	targetID := target.Items[0].ObservationID
	ownerTarget.Proposals[0].Content = "[PERSON] walks"
	old := applyObservationInput(t, f.store, ownerTarget, 0)
	requirements.Len(old.Items, 1)
	oldID := old.Items[0].ObservationID

	reinforcementInput.Proposals[0].ReinforcesID = &targetID
	reinforcement, err := f.store.PreparePersonObservationBatch(ctx, reinforcementInput)
	requirements.NoError(err)
	requirements.Len(reinforcement.Items, 1)
	requirements.Nil(reinforcement.Items[0].Failure)
	_, err = f.store.ApplyPersonObservationBatch(ctx, reinforcement, generation, "reasoner")
	requirements.NoError(err)
	citation := reinforcement.Items[0].Citations[0].Leaf.MessageID
	requirements.Positive(citation)

	derivedInput := personobs.BatchInput{
		PersonID: f.alicePersonID, ProgramFingerprint: reinforcementInput.ProgramFingerprint,
		Generation: reinforcementInput.Generation,
		Actor:      "reasoner", Proposals: []personobs.ProposedObservation{{
			PersonID: f.alicePersonID, Level: personobs.LevelDeductive,
			Origin: personobs.OriginReasoning, Kind: personobs.KindPattern,
			Content: "[PERSON] often runs", ConfidenceBP: 800,
			Premises:      []personobs.PremiseInput{{ID: targetID, Role: personobs.PremiseRoleSupport}},
			SupersedesIDs: []int64{oldID},
		}},
	}
	derived, err := f.store.PreparePersonObservationBatch(ctx, derivedInput)
	requirements.NoError(err)
	requirements.Len(derived.Items, 1)
	requirements.Nil(derived.Items[0].Failure)

	sourcesRevalidated := make(chan struct{})
	releaseValidatedAttempt := make(chan struct{})
	var validatedOnce, releaseValidatedOnce sync.Once
	restoreBarrier := f.store.SetPersonObservationTxBarrierForTest(func(phase string) {
		if phase == "sources-revalidated" {
			validatedOnce.Do(func() {
				close(sourcesRevalidated)
				<-releaseValidatedAttempt
			})
		}
	})
	t.Cleanup(restoreBarrier)
	t.Cleanup(func() { releaseValidatedOnce.Do(func() { close(releaseValidatedAttempt) }) })

	type applyOutcome struct {
		result personobs.BatchResult
		err    error
	}
	applyDone := make(chan applyOutcome, 1)
	go func() {
		result, err := f.store.ApplyPersonObservationBatch(ctx, derived, generation, "reasoner")
		applyDone <- applyOutcome{result: result, err: err}
	}()
	requireChannelSignal(t, sourcesRevalidated, "apply did not revalidate the reinforced source")

	updateDone := make(chan error, 1)
	go func() {
		_, err := f.store.DB().ExecContext(ctx,
			f.store.Rebind(`UPDATE messages SET draft_authored=TRUE WHERE id=?`), citation)
		updateDone <- err
	}()
	waitForLockWait(t, f.store, "UPDATE messages SET draft_authored",
		"the refreshed source must stay locked through observation commit")
	releaseValidatedOnce.Do(func() { close(releaseValidatedAttempt) })
	apply := <-applyDone
	requirements.NoError(apply.err)
	requirements.Len(apply.result.Items, 1)
	assertions.Contains(apply.result.Items[0].AffectedIDs, oldID)
	requirements.NoError(<-updateDone)

	oldDetail := observationGetConcurrency(t, f.store, oldID)
	assertions.Equal(personobs.StatusSuperseded, oldDetail.Status)
}

func TestObservationApplyLocksPossibleContentKeyFallbackSourcesOnPostgres(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	testCase := newObservationSupersessionFallbackCase(t)
	if !testCase.fixture.store.IsPostgreSQL() {
		t.Skip("PostgreSQL observation fallback source lock regression")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	sourcesRevalidated := make(chan struct{})
	releaseApply := make(chan struct{})
	var barrierOnce, releaseOnce sync.Once
	restoreBarrier := testCase.fixture.store.SetPersonObservationTxBarrierForTest(func(phase string) {
		if phase == "sources-revalidated" {
			barrierOnce.Do(func() {
				close(sourcesRevalidated)
				<-releaseApply
			})
		}
	})
	t.Cleanup(restoreBarrier)
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseApply) }) })

	type applyOutcome struct {
		result personobs.BatchResult
		err    error
	}
	applyDone := make(chan applyOutcome, 1)
	go func() {
		result, err := testCase.fixture.store.ApplyPersonObservationBatch(
			ctx, testCase.batch, testCase.generationID, "extractor")
		applyDone <- applyOutcome{result: result, err: err}
	}()
	requireChannelSignal(t, sourcesRevalidated, "apply did not revalidate its locked fallback sources")

	updateDone := make(chan error, 1)
	go func() {
		_, err := testCase.fixture.store.DB().ExecContext(ctx,
			testCase.fixture.store.Rebind(`UPDATE messages SET draft_authored=TRUE WHERE id=?`),
			testCase.fallbackMessageID)
		updateDone <- err
	}()
	waitForLockWait(t, testCase.fixture.store, "UPDATE messages SET draft_authored",
		"the possible fallback evidence source must stay locked through observation commit")

	releaseOnce.Do(func() { close(releaseApply) })
	apply := <-applyDone
	requirements.NoError(apply.err)
	requirements.Len(apply.result.Items, 2)
	assertions.Equal("superseded-target", apply.result.Items[0].Outcome)
	assertions.Equal("reinforced", apply.result.Items[1].Outcome)
	assertions.Equal(testCase.fallbackID, apply.result.Items[1].ObservationID)
	requirements.NoError(<-updateDone)
}
