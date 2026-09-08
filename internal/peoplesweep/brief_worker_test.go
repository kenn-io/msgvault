package peoplesweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// briefCadenceDue is a contact cadence one day out, inside the default 72h
// pre-call window from the fixtures' clock.
var briefCadenceDue = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// briefLaneOff turns the whole brief lane off without unenrolling anybody.
var briefLaneOff = false

// briefFakeStore is the durable brief state a worker run reads.
type briefFakeStore struct {
	enrolled       bool
	eligibility    BriefEligibility
	highWater      int64
	changedAfter   int64
	cadenceDueAt   *time.Time
	ensuredWork    []int64
	trackedWork    bool
	eligibilityErr error
}

func (s *briefFakeStore) PersonSweepBriefEligibility(
	_ context.Context, personID int64,
) (BriefEligibility, bool, error) {
	if s.eligibilityErr != nil {
		return BriefEligibility{}, false, s.eligibilityErr
	}
	if !s.enrolled {
		return BriefEligibility{}, false, nil
	}
	eligibility := s.eligibility
	eligibility.PersonID = personID
	return eligibility, true, nil
}

func (s *briefFakeStore) PersonSweepCadenceDueAt(
	_ context.Context, _ int64, _ time.Time,
) (time.Time, bool, error) {
	if s.cadenceDueAt == nil {
		return time.Time{}, false, nil
	}
	return *s.cadenceDueAt, true, nil
}

func (s *briefFakeStore) HasPersonSweepChangesAfter(
	_ context.Context, _ int64, sequence int64,
) (bool, error) {
	return s.changedAfter > sequence, nil
}

func (s *briefFakeStore) LatestPersonSweepChangeSequence(context.Context) (int64, error) {
	return s.highWater, nil
}

func (s *briefFakeStore) EnsurePersonSweepWork(_ context.Context, personID int64, _ bool) (bool, error) {
	s.ensuredWork = append(s.ensuredWork, personID)
	return s.trackedWork, nil
}

// briefWorkerArchiveItems is the recent stretch of conversation every brief
// worker case summarizes.
func briefWorkerArchiveItems() []EvidenceItem {
	return []EvidenceItem{
		briefWindowItem(80, SourceConversationText, 20, "they finished the move"),
		briefWindowItem(81, SourceConversationText, 22, "they start the new role in September"),
	}
}

// briefWorkerCandidate is a valid brief the provider could return over the
// archive items above.
func briefWorkerCandidate(items []EvidenceItem) json.RawMessage {
	newest := packetEvidenceID(items[len(items)-1])
	return briefOutputJSON{
		LastInteraction: fmt.Sprintf(
			`{"evidence_id":%q,"summary":"they start a new role in September"}`, newest),
		Highlights: []string{
			briefHighlightJSON("they start a new role in September", BriefSpeakerPerson, "", newest),
		},
		FollowUps: []string{fmt.Sprintf(
			`{"question":"how the first week went","why":"they were about to start",`+
				`"highlight_index":0,"evidence_ids":[%q]}`, newest)},
	}.raw()
}

func briefDriverResponse(candidate json.RawMessage, requestID string) DriverResponse {
	return DriverResponse{CandidateJSON: candidate, ProviderRequestID: requestID,
		ProviderVersion: "provider-v1", ModelVersion: "model-v1",
		Usage: TokenUsage{InputTokens: 9, OutputTokens: 4}, UsageKnown: true}
}

type briefWorkerCase struct {
	mode        BriefMode
	brief       *briefFakeStore
	archive     *briefFakeArchive
	responses   []DriverResponse
	budget      func(*BudgetConfig)
	briefConfig func(*BriefConfig)
	// provider mutates the consented profile after the fixture has allowed
	// sensitive content, which is how a policy refusal is set up.
	provider func(*ProviderConfig)
	// denyReservation is the 1-based reservation the store refuses with a
	// budget denial, which is how a person budget runs out mid-attempt.
	denyReservation int
	// runRequest, when set, drives Worker.Run instead of RunPersonBrief so the
	// claim path is exercised too.
	runRequest *RunRequest
	// leaseHeld makes the store refuse every claim, which is how another
	// worker holding the person's lease looks to this one.
	leaseHeld  bool
	gapResults []GapResult
}

type briefWorkerOutcome struct {
	store   *workerFailureStore
	driver  *workerRepairDriver
	sink    *workerProductionSink
	archive *briefFakeArchive
	brief   *briefFakeStore
	result  PersonRunResult
	err     error
}

func runBriefWorkerCase(t *testing.T, testCase briefWorkerCase) briefWorkerOutcome {
	t.Helper()
	config, catalog := workerTestConfig(t)
	mutateTestProvider(&config, func(provider *ProviderConfig) { provider.AllowSensitive = true })
	config.Budgets.MaxRequestsPerPerson = 3
	config.Budgets.MaxRequestsPerRun = 3
	config.Budgets.MaxRequestsPerDay = 3
	config.Budgets.MaxInputTokensPerPerson = 4_000_000
	config.Budgets.MaxInputTokensPerRun = 4_000_000
	config.Budgets.MaxInputTokensPerDay = 4_000_000
	config.Budgets.MaxOutputTokensPerPerson = 3 * extractionMaxOutputTokens
	config.Budgets.MaxOutputTokensPerRun = 3 * extractionMaxOutputTokens
	config.Budgets.MaxOutputTokensPerDay = 3 * extractionMaxOutputTokens
	if testCase.budget != nil {
		testCase.budget(&config.Budgets)
	}
	if testCase.briefConfig != nil {
		testCase.briefConfig(&config.Brief)
	}
	if testCase.provider != nil {
		mutateTestProvider(&config, testCase.provider)
	}

	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store := &workerFailureStore{cursor: Cursor{ReconciliationComplete: true, LastBackstopAt: &now},
		reserveErrAt: testCase.denyReservation, gapResults: testCase.gapResults}
	seed := packetTestEvidence(71, SourceConversationText, "changed seed")
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {Seeds: []EvidenceItem{seed}, Changes: []ArchiveChange{{
			Sequence: 1, PersonID: 7, SourceLane: SourceConversationText}}, NextSequence: 1},
	}}
	driver := &workerRepairDriver{responses: testCase.responses}
	sink := &workerProductionSink{}
	archive := testCase.archive
	if archive == nil {
		archive = &briefFakeArchive{items: briefWorkerArchiveItems()}
	}
	lease := Lease{PersonID: 7, WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)}
	store.lease = &lease
	if testCase.leaseHeld {
		store.lease = nil
	}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source), Sink: sink,
		Runner: newWorkerRepairRunner(t, config, driver),
		Brief:  testCase.brief, Archive: archive,
		Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "attempt-brief" }, WorkerID: "worker-fixture"}

	outcome := briefWorkerOutcome{store: store, driver: driver, sink: sink,
		archive: archive, brief: testCase.brief}
	if testCase.runRequest != nil {
		_, outcome.err = worker.Run(t.Context(), *testCase.runRequest)
		return outcome
	}
	outcome.result, outcome.err = worker.RunPersonBrief(
		t.Context(), "run-brief", lease, RunIncremental, testCase.mode)
	return outcome
}

func briefCallCoordinates(request ApplyRequest) []ProviderCallCoordinate {
	coordinates := make([]ProviderCallCoordinate, 0, len(request.Batches))
	for _, batch := range request.Batches {
		coordinates = append(coordinates, ProviderCallCoordinate{BatchOrdinal: batch.Ordinal,
			CallOrdinal: batch.CallOrdinal, Purpose: batch.Purpose})
	}
	return coordinates
}

func TestPersonSweepWorkerAppendsBriefAfterExtractionBatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	items := briefWorkerArchiveItems()
	outcome := runBriefWorkerCase(t, briefWorkerCase{
		mode:  BriefModeAuto,
		brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90},
		responses: []DriverResponse{
			briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
			briefDriverResponse(briefWorkerCandidate(items), "request-brief"),
		},
	})
	require.NoError(outcome.err)
	require.Len(outcome.sink.requests, 1)
	applied := outcome.sink.requests[0]

	assert.Equal([]ProviderCallCoordinate{
		{BatchOrdinal: 0, CallOrdinal: 0, Purpose: ProviderCallPurposePrimary},
		{BatchOrdinal: 1, CallOrdinal: 0, Purpose: ProviderCallPurposeBrief},
	}, briefCallCoordinates(applied), "the brief closes the attempt at the next ordinal")
	require.NotNil(applied.Brief)
	assert.Equal(BriefProgramID, applied.Brief.ProgramID)
	assert.Equal(BriefProgramFingerprint(), applied.Brief.ProgramFingerprint)
	assert.Equal(BriefRendererPolicyV1, applied.Brief.Rendered.Policy)
	assert.NotEmpty(applied.Brief.Rendered.Text)
	assert.Equal(int64(90), applied.Brief.Boundary.ThroughSequence,
		"the boundary records the archive high water the plan captured")
	assert.NotEmpty(applied.Brief.Evidence)
	assert.Empty(applied.BriefFailureClass)
	assert.Equal(2, applied.Usage.Requests)
}

func TestPersonSweepWorkerReportsBriefSchedulingEligibilityFailure(t *testing.T) {
	cause := errors.New("archive read failed")
	outcome := runBriefWorkerCase(t, briefWorkerCase{
		brief:      &briefFakeStore{eligibilityErr: cause},
		gapResults: []GapResult{{PeopleScanned: 1, PersonIDs: []int64{7}, NextPersonID: 7}},
		runRequest: &RunRequest{Kind: RunScheduled, Mode: RunIncremental, Limit: 1},
	})
	require.ErrorIs(t, outcome.err, cause)
	assert.Equal(t, []RunStatus{RunFailed}, outcome.store.finished)
	assert.Empty(t, outcome.sink.requests)
}

func TestPersonSweepWorkerRunsBriefOnStatusOnlyExtraction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	items := briefWorkerArchiveItems()
	config, catalog := workerTestConfig(t)
	mutateTestProvider(&config, func(provider *ProviderConfig) { provider.AllowSensitive = true })
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store := &workerFailureStore{cursor: Cursor{ReconciliationComplete: true, LastBackstopAt: &now}}
	// No seeds: the extraction half of the attempt is status-only.
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {Changes: []ArchiveChange{{Sequence: 1, PersonID: 7,
			SourceLane: SourceConversationText}}, NextSequence: 1},
	}}
	driver := &workerRepairDriver{responses: []DriverResponse{
		briefDriverResponse(briefWorkerCandidate(items), "request-brief")}}
	sink := &workerProductionSink{}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source), Sink: sink,
		Runner:  newWorkerRepairRunner(t, config, driver),
		Brief:   &briefFakeStore{enrolled: true, highWater: 12, changedAfter: 12},
		Archive: &briefFakeArchive{items: items},
		Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "attempt-status-only-brief" }, WorkerID: "worker-fixture"}

	_, err := worker.RunPersonBrief(t.Context(), "run-status-only", Lease{PersonID: 7,
		WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)},
		RunIncremental, BriefModeAuto)
	require.NoError(err)
	require.Len(sink.requests, 1)
	applied := sink.requests[0]

	assert.Equal([]ProviderCallCoordinate{
		{BatchOrdinal: 0, CallOrdinal: 0, Purpose: ProviderCallPurposeBrief},
	}, briefCallCoordinates(applied), "the brief takes ordinal 0 when extraction ran no batch")
	require.NotNil(applied.Brief)
	assert.NotEqual(StatusOnlyProvider, applied.Generation.Provider,
		"a completed brief call makes the generation a provider generation")
	assert.Equal("provider-v1", applied.Generation.ProviderVersion)
	assert.Equal("model-v1", applied.Generation.ModelVersion)
	assert.Equal(1, applied.Usage.Requests)
}

func TestPersonSweepWorkerDefersBriefWhenBudgetIsExhausted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	items := briefWorkerArchiveItems()
	outcome := runBriefWorkerCase(t, briefWorkerCase{
		mode:  BriefModeAuto,
		brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90},
		responses: []DriverResponse{
			briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
			briefDriverResponse(briefWorkerCandidate(items), "request-brief"),
		},
		denyReservation: 2,
	})
	require.NoError(outcome.err, "budget exhaustion defers the brief without failing the attempt")
	require.Len(outcome.sink.requests, 1)
	applied := outcome.sink.requests[0]

	assert.Equal([]ProviderCallCoordinate{
		{BatchOrdinal: 0, CallOrdinal: 0, Purpose: ProviderCallPurposePrimary},
	}, briefCallCoordinates(applied), "the extraction still applies")
	assert.Nil(applied.Brief)
	assert.Equal(FailureBudget, applied.BriefFailureClass)
	assert.Equal(1, outcome.driver.calls, "no paid brief call is made once the reservation is denied")
}

func TestPersonSweepWorkerSkipsBriefUnderMinInterval(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	generated := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	for name, testCase := range map[string]struct {
		brief     *briefFakeStore
		mode      BriefMode
		wantBrief bool
	}{
		"recent brief with new activity": {
			mode: BriefModeAuto,
			brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90,
				eligibility: BriefEligibility{Version: 3, Status: "current", GeneratedAt: &generated,
					Boundary: &BriefBoundary{ThroughSequence: 40}}},
		},
		"no new activity": {
			mode: BriefModeAuto,
			brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 40,
				eligibility: BriefEligibility{Version: 3, Status: "current", GeneratedAt: &generated,
					Boundary: &BriefBoundary{ThroughSequence: 40}}},
		},
		"not enrolled": {
			mode:  BriefModeAuto,
			brief: &briefFakeStore{highWater: 90, changedAfter: 90},
		},
		"explicit skip": {
			mode:  BriefModeSkip,
			brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90},
		},
		"cadence inside the pre-call window": {
			mode: BriefModeAuto,
			brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90,
				cadenceDueAt: &briefCadenceDue,
				eligibility: BriefEligibility{Version: 3, Status: "current", GeneratedAt: &generated,
					Boundary: &BriefBoundary{ThroughSequence: 40}}},
			wantBrief: true,
		},
		"forced past the min interval": {
			mode: BriefModeForce,
			brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 40,
				eligibility: BriefEligibility{Version: 3, Status: "current", GeneratedAt: &generated,
					Boundary: &BriefBoundary{ThroughSequence: 40}}},
			wantBrief: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			items := briefWorkerArchiveItems()
			outcome := runBriefWorkerCase(t, briefWorkerCase{
				mode: testCase.mode, brief: testCase.brief,
				responses: []DriverResponse{
					briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
					briefDriverResponse(briefWorkerCandidate(items), "request-brief"),
				},
			})
			require.NoError(outcome.err)
			require.Len(outcome.sink.requests, 1)
			if !testCase.wantBrief {
				assert.Nil(outcome.sink.requests[0].Brief)
				assert.Empty(outcome.sink.requests[0].BriefFailureClass)
				assert.Equal(1, outcome.driver.calls, "an ineligible brief costs nothing")
				return
			}
			assert.NotNil(outcome.sink.requests[0].Brief)
			assert.Equal(2, outcome.driver.calls)
		})
	}
}

func TestPersonSweepWorkerRepairsAnInvalidBriefOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	items := briefWorkerArchiveItems()
	outcome := runBriefWorkerCase(t, briefWorkerCase{
		mode:  BriefModeAuto,
		brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90},
		responses: []DriverResponse{
			briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
			briefDriverResponse(json.RawMessage(`{"highlights":"invalid"}`), "request-brief"),
			briefDriverResponse(briefWorkerCandidate(items), "request-brief-repair"),
		},
	})
	require.NoError(outcome.err)
	require.Len(outcome.sink.requests, 1)
	applied := outcome.sink.requests[0]

	assert.Equal([]ProviderCallCoordinate{
		{BatchOrdinal: 0, CallOrdinal: 0, Purpose: ProviderCallPurposePrimary},
		{BatchOrdinal: 1, CallOrdinal: 0, Purpose: ProviderCallPurposeBrief},
		{BatchOrdinal: 1, CallOrdinal: 1, Purpose: ProviderCallPurposeBriefRepair},
	}, briefCallCoordinates(applied), "a brief repairs once on its own call ordinal")
	assert.NotNil(applied.Brief)
	assert.Equal(3, outcome.driver.calls)
}

func TestPersonSweepWorkerRecordsBriefFailureAndStillAppliesExtraction(t *testing.T) {
	for name, testCase := range map[string]struct {
		responses []DriverResponse
		wantClass FailureClass
		wantCalls int
	}{
		"second invalid candidate": {
			responses: []DriverResponse{
				briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
				briefDriverResponse(json.RawMessage(`{"highlights":"invalid"}`), "request-brief"),
				briefDriverResponse(json.RawMessage(`{"highlights":"still invalid"}`), "request-repair"),
			},
			wantClass: FailureInvalidOutput,
			wantCalls: 3,
		},
		"every item dropped": {
			responses: []DriverResponse{
				briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
				briefDriverResponse(briefOutputJSON{
					Highlights: []string{briefHighlightJSON(
						"unsupported", BriefSpeakerPerson, "", "evidence:missing")},
				}.raw(), "request-brief"),
			},
			wantClass: FailureInvalidOutput,
			wantCalls: 2,
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			outcome := runBriefWorkerCase(t, briefWorkerCase{
				mode:      BriefModeAuto,
				brief:     &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90},
				responses: testCase.responses,
			})
			require.NoError(outcome.err, "a brief failure must not roll back the extraction")
			require.Len(outcome.sink.requests, 1)
			applied := outcome.sink.requests[0]
			assert.Nil(applied.Brief)
			assert.Equal(testCase.wantClass, applied.BriefFailureClass)
			assert.Equal(testCase.wantCalls, outcome.driver.calls)
			assert.Contains(briefCallCoordinates(applied), ProviderCallCoordinate{
				BatchOrdinal: 0, CallOrdinal: 0, Purpose: ProviderCallPurposePrimary},
				"the extraction batch still applies")
		})
	}
}

type briefBlockingDriver struct {
	workerRepairDriver

	onBrief func()
}

func (d *briefBlockingDriver) GeneratePrepared(
	ctx context.Context, profile ProviderProfile, credential Credential, prepared PreparedStructuredRequest,
) (DriverResponse, error) {
	if prepared.Request().ProgramID != BriefProgramID {
		return d.workerRepairDriver.GeneratePrepared(ctx, profile, credential, prepared)
	}
	if d.onBrief != nil {
		d.onBrief()
	}
	<-ctx.Done()
	return DriverResponse{}, ctx.Err()
}

func TestPersonSweepWorkerBriefCallFailure(t *testing.T) {
	for _, name := range []string{"request timeout", "parent cancellation", "parent deadline",
		"heartbeat error", "heartbeat deadline", "heartbeat lease lost"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				config, catalog := workerTestConfig(t)
				mutateTestProvider(&config, func(provider *ProviderConfig) {
					provider.AllowSensitive = true
					provider.RequestTimeout = time.Second
				})
				now := time.Now()
				lease := Lease{PersonID: 7, WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)}
				store := &workerFailureStore{lease: &lease,
					cursor: Cursor{ReconciliationComplete: true, LastBackstopAt: &now}}
				seed := packetTestEvidence(71, SourceConversationText, "changed seed")
				source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
					GenerationCursorOptimistic: {Seeds: []EvidenceItem{seed}, Changes: []ArchiveChange{{
						Sequence: 1, PersonID: 7, SourceLane: SourceConversationText}}, NextSequence: 1},
				}}
				driver := &briefBlockingDriver{responses: []DriverResponse{
					briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
				}}
				ctx := t.Context()
				var wantErr error
				switch name {
				case "parent cancellation":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					defer cancel()
					driver.onBrief = cancel
					wantErr = context.Canceled
				case "parent deadline":
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, time.Second/2)
					defer cancel()
					wantErr = context.DeadlineExceeded
				case "heartbeat error", "heartbeat deadline", "heartbeat lease lost":
					config.LeaseDuration = 300 * time.Millisecond
					wantErr = errors.New("database unavailable")
					switch name {
					case "heartbeat deadline":
						wantErr = context.DeadlineExceeded
					case "heartbeat lease lost":
						wantErr = ErrLeaseLost
					}
					store.renewFailure = wantErr
					driver.onBrief = func() { store.failNextRenewal.Store(true) }
				}
				sink := &workerProductionSink{}
				worker := Worker{Config: config, Store: store, Source: source,
					Context: NewContextRetriever(source), Sink: sink,
					Runner:  newWorkerRepairRunner(t, config, driver),
					Brief:   &briefFakeStore{enrolled: true, highWater: 90},
					Archive: &briefFakeArchive{items: briefWorkerArchiveItems()},
					Catalog: workerFailureCatalog{catalog: catalog}, Clock: time.Now,
					NewID: func() string { return "attempt-brief" }, WorkerID: "worker-fixture"}
				_, err := worker.RunPersonBrief(ctx, "run-brief", lease, RunIncremental, BriefModeAuto)
				if wantErr != nil {
					require.ErrorIs(err, wantErr)
					assert.Empty(sink.requests, "parent or lease failure must prevent extraction apply")
					return
				}
				require.NoError(err, "the brief's request timeout must preserve completed extraction")
				require.Len(sink.requests, 1)
				applied := sink.requests[0]
				assert.Nil(applied.Brief)
				assert.Equal(FailureTimeout, applied.BriefFailureClass)
				assert.Equal([]ProviderCallCoordinate{{BatchOrdinal: 0, CallOrdinal: 0,
					Purpose: ProviderCallPurposePrimary}}, briefCallCoordinates(applied))
			})
		})
	}
}

func TestPersonSweepWorkerForcedBriefPublishesWorkForOnePerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	items := briefWorkerArchiveItems()
	brief := &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 40, trackedWork: true}
	outcome := runBriefWorkerCase(t, briefWorkerCase{
		brief: brief,
		responses: []DriverResponse{
			briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
			briefDriverResponse(briefWorkerCandidate(items), "request-brief"),
		},
		runRequest: &RunRequest{Kind: RunManual, Mode: RunIncremental, PersonID: 7,
			Limit: 1, Brief: BriefModeForce},
	})
	require.NoError(outcome.err)
	assert.Equal([]int64{7}, brief.ensuredWork,
		"a forced brief publishes work so the person can be claimed")
	require.Len(outcome.sink.requests, 1)
	assert.NotNil(outcome.sink.requests[0].Brief)
}

// TestPersonSweepWorkerRefusesAForcedBriefWhileAnotherWorkerHoldsTheLease
// pins the contention path. A forced brief publishes its own work, so the only
// way the claim comes back empty is that another worker holds the person's
// lease. The run used to finish successfully with nothing attempted, which the
// API reported as 200 and "no new version"; it must refuse with a retryable
// typed error, and it must not wait on the other worker's lease.
func TestPersonSweepWorkerRefusesAForcedBriefWhileAnotherWorkerHoldsTheLease(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	brief := &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 40, trackedWork: true}
	outcome := runBriefWorkerCase(t, briefWorkerCase{
		brief:     brief,
		responses: []DriverResponse{},
		leaseHeld: true,
		runRequest: &RunRequest{Kind: RunManual, Mode: RunIncremental, PersonID: 7,
			Limit: 1, Brief: BriefModeForce},
	})
	require.ErrorIs(outcome.err, ErrPersonBriefBusy)
	assert.Contains(outcome.err.Error(), "person 7")
	assert.Equal([]int64{7}, brief.ensuredWork, "the work was published before the claim")
	assert.Equal(1, outcome.store.claimCalls, "one claim, no spin-wait on the lease")
	assert.Empty(outcome.sink.requests, "nothing ran, so nothing was applied")
	assert.Zero(outcome.driver.calls)
	assert.Equal([]RunStatus{RunFailed}, outcome.store.finished,
		"the run row records that the request produced nothing")
}

// TestPersonSweepWorkerScheduledRunWithNoClaimableWorkIsNotBusy keeps the
// refusal scoped to a forced single-person brief: a scheduled or unforced run
// that finds no claimable work is an ordinary empty run.
func TestPersonSweepWorkerScheduledRunWithNoClaimableWorkIsNotBusy(t *testing.T) {
	brief := &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 40, trackedWork: true}
	outcome := runBriefWorkerCase(t, briefWorkerCase{
		brief:     brief,
		responses: []DriverResponse{},
		leaseHeld: true,
		runRequest: &RunRequest{Kind: RunScheduled, Mode: RunIncremental,
			Limit: 1, Brief: BriefModeAuto},
	})
	require.NoError(t, outcome.err)
	assert.Equal(t, []RunStatus{RunSucceeded}, outcome.store.finished)
}

func TestPersonSweepWorkerRejectsUnknownBriefMode(t *testing.T) {
	config, catalog := workerTestConfig(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store := &workerFailureStore{}
	source := &workerProductionSource{}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source), Sink: &workerProductionSink{},
		Runner: &workerProductionRunner{}, Catalog: workerFailureCatalog{catalog: catalog},
		Clock: func() time.Time { return now }, NewID: func() string { return "run-bad-brief" },
		WorkerID: "worker-fixture"}
	_, err := worker.Run(t.Context(), RunRequest{Kind: RunManual, Mode: RunIncremental,
		Limit: 1, Brief: BriefMode("regenerate")})
	require.ErrorContains(t, err, "valid brief mode")
}

// A brief packet always carries verbatim archive text, so a profile without
// allow_sensitive can never send one. The refusal happens in the plan, before
// any window is built or any budget is reserved.
func TestPersonSweepBriefPlanRefusesProfilesWithoutSensitiveInput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	config, _ := workerTestConfig(t)
	archive := &briefFakeArchive{items: briefWorkerArchiveItems()}
	worker := Worker{Config: config,
		Brief:   &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90},
		Archive: archive}
	profile := ProviderProfile{Fingerprint: "profile", AllowSensitive: false}

	plan, err := worker.planPersonBrief(t.Context(), 7, BriefModeForce, profile,
		time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(err)
	assert.False(plan.run)
	assert.Empty(archive.candidateCalls, "no brief window is even built")

	profile.AllowSensitive = true
	plan, err = worker.planPersonBrief(t.Context(), 7, BriefModeForce, profile,
		time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(err)
	assert.True(plan.run)
	assert.Equal(int64(90), plan.throughSequence)
}

func TestPersonSweepWorkerSkipsBriefWhenTheLaneIsDisabled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	items := briefWorkerArchiveItems()
	outcome := runBriefWorkerCase(t, briefWorkerCase{
		mode:  BriefModeForce,
		brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90},
		responses: []DriverResponse{
			briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
			briefDriverResponse(briefWorkerCandidate(items), "request-brief"),
		},
		briefConfig: func(brief *BriefConfig) { brief.Enabled = &briefLaneOff },
	})
	require.NoError(outcome.err)
	require.Len(outcome.sink.requests, 1)
	assert.Nil(outcome.sink.requests[0].Brief)
	assert.Equal(1, outcome.driver.calls)
}

func TestPersonSweepWorkerSkipsBriefWithNoEligibleEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	outcome := runBriefWorkerCase(t, briefWorkerCase{
		mode:    BriefModeForce,
		brief:   &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90},
		archive: &briefFakeArchive{},
		responses: []DriverResponse{
			briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
		},
	})
	require.NoError(outcome.err)
	require.Len(outcome.sink.requests, 1)
	assert.Nil(outcome.sink.requests[0].Brief)
	assert.Empty(outcome.sink.requests[0].BriefFailureClass,
		"having nothing to summarize is not a failure")
	assert.Equal(1, outcome.driver.calls)
}

func TestPersonSweepWorkerKeepsBriefAttributesAsSuggestions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	items := briefWorkerArchiveItems()
	_, catalog := workerTestConfig(t)
	target := catalog.Targets[0]
	newest := packetEvidenceID(items[len(items)-1])
	candidate := briefOutputJSON{
		Highlights: []string{briefHighlightJSON(
			"they start a new role", BriefSpeakerPerson, "", newest)},
		Attributes: []string{fmt.Sprintf(
			`{"target_key":%q,"relation":"support","value":"Riverton","evidence_ids":[%q],`+
				`"valid_from":null,"valid_until":null,"confidence_basis_points":900}`,
			target.Key, newest)},
	}.raw()
	outcome := runBriefWorkerCase(t, briefWorkerCase{
		mode:  BriefModeAuto,
		brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90},
		responses: []DriverResponse{
			briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
			briefDriverResponse(candidate, "request-brief"),
		},
	})
	require.NoError(outcome.err)
	require.Len(outcome.sink.requests, 1)
	claims := outcome.sink.requests[0].Generation.Claims
	assert.Empty(claims, "brief suggestions must not enter the extraction generation")
	var output BriefOutput
	require.NoError(json.Unmarshal(outcome.sink.requests[0].Brief.Structured, &output))
	assert.Len(output.PossibleAttributes, 1)
}

// A person the scheduled sweep already finished has no cursor progress, and a
// generation must name a source range. A planned brief re-reads one bounded
// backstop page so the attempt has a real range to bind to, which is what makes
// "generate this person's brief now" work at all.
func TestPersonSweepWorkerForcesABackstopWhenABriefHasNoCursorProgress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	items := briefWorkerArchiveItems()
	config, catalog := workerTestConfig(t)
	mutateTestProvider(&config, func(provider *ProviderConfig) { provider.AllowSensitive = true })
	config.Budgets.MaxRequestsPerPerson = 3
	config.Budgets.MaxOutputTokensPerPerson = 3 * extractionMaxOutputTokens
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	// Reconciliation is complete, the backstop is not due, and no lane has new
	// changes, so the ordinary assembly makes no progress at all.
	store := &workerFailureStore{cursor: Cursor{ReconciliationComplete: true, LastBackstopAt: &now}}
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {},
		GenerationCursorBackstop: {
			Seeds:            []EvidenceItem{packetTestEvidence(90, SourceConversationText, "history")},
			CapturedUpperKey: "0090", NextReconcileKey: "0090", ReconciliationDone: true,
		},
	}}
	driver := &workerRepairDriver{responses: []DriverResponse{
		briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
		briefDriverResponse(briefWorkerCandidate(items), "request-brief"),
	}}
	sink := &workerProductionSink{}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source), Sink: sink,
		Runner:  newWorkerRepairRunner(t, config, driver),
		Brief:   &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 0},
		Archive: &briefFakeArchive{items: items},
		Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "attempt-forced-backstop" }, WorkerID: "worker-fixture"}

	_, err := worker.RunPersonBrief(t.Context(), "run-forced-backstop", Lease{PersonID: 7,
		WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)},
		RunIncremental, BriefModeForce)
	require.NoError(err)
	require.Len(sink.requests, 1)
	applied := sink.requests[0]
	require.Len(applied.CursorEnvelope, 1)
	assert.Equal(GenerationCursorBackstop, applied.CursorEnvelope[0].Mode,
		"the forced page is a backstop, not an invented cursor range")
	assert.NotNil(applied.Brief)
}

// A caught-up person with no planned brief has nothing left to retry.
func TestPersonSweepWorkerCompletesIdleWorkWithoutABrief(t *testing.T) {
	assert := assert.New(t)
	config, catalog := workerTestConfig(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store := &workerFailureStore{cursor: Cursor{ReconciliationComplete: true, LastBackstopAt: &now}}
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {},
	}}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source), Sink: &workerProductionSink{},
		Runner: &workerProductionRunner{}, Catalog: workerFailureCatalog{catalog: catalog},
		Clock: func() time.Time { return now },
		NewID: func() string { return "attempt-no-progress" }, WorkerID: "worker-fixture"}

	_, err := worker.RunPerson(t.Context(), "run-no-progress", Lease{PersonID: 7,
		WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)}, RunIncremental)
	require.NoError(t, err)
	assert.Len(store.idleCompleted, 1)
	assert.Empty(store.started, "idle work creates no inference attempt")
	assert.Empty(store.failed)
}

// Ruling R10: rejecting a version is the owner asking for a different brief, so
// the next run regenerates without waiting out min_interval and without needing
// new activity. The rejected version's boundary still seeds the overlap.
func TestPersonSweepWorkerRegeneratesAfterARejectedVersion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	generated := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	rejectedBoundary := &BriefBoundary{ThroughSequence: 40,
		ThroughEventTime: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	items := briefWorkerArchiveItems()
	outcome := runBriefWorkerCase(t, briefWorkerCase{
		mode: BriefModeAuto,
		// No new activity and a brief generated one day ago: a current version
		// would be skipped on both counts.
		brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 40,
			eligibility: BriefEligibility{Version: 3, Status: BriefStatusRejected,
				GeneratedAt: &generated, Boundary: rejectedBoundary}},
		responses: []DriverResponse{
			briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
			briefDriverResponse(briefWorkerCandidate(items), "request-brief"),
		},
	})
	require.NoError(outcome.err)
	require.Len(outcome.sink.requests, 1)
	assert.NotNil(outcome.sink.requests[0].Brief,
		"a rejected version regenerates without waiting out min_interval")

	// The same state with the version still current is skipped.
	skipped := runBriefWorkerCase(t, briefWorkerCase{
		mode: BriefModeAuto,
		brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 40,
			eligibility: BriefEligibility{Version: 3, Status: BriefStatusCurrent,
				GeneratedAt: &generated, Boundary: rejectedBoundary}},
		responses: []DriverResponse{
			briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
		},
	})
	require.NoError(skipped.err)
	require.Len(skipped.sink.requests, 1)
	assert.Nil(skipped.sink.requests[0].Brief)
}

func TestPersonSweepWorkerRejectedBoundaryStillSeedsTheOverlap(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	rejectedBoundary := &BriefBoundary{ThroughSequence: 40,
		ThroughEventTime: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	worker := Worker{Config: mustDefaultedBriefConfig(t),
		Brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 40,
			eligibility: BriefEligibility{Version: 3, Status: BriefStatusRejected,
				Boundary: rejectedBoundary}},
		Archive: &briefFakeArchive{items: briefWorkerArchiveItems()}}
	profile := ProviderProfile{Fingerprint: "profile", AllowSensitive: true}

	plan, err := worker.planPersonBrief(t.Context(), 7, BriefModeAuto, profile,
		time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(err)
	assert.True(plan.run)
	require.NotNil(plan.previous)
	assert.Equal(rejectedBoundary.ThroughEventTime, plan.previous.ThroughEventTime,
		"the rejected window is still already-covered ground for the overlap rule")
}

// TestPersonSweepWorkerPlansNoBriefWhenTheProfileHasNoBriefLane pins the
// scheduled-mode half of the same gate: a document_text-only profile plans no
// brief at all, so the attempt never builds a window that would fail on "no
// allowed source lane" and record an internal brief failure. The decision is
// made from the profile alone, before any durable state is read.
func TestPersonSweepWorkerPlansNoBriefWhenTheProfileHasNoBriefLane(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	brief := &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90, trackedWork: true}
	worker := Worker{Config: mustDefaultedBriefConfig(t), Brief: brief,
		Archive: &briefFakeArchive{items: briefWorkerArchiveItems()}}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	for _, mode := range []BriefMode{BriefModeAuto, BriefModeForce} {
		plan, err := worker.planPersonBrief(t.Context(), 7, mode, ProviderProfile{
			Fingerprint: "profile", AllowSensitive: true,
			AllowedSources: []SourceClass{SourceDocumentText},
		}, now)
		require.NoError(err, mode)
		assert.False(plan.run, "%s mode plans no brief without a brief lane", mode)
		assert.Zero(plan.throughSequence, "%s mode reads no durable state first", mode)
	}

	plan, err := worker.planPersonBrief(t.Context(), 7, BriefModeAuto, ProviderProfile{
		Fingerprint: "profile", AllowSensitive: true,
		AllowedSources: []SourceClass{SourceDocumentText, SourceMeetingText},
	}, now)
	require.NoError(err)
	assert.False(plan.run, "meeting_text is not a brief lane in this version (R13)")

	plan, err = worker.planPersonBrief(t.Context(), 7, BriefModeAuto, ProviderProfile{
		Fingerprint: "profile", AllowSensitive: true,
		AllowedSources: []SourceClass{SourceDocumentText, SourceConversationText},
	}, now)
	require.NoError(err)
	assert.True(plan.run, "the conversation lane alone is enough to plan the brief")
}

func mustDefaultedBriefConfig(t *testing.T) Config {
	t.Helper()
	config, _ := workerTestConfig(t)
	return config
}

func TestPersonSweepWorkerRefusesAForcedBriefForAnUnenrolledPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	brief := &briefFakeStore{highWater: 90, changedAfter: 90, trackedWork: true}
	outcome := runBriefWorkerCase(t, briefWorkerCase{
		brief:     brief,
		responses: []DriverResponse{},
		runRequest: &RunRequest{Kind: RunManual, Mode: RunIncremental, PersonID: 7,
			Limit: 1, Brief: BriefModeForce},
	})
	require.ErrorIs(outcome.err, ErrPersonBriefNotEnrolled)
	assert.Empty(brief.ensuredWork,
		"an unenrolled person must not have work published on their behalf")
	assert.Empty(outcome.sink.requests, "and must not pay for an extraction attempt")
	assert.Zero(outcome.driver.calls)
}

// TestPersonSweepWorkerRefusesAForcedBriefTheLaneOrPolicyRulesOut covers the
// gates that used to answer a manual "generate now" with a successful run and
// no brief: the lane turned off globally, a consented profile that refuses the
// sensitive packet every brief carries, and a profile whose allowed_sources
// hold no lane the window reads. All are caller errors, so none may publish
// work or pay for an attempt.
func TestPersonSweepWorkerRefusesAForcedBriefTheLaneOrPolicyRulesOut(t *testing.T) {
	for _, test := range []struct {
		name     string
		testCase briefWorkerCase
		want     error
	}{
		{
			name:     "the lane is disabled",
			testCase: briefWorkerCase{briefConfig: func(brief *BriefConfig) { brief.Enabled = &briefLaneOff }},
			want:     ErrPersonBriefLaneDisabled,
		},
		{
			name: "the profile refuses sensitive content",
			testCase: briefWorkerCase{
				provider: func(provider *ProviderConfig) { provider.AllowSensitive = false },
			},
			want: ErrPersonBriefPolicyRefused,
		},
		{
			name: "the profile allows only the document lane",
			testCase: briefWorkerCase{
				provider: func(provider *ProviderConfig) {
					provider.AllowedSources = []SourceClass{SourceDocumentText}
				},
			},
			want: ErrPersonBriefNoSupportedLane,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			brief := &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90,
				trackedWork: true}
			testCase := test.testCase
			testCase.brief = brief
			testCase.responses = []DriverResponse{}
			testCase.runRequest = &RunRequest{Kind: RunManual, Mode: RunIncremental,
				PersonID: 7, Limit: 1, Brief: BriefModeForce}
			outcome := runBriefWorkerCase(t, testCase)

			require.ErrorIs(outcome.err, test.want)
			assert.Contains(outcome.err.Error(), "person 7")
			assert.Empty(brief.ensuredWork, "no work is published for a refused request")
			assert.Empty(outcome.sink.requests, "and no extraction attempt is paid for")
			assert.Zero(outcome.driver.calls)
		})
	}
}

func TestPersonSweepWorkerRunResultCarriesEachPersonsAttemptAndBrief(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	items := briefWorkerArchiveItems()
	config, catalog := workerTestConfig(t)
	mutateTestProvider(&config, func(provider *ProviderConfig) { provider.AllowSensitive = true })
	config.Budgets.MaxRequestsPerPerson = 3
	config.Budgets.MaxOutputTokensPerPerson = 3 * extractionMaxOutputTokens
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	lease := Lease{PersonID: 7, WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)}
	store := &workerFailureStore{lease: &lease,
		cursor: Cursor{ReconciliationComplete: true, LastBackstopAt: &now}}
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {
			Seeds:        []EvidenceItem{packetTestEvidence(71, SourceConversationText, "changed seed")},
			Changes:      []ArchiveChange{{Sequence: 1, PersonID: 7, SourceLane: SourceConversationText}},
			NextSequence: 1},
	}}
	driver := &workerRepairDriver{responses: []DriverResponse{
		briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
		briefDriverResponse(briefWorkerCandidate(items), "request-brief"),
	}}
	sink := &workerProductionSink{briefVersion: 4}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source), Sink: sink,
		Runner:  newWorkerRepairRunner(t, config, driver),
		Brief:   &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90, trackedWork: true},
		Archive: &briefFakeArchive{items: items},
		Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "attempt-run-result" }, WorkerID: "worker-fixture"}

	result, err := worker.Run(t.Context(), RunRequest{Kind: RunManual, Mode: RunIncremental,
		PersonID: 7, Limit: 1, Brief: BriefModeForce})
	require.NoError(err)
	require.Len(result.People, 1,
		"a manual single-person run reports that person without a second query")
	assert.Equal("attempt-run-result", result.People[0].AttemptID)
	assert.Equal(int64(7), result.People[0].PersonID)
	assert.Equal(4, result.People[0].BriefVersion)
	assert.Empty(result.People[0].BriefFailureClass)
}

// briefLongText pads a readable opening out to an exact rune length so the cap
// test can build a paragraph well past 240 runes without a wall of prose.
func briefLongText(seed string, runes int) string {
	for utf8.RuneCountInString(seed) < runes {
		seed += " and they went into more detail about it than usual"
	}
	return string([]rune(seed)[:runes])
}

// briefLongWorkerCandidate is a brief whose paragraph is far past the smallest
// cap an operator may configure, so trimming is observable end to end.
func briefLongWorkerCandidate(items []EvidenceItem) json.RawMessage {
	newest := packetEvidenceID(items[len(items)-1])
	return briefOutputJSON{
		LastInteraction: fmt.Sprintf(`{"evidence_id":%q,"summary":%q}`, newest,
			briefLongText("they start a new role in September", 180)),
		Highlights: []string{
			briefHighlightJSON(briefLongText("they finished the move", 200),
				BriefSpeakerPerson, "", newest),
			briefHighlightJSON(briefLongText("they start a new role in September", 200),
				BriefSpeakerPerson, "", newest),
		},
		FollowUps: []string{fmt.Sprintf(
			`{"question":"how the first week went","why":"they were about to start",`+
				`"highlight_index":0,"evidence_ids":[%q]}`, newest)},
	}.raw()
}

// TestPersonSweepWorkerCapsTheRenderedBrief proves the configured rune cap
// reaches the renderer through the window request, and that the stored
// structure is the trimmed one rather than the paragraph alone.
func TestPersonSweepWorkerCapsTheRenderedBrief(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	items := briefWorkerArchiveItems()

	newCase := func(runeCap int) briefWorkerCase {
		testCase := briefWorkerCase{
			mode:  BriefModeAuto,
			brief: &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 90},
			responses: []DriverResponse{
				briefDriverResponse(json.RawMessage(`{"claims":[]}`), "request-extraction"),
				briefDriverResponse(briefLongWorkerCandidate(items), "request-brief"),
			},
		}
		if runeCap > 0 {
			testCase.briefConfig = func(brief *BriefConfig) { brief.MaxRenderedRunes = runeCap }
		}
		return testCase
	}

	uncapped := runBriefWorkerCase(t, newCase(4096))
	require.NoError(uncapped.err)
	require.Len(uncapped.sink.requests, 1)
	full := uncapped.sink.requests[0].Brief
	require.NotNil(full)
	require.Len(full.Rendered.Sentences, 4, "interaction, two highlights, one follow-up")
	assert.Zero(full.DroppedItemCount)
	assert.Greater(utf8.RuneCountInString(full.Rendered.Text), briefMinRenderedRunes)

	capped := runBriefWorkerCase(t, newCase(briefMinRenderedRunes))
	require.NoError(capped.err)
	require.Len(capped.sink.requests, 1)
	trimmed := capped.sink.requests[0].Brief
	require.NotNil(trimmed)

	head := full.Rendered.Sentences[0].Text
	assert.Equal(head, trimmed.Rendered.Text, "the last-interaction sentence survives the cap")
	assert.LessOrEqual(utf8.RuneCountInString(trimmed.Rendered.Text), briefMinRenderedRunes)
	require.Len(trimmed.Rendered.Sentences, 1)
	assert.Equal(3, trimmed.DroppedItemCount, "the follow-up and both highlights are counted")

	var stored BriefOutput
	require.NoError(json.Unmarshal(trimmed.Structured, &stored))
	assert.Empty(stored.Highlights, "the stored structure is the trimmed one")
	assert.Empty(stored.FollowUps)
	assert.NotNil(stored.LastMeaningfulInteraction)
	assert.Len(trimmed.Evidence, 1, "the pointers cover only the kept item")
}
