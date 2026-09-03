package operations

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestOperationEnumsRejectUnknownValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		valid    []string
		validate func(string) error
	}{
		{
			name: "kind",
			valid: []string{
				"source_sync", "person_sweep", "carddav_sync", "message_embedding",
				"person_embedding", "document_extraction", "document_embedding",
				"visual_embedding", "person_enrichment",
			},
			validate: func(value string) error { return Kind(value).Validate() },
		},
		{
			name:     "lane",
			valid:    []string{"messages", "person_facts", "contacts", "documents", "visual_attachments"},
			validate: func(value string) error { return Lane(value).Validate() },
		},
		{
			name:     "state",
			valid:    []string{"queued", "running", "succeeded", "partial", "failed", "cancelled"},
			validate: func(value string) error { return State(value).Validate() },
		},
		{
			name:     "trigger",
			valid:    []string{"manual", "scheduled"},
			validate: func(value string) error { return Trigger(value).Validate() },
		},
		{
			name: "counter name",
			valid: []string{
				"processed", "added", "updated", "item_errors", "attempted", "succeeded",
				"failed", "projected_writes", "books", "created", "removed",
			},
			validate: func(value string) error { return CounterName(value).Validate() },
		},
		{
			name:     "counter unit",
			valid:    []string{"messages", "people", "writes", "books", "contacts"},
			validate: func(value string) error { return CounterUnit(value).Validate() },
		},
		{
			name:     "history availability",
			valid:    []string{"available", "unavailable"},
			validate: func(value string) error { return HistoryAvailability(value).Validate() },
		},
		{
			name:     "action",
			valid:    []string{"carddav_sync", "visual_build", "visual_resume"},
			validate: func(value string) error { return ActionID(value).Validate() },
		},
		{
			name: "related status",
			valid: []string{
				"listSourceStatus", "getDocumentIndexStatus", "getDocumentVectorStatus",
				"getVisualAttachmentStatus", "getCardDAVStatus",
			},
			validate: func(value string) error { return RelatedStatusID(value).Validate() },
		},
		{
			name:     "stable ID type",
			valid:    []string{"int64", "text"},
			validate: func(value string) error { return StableIDType(value).Validate() },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, value := range test.valid {
				require.NoError(t, test.validate(value), value)
			}
			require.Error(t, test.validate(""))
			require.Error(t, test.validate("unknown"))
		})
	}
}

func TestOperationLaneRegistryIsClosedAndSorted(t *testing.T) {
	t.Parallel()

	want := []LaneDefinition{
		{Kind: KindCardDAVSync, Lane: LaneContacts, HistoryAvailability: HistoryAvailable},
		{Kind: KindDocumentEmbedding, Lane: LaneDocuments, HistoryAvailability: HistoryUnavailable, UnavailableCode: "document_embedding_history_unavailable"},
		{Kind: KindDocumentExtraction, Lane: LaneDocuments, HistoryAvailability: HistoryUnavailable, UnavailableCode: "document_extraction_history_unavailable"},
		{Kind: KindMessageEmbedding, Lane: LaneMessages, HistoryAvailability: HistoryUnavailable, UnavailableCode: "message_embedding_history_unavailable"},
		{Kind: KindPersonEmbedding, Lane: LanePersonFacts, HistoryAvailability: HistoryUnavailable, UnavailableCode: "person_embedding_history_unavailable"},
		{Kind: KindPersonEnrichment, Lane: LanePersonFacts, HistoryAvailability: HistoryUnavailable, UnavailableCode: "person_enrichment_history_unavailable"},
		{Kind: KindPersonSweep, Lane: LanePersonFacts, HistoryAvailability: HistoryAvailable},
		{Kind: KindSourceSync, Lane: LaneMessages, HistoryAvailability: HistoryAvailable},
		{Kind: KindVisualEmbedding, Lane: LaneVisualAttachments, HistoryAvailability: HistoryUnavailable, UnavailableCode: "visual_embedding_history_unavailable"},
	}

	got := LaneRegistry()
	assert.Equal(t, want, got)
	require.NotEmpty(t, got)
	got[0].Lane = LaneMessages
	assert.Equal(t, LaneContacts, LaneRegistry()[0].Lane, "callers must not mutate the registry")
}

func TestStableIDEnforcesKindPairingAndBounds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Parallel()

	source, err := NewInt64ID(KindSourceSync, 10)
	require.NoError(err)
	assert.Equal(KindSourceSync, source.Kind())
	assert.Equal(StableIDInt64, source.Type())
	value, ok := source.Int64()
	assert.True(ok)
	assert.Equal(int64(10), value)
	_, ok = source.Text()
	assert.False(ok)
	require.NoError(source.Validate())

	people, err := NewTextID(KindPersonSweep, "run-0002")
	require.NoError(err)
	assert.Equal(StableIDText, people.Type())
	text, ok := people.Text()
	assert.True(ok)
	assert.Equal("run-0002", text)
	_, ok = people.Int64()
	assert.False(ok)

	_, err = NewInt64ID(KindSourceSync, 0)
	require.Error(err)
	_, err = NewInt64ID(KindSourceSync, -1)
	require.Error(err)
	_, err = NewTextID(KindPersonSweep, "")
	require.Error(err)
	_, err = NewTextID(KindPersonSweep, " run-0002")
	require.Error(err)
	_, err = NewTextID(KindPersonSweep, strings.Repeat("x", MaxTextStableIDBytes+1))
	require.Error(err)
	_, err = NewTextID(KindPersonSweep, string([]byte{0xff}))
	require.Error(err)

	_, err = NewTextID(KindSourceSync, "10")
	require.Error(err)
	_, err = NewInt64ID(KindPersonSweep, 10)
	require.Error(err)
	_, err = NewInt64ID(KindMessageEmbedding, 10)
	require.Error(err, "unavailable lanes must not gain a run representation")
	require.Error((StableID{}).Validate())
}

func TestRunOrderingUsesTimeThenKindThenTypedID(t *testing.T) {
	t.Parallel()

	instant := time.Date(2026, 8, 28, 12, 34, 56, 123456000, time.UTC)
	source10 := mustInt64ID(t, KindSourceSync, 10)
	source9 := mustInt64ID(t, KindSourceSync, 9)
	cardDAV10 := mustInt64ID(t, KindCardDAVSync, 10)
	peopleB := mustTextID(t, KindPersonSweep, "run-b")
	peopleA := mustTextID(t, KindPersonSweep, "run-a")

	runs := []Run{
		{ID: source9, Lane: LaneMessages, StartedAt: instant},
		{ID: peopleA, Lane: LanePersonFacts, StartedAt: instant},
		{ID: source10, Lane: LaneMessages, StartedAt: instant},
		{ID: peopleB, Lane: LanePersonFacts, StartedAt: instant},
		{ID: cardDAV10, Lane: LaneContacts, StartedAt: instant},
		{ID: source10, Lane: LaneMessages, StartedAt: instant.Add(time.Second)},
	}

	SortRuns(runs)
	assert.Equal(t, []StableID{
		source10,
		cardDAV10,
		peopleB,
		peopleA,
		source10,
		source9,
	}, runIDs(runs))

	sameInstantDifferentZones := []Run{
		{ID: source9, Lane: LaneMessages, StartedAt: instant.In(time.FixedZone("synthetic", -7*60*60))},
		{ID: source10, Lane: LaneMessages, StartedAt: instant},
	}
	SortRuns(sameInstantDifferentZones)
	assert.Equal(t, []StableID{source10, source9}, runIDs(sameInstantDifferentZones))
}

func TestRunValidationEnforcesCounterAllowLists(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		kind     Kind
		counters []PublicCounter
		wantErr  bool
	}{
		{
			name: "source counters",
			kind: KindSourceSync,
			counters: []PublicCounter{
				{Name: CounterProcessed, Unit: CounterUnitMessages, Value: 12},
				{Name: CounterAdded, Unit: CounterUnitMessages, Value: 3},
				{Name: CounterUpdated, Unit: CounterUnitMessages, Value: 2},
				{Name: CounterItemErrors, Unit: CounterUnitMessages, Value: 1},
			},
		},
		{
			name: "people counters",
			kind: KindPersonSweep,
			counters: []PublicCounter{
				{Name: CounterAttempted, Unit: CounterUnitPeople, Value: 5},
				{Name: CounterSucceeded, Unit: CounterUnitPeople, Value: 3},
				{Name: CounterFailed, Unit: CounterUnitPeople, Value: 2},
				{Name: CounterProjectedWrites, Unit: CounterUnitWrites, Value: 7},
			},
		},
		{
			name: "CardDAV counters",
			kind: KindCardDAVSync,
			counters: []PublicCounter{
				{Name: CounterBooks, Unit: CounterUnitBooks, Value: 2},
				{Name: CounterCreated, Unit: CounterUnitContacts, Value: 3},
				{Name: CounterUpdated, Unit: CounterUnitContacts, Value: 4},
				{Name: CounterRemoved, Unit: CounterUnitContacts, Value: 1},
			},
		},
		{
			name:     "negative",
			kind:     KindSourceSync,
			counters: []PublicCounter{{Name: CounterProcessed, Unit: CounterUnitMessages, Value: -1}},
			wantErr:  true,
		},
		{
			name: "duplicate",
			kind: KindSourceSync,
			counters: []PublicCounter{
				{Name: CounterProcessed, Unit: CounterUnitMessages, Value: 1},
				{Name: CounterProcessed, Unit: CounterUnitMessages, Value: 2},
			},
			wantErr: true,
		},
		{
			name:     "wrong unit",
			kind:     KindSourceSync,
			counters: []PublicCounter{{Name: CounterProcessed, Unit: CounterUnitPeople, Value: 1}},
			wantErr:  true,
		},
		{
			name:     "wrong kind",
			kind:     KindSourceSync,
			counters: []PublicCounter{{Name: CounterAttempted, Unit: CounterUnitPeople, Value: 1}},
			wantErr:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateCounters(test.kind, test.counters)
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestOperationSourceStateProjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		durable    string
		itemErrors int64
		added      int64
		updated    int64
		wantState  State
		wantError  *PublicError
		wantErr    bool
	}{
		{name: "running", durable: "running", wantState: StateRunning},
		{name: "completed clean", durable: "completed", wantState: StateSucceeded},
		{name: "completed with item errors", durable: "completed", itemErrors: 1, wantState: StatePartial},
		{name: "failed after add", durable: "failed", added: 1, wantState: StatePartial, wantError: sourceSyncFailedError()},
		{name: "failed after update", durable: "failed", updated: 1, wantState: StatePartial, wantError: sourceSyncFailedError()},
		{name: "failed without mutation", durable: "failed", wantState: StateFailed, wantError: sourceSyncFailedError()},
		{name: "legacy cancelled", durable: "cancelled", wantState: StateCancelled},
		{name: "unknown", durable: "paused", wantErr: true},
		{name: "negative errors", durable: "completed", itemErrors: -1, wantErr: true},
		{name: "negative added", durable: "failed", added: -1, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			t.Parallel()
			state, publicError, err := ProjectSourceState(test.durable, test.itemErrors, test.added, test.updated)
			if test.wantErr {
				require.Error(err)
				return
			}
			require.NoError(err)
			assert.Equal(test.wantState, state)
			assert.Equal(test.wantError, publicError)
		})
	}
}

func TestOperationPublicFailureProjectionIsFixedAndBounded(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Parallel()

	people := []struct {
		class peoplesweep.FailureClass
		code  PublicErrorCode
	}{
		{peoplesweep.FailurePolicy, PublicErrorPolicy},
		{peoplesweep.FailureBudget, PublicErrorBudget},
		{peoplesweep.FailureLeaseLost, PublicErrorLeaseLost},
		{peoplesweep.FailureRateLimited, PublicErrorRateLimited},
		{peoplesweep.FailureTimeout, PublicErrorTimeout},
		{peoplesweep.FailureProviderHTTP, PublicErrorProviderHTTP},
		{peoplesweep.FailureInvalidOutput, PublicErrorInvalidOutput},
		{peoplesweep.FailureArchiveGap, PublicErrorArchiveGap},
		{peoplesweep.FailureInternal, PublicErrorInternal},
		{"", PublicErrorPersonSweepFailed},
		{"synthetic_unknown", PublicErrorPersonSweepFailed},
	}
	for _, test := range people {
		projected := ProjectPersonSweepFailure(test.class)
		assert.Equal(test.code, projected.Code)
		require.NoError(projected.Validate())
		assert.NotEmpty(projected.Message)
		assert.LessOrEqual(len(projected.Message), MaxPublicErrorMessageBytes)
	}

	cardDAV := []struct {
		durable string
		code    PublicErrorCode
	}{
		{"cancelled", PublicErrorCancelled},
		{"retry_after", PublicErrorRetryAfter},
		{"authentication_failed", PublicErrorAuthenticationFailed},
		{"upstream_failed", PublicErrorUpstreamFailed},
		{"safety_limit", PublicErrorSafetyLimit},
		{"sync_failed", PublicErrorSyncFailed},
		{"unsafe_error_redacted", PublicErrorUnsafeErrorRedacted},
		{"daemon_restarted", PublicErrorDaemonRestarted},
		{"synthetic_unknown", PublicErrorCardDAVSyncFailed},
	}
	for _, test := range cardDAV {
		projected := ProjectCardDAVFailure(test.durable)
		require.NotNil(t, projected)
		assert.Equal(test.code, projected.Code)
		require.NoError(projected.Validate())
		assert.LessOrEqual(len(projected.Message), MaxPublicErrorMessageBytes)
	}
	assert.Nil(ProjectCardDAVFailure(""))
	require.Error((PublicError{Code: PublicErrorSourceSyncFailed, Message: "arbitrary database text"}).Validate())
	require.Error((PublicError{Code: "unknown", Message: "fixed-looking"}).Validate())
}

func TestOperationQueryIsNormalizedAndPrivacyBounded(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Parallel()

	position := &Position{
		StartedAt: time.Date(2026, 8, 28, 12, 34, 56, 0, time.UTC),
		ID:        mustInt64ID(t, KindSourceSync, 12),
	}
	valid := Query{
		Kinds:    []Kind{KindCardDAVSync, KindSourceSync},
		States:   []State{StateFailed, StatePartial},
		Position: position,
		Limit:    25,
	}
	require.NoError(valid.Validate())
	require.Error((Query{Kinds: []Kind{KindSourceSync, KindCardDAVSync}, Limit: 25}).Validate(), "kinds must be normalized")
	require.Error((Query{Kinds: []Kind{KindSourceSync, KindSourceSync}, Limit: 25}).Validate(), "duplicate kinds must reject")
	require.Error((Query{States: []State{StatePartial, StateFailed}, Limit: 25}).Validate(), "states must be normalized")
	require.Error((Query{States: []State{StateFailed, StateFailed}, Limit: 25}).Validate(), "duplicate states must reject")
	require.Error((Query{Kinds: []Kind{"unknown"}, Limit: 25}).Validate())
	require.Error((Query{States: []State{"unknown"}, Limit: 25}).Validate())
	require.Error((Query{Limit: 0}).Validate())
	require.Error((Query{Limit: 101}).Validate())
	require.Error((Query{Position: &Position{}, Limit: 25}).Validate())
	require.Error((Query{
		Kinds: []Kind{KindCardDAVSync}, Position: position, Limit: 25,
	}).Validate(), "the position kind must be selected")
	nonUTCPosition := *position
	nonUTCPosition.StartedAt = position.StartedAt.In(time.FixedZone("synthetic", 2*60*60))
	require.Error((Query{Position: &nonUTCPosition, Limit: 25}).Validate(), "positions must be normalized to UTC")

	queryType := reflect.TypeFor[Query]()
	fields := make([]string, 0, queryType.NumField())
	for field := range queryType.Fields() {
		fields = append(fields, field.Name)
	}
	assert.Equal([]string{"Kinds", "States", "Position", "Limit"}, fields)
}

func TestOperationRunValidationRejectsCrossLaneAndArbitraryFailure(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	started := time.Date(2026, 8, 28, 12, 34, 56, 0, time.UTC)
	finished := started.Add(time.Minute)
	trigger := TriggerManual
	run := Run{
		ID:         mustInt64ID(t, KindCardDAVSync, 1),
		Lane:       LaneContacts,
		State:      StateSucceeded,
		Trigger:    &trigger,
		StartedAt:  started,
		FinishedAt: &finished,
		Counters: []PublicCounter{
			{Name: CounterBooks, Unit: CounterUnitBooks, Value: 1},
		},
	}
	require.NoError(run.Validate())

	wrongLane := run
	wrongLane.Lane = LaneMessages
	require.Error(wrongLane.Validate())

	badTime := run
	before := started.Add(-time.Second)
	badTime.FinishedAt = &before
	require.Error(badTime.Validate())

	badTrigger := run
	unknownTrigger := Trigger("unknown")
	badTrigger.Trigger = &unknownTrigger
	require.Error(badTrigger.Validate())

	sourceTrigger := run
	sourceTrigger.ID = mustInt64ID(t, KindSourceSync, 1)
	sourceTrigger.Lane = LaneMessages
	sourceTrigger.Counters = []PublicCounter{
		{Name: CounterProcessed, Unit: CounterUnitMessages, Value: 1},
	}
	require.Error(sourceTrigger.Validate(), "source history has no truthful trigger")

	queued := run
	queued.State = StateQueued
	require.Error(queued.Validate(), "the first-slice ledgers must not synthesize queued runs")

	nonUTC := run
	nonUTC.StartedAt = started.In(time.FixedZone("synthetic", 2*60*60))
	require.Error(nonUTC.Validate())

	badError := run
	badError.Error = &PublicError{Code: PublicErrorCardDAVSyncFailed, Message: "stored private marker"}
	require.Error(badError.Validate())

	succeededWithFixedFailure := run
	succeededWithFixedFailure.Error = ProjectCardDAVFailure("sync_failed")
	require.Error(succeededWithFixedFailure.Validate())
}

func TestOperationRunValidationEnforcesKindStateErrorMatrix(t *testing.T) {
	t.Parallel()

	sourceError := sourceSyncFailedError()
	personErrorValue := ProjectPersonSweepFailure(peoplesweep.FailureTimeout)
	personError := &personErrorValue
	cardDAVError := ProjectCardDAVFailure("upstream_failed")
	runningWithFinish := operationRunFixture(t, KindCardDAVSync, StateRunning, nil)
	runningFinishedAt := runningWithFinish.StartedAt.Add(time.Minute)
	runningWithFinish.FinishedAt = &runningFinishedAt
	failedWithoutFinish := operationRunFixture(t, KindCardDAVSync, StateFailed, cardDAVError)
	failedWithoutFinish.FinishedAt = nil
	sourceItemErrorPartial := operationRunFixture(t, KindSourceSync, StatePartial, nil)
	sourceItemErrorPartial.Counters = append(sourceItemErrorPartial.Counters, PublicCounter{
		Name: CounterItemErrors, Unit: CounterUnitMessages, Value: 1,
	})
	sourceAddedPartial := operationRunFixture(t, KindSourceSync, StatePartial, sourceError)
	sourceAddedPartial.Counters = append(sourceAddedPartial.Counters, PublicCounter{
		Name: CounterAdded, Unit: CounterUnitMessages, Value: 1,
	})
	sourceUpdatedPartial := operationRunFixture(t, KindSourceSync, StatePartial, sourceError)
	sourceUpdatedPartial.Counters = append(sourceUpdatedPartial.Counters, PublicCounter{
		Name: CounterUpdated, Unit: CounterUnitMessages, Value: 1,
	})
	sourceSucceededWithItemErrors := operationRunFixture(t, KindSourceSync, StateSucceeded, nil)
	sourceSucceededWithItemErrors.Counters = append(sourceSucceededWithItemErrors.Counters, PublicCounter{
		Name: CounterItemErrors, Unit: CounterUnitMessages, Value: 1,
	})
	sourceFailedAfterAdd := operationRunFixture(t, KindSourceSync, StateFailed, sourceError)
	sourceFailedAfterAdd.Counters = append(sourceFailedAfterAdd.Counters, PublicCounter{
		Name: CounterAdded, Unit: CounterUnitMessages, Value: 1,
	})
	sourceFailedAfterUpdate := operationRunFixture(t, KindSourceSync, StateFailed, sourceError)
	sourceFailedAfterUpdate.Counters = append(sourceFailedAfterUpdate.Counters, PublicCounter{
		Name: CounterUpdated, Unit: CounterUnitMessages, Value: 1,
	})

	tests := []struct {
		name    string
		run     Run
		wantErr bool
	}{
		{name: "source running", run: operationRunFixture(t, KindSourceSync, StateRunning, nil)},
		{name: "source succeeded", run: operationRunFixture(t, KindSourceSync, StateSucceeded, nil)},
		{name: "source partial from completed item errors", run: sourceItemErrorPartial},
		{name: "source partial from failed add", run: sourceAddedPartial},
		{name: "source partial from failed update", run: sourceUpdatedPartial},
		{name: "source failed", run: operationRunFixture(t, KindSourceSync, StateFailed, sourceError)},
		{name: "source legacy cancelled", run: operationRunFixture(t, KindSourceSync, StateCancelled, nil)},
		{name: "source succeeded with item errors", run: sourceSucceededWithItemErrors, wantErr: true},
		{name: "source failed after add", run: sourceFailedAfterAdd, wantErr: true},
		{name: "source failed after update", run: sourceFailedAfterUpdate, wantErr: true},
		{name: "source failed missing error", run: operationRunFixture(t, KindSourceSync, StateFailed, nil), wantErr: true},
		{name: "source completed-origin partial with processed only", run: operationRunFixture(t, KindSourceSync, StatePartial, nil), wantErr: true},
		{name: "source failed-origin partial with processed only", run: operationRunFixture(t, KindSourceSync, StatePartial, sourceError), wantErr: true},
		{name: "source failed with people error", run: operationRunFixture(t, KindSourceSync, StateFailed, personError), wantErr: true},
		{name: "source partial with CardDAV error", run: operationRunFixture(t, KindSourceSync, StatePartial, cardDAVError), wantErr: true},
		{name: "source cancelled with failure", run: operationRunFixture(t, KindSourceSync, StateCancelled, sourceError), wantErr: true},

		{name: "person running", run: operationRunFixture(t, KindPersonSweep, StateRunning, nil)},
		{name: "person succeeded", run: operationRunFixture(t, KindPersonSweep, StateSucceeded, nil)},
		{name: "person partial", run: operationRunFixture(t, KindPersonSweep, StatePartial, personError)},
		{name: "person failed", run: operationRunFixture(t, KindPersonSweep, StateFailed, personError)},
		{name: "person cancelled unsupported", run: operationRunFixture(t, KindPersonSweep, StateCancelled, personError), wantErr: true},
		{name: "person partial missing error", run: operationRunFixture(t, KindPersonSweep, StatePartial, nil), wantErr: true},
		{name: "person failed missing error", run: operationRunFixture(t, KindPersonSweep, StateFailed, nil), wantErr: true},
		{name: "person failed with source error", run: operationRunFixture(t, KindPersonSweep, StateFailed, sourceError), wantErr: true},
		{name: "person failed with CardDAV error", run: operationRunFixture(t, KindPersonSweep, StateFailed, cardDAVError), wantErr: true},

		{name: "CardDAV running", run: operationRunFixture(t, KindCardDAVSync, StateRunning, nil)},
		{name: "CardDAV succeeded", run: operationRunFixture(t, KindCardDAVSync, StateSucceeded, nil)},
		{name: "CardDAV partial", run: operationRunFixture(t, KindCardDAVSync, StatePartial, cardDAVError)},
		{name: "CardDAV failed", run: operationRunFixture(t, KindCardDAVSync, StateFailed, cardDAVError)},
		{name: "CardDAV cancelled", run: operationRunFixture(t, KindCardDAVSync, StateCancelled, ProjectCardDAVFailure("cancelled"))},
		{name: "CardDAV partial missing error", run: operationRunFixture(t, KindCardDAVSync, StatePartial, nil), wantErr: true},
		{name: "CardDAV failed missing error", run: operationRunFixture(t, KindCardDAVSync, StateFailed, nil), wantErr: true},
		{name: "CardDAV cancelled missing error", run: operationRunFixture(t, KindCardDAVSync, StateCancelled, nil), wantErr: true},
		{name: "CardDAV failed with people error", run: operationRunFixture(t, KindCardDAVSync, StateFailed, personError), wantErr: true},
		{name: "CardDAV failed with source error", run: operationRunFixture(t, KindCardDAVSync, StateFailed, sourceError), wantErr: true},
		{name: "running with finish", run: runningWithFinish, wantErr: true},
		{name: "terminal without finish", run: failedWithoutFinish, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.run.Validate()
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestLaneHistoryStatusValidationEnforcesRegistryAndRunRoles(t *testing.T) {
	newRequirements := require.New
	require := require.New(t)
	t.Parallel()

	active := operationRunFixture(t, KindCardDAVSync, StateRunning, nil)
	active.ID = mustInt64ID(t, KindCardDAVSync, 3)
	latest := operationRunFixture(t, KindCardDAVSync, StateFailed, ProjectCardDAVFailure("upstream_failed"))
	latest.ID = mustInt64ID(t, KindCardDAVSync, 4)
	latestSuccessful := operationRunFixture(t, KindCardDAVSync, StateSucceeded, nil)
	latestSuccessful.ID = mustInt64ID(t, KindCardDAVSync, 2)
	newerSuccessful := latestSuccessful
	newerSuccessful.ID = mustInt64ID(t, KindCardDAVSync, 5)
	valid := LaneHistoryStatus{
		Kind: KindCardDAVSync, Lane: LaneContacts, HistoryAvailability: HistoryAvailable,
		Active: &active, Latest: &active, LatestSuccessful: &latestSuccessful,
	}
	require.NoError(valid.Validate())
	require.NoError((LaneHistoryStatus{
		Kind: KindCardDAVSync, Lane: LaneContacts, HistoryAvailability: HistoryAvailable,
		Latest: &latest, LatestSuccessful: &latestSuccessful,
	}).Validate(), "the latest run may be terminal")
	require.NoError((LaneHistoryStatus{
		Kind: KindSourceSync, Lane: LaneMessages, HistoryAvailability: HistoryAvailable,
	}).Validate(), "available history with no runs is valid")
	require.NoError((LaneHistoryStatus{
		Kind: KindVisualEmbedding, Lane: LaneVisualAttachments,
		HistoryAvailability: HistoryUnavailable,
		UnavailableCode:     "visual_embedding_history_unavailable",
	}).Validate())

	tests := []struct {
		name   string
		mutate func(*testing.T, *LaneHistoryStatus)
	}{
		{name: "unknown kind", mutate: func(_ *testing.T, status *LaneHistoryStatus) { status.Kind = "unknown" }},
		{name: "wrong lane", mutate: func(_ *testing.T, status *LaneHistoryStatus) { status.Lane = LaneMessages }},
		{name: "wrong availability", mutate: func(_ *testing.T, status *LaneHistoryStatus) { status.HistoryAvailability = HistoryUnavailable }},
		{name: "available with unavailable code", mutate: func(_ *testing.T, status *LaneHistoryStatus) { status.UnavailableCode = "synthetic_unavailable" }},
		{name: "active terminal", mutate: func(_ *testing.T, status *LaneHistoryStatus) { status.Active = &latestSuccessful }},
		{name: "active wrong kind", mutate: func(t *testing.T, status *LaneHistoryStatus) {
			t.Helper()
			wrong := operationRunFixture(t, KindSourceSync, StateRunning, nil)
			status.Active = &wrong
		}},
		{name: "latest wrong kind", mutate: func(t *testing.T, status *LaneHistoryStatus) {
			t.Helper()
			wrong := operationRunFixture(t, KindPersonSweep, StateFailed, personFailureForTest())
			status.Latest = &wrong
		}},
		{name: "latest successful partial", mutate: func(t *testing.T, status *LaneHistoryStatus) {
			t.Helper()
			partial := operationRunFixture(t, KindCardDAVSync, StatePartial, ProjectCardDAVFailure("sync_failed"))
			status.LatestSuccessful = &partial
		}},
		{name: "latest successful wrong kind", mutate: func(t *testing.T, status *LaneHistoryStatus) {
			t.Helper()
			wrong := operationRunFixture(t, KindSourceSync, StateSucceeded, nil)
			status.LatestSuccessful = &wrong
		}},
		{name: "latest successful without latest", mutate: func(_ *testing.T, status *LaneHistoryStatus) {
			status.Active = nil
			status.Latest = nil
		}},
		{name: "latest successful newer than latest", mutate: func(_ *testing.T, status *LaneHistoryStatus) {
			status.Active = nil
			status.Latest = &latest
			status.LatestSuccessful = &newerSuccessful
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := newRequirements(t)
			t.Parallel()
			status := valid
			test.mutate(t, &status)
			require.Error(status.Validate())
		})
	}

	unavailable := LaneHistoryStatus{
		Kind: KindVisualEmbedding, Lane: LaneVisualAttachments,
		HistoryAvailability: HistoryUnavailable,
		UnavailableCode:     "visual_embedding_history_unavailable",
	}
	t.Run("unavailable wrong code", func(t *testing.T) {
		require := newRequirements(t)
		status := unavailable
		status.UnavailableCode = "generic_unavailable"
		require.Error(status.Validate())
	})
	t.Run("unavailable carries run", func(t *testing.T) {
		require := newRequirements(t)
		status := unavailable
		status.Latest = &latest
		require.Error(status.Validate())
	})
}

func TestHistoryReaderContractUsesNormalizedTypes(t *testing.T) {
	t.Parallel()

	var _ HistoryReader = (*readerContractStub)(nil)
}

type readerContractStub struct{}

func (*readerContractStub) Kinds() []Kind { return nil }

func (*readerContractStub) ListRuns(context.Context, Query) ([]Run, error) { return nil, nil }

func (*readerContractStub) GetRun(context.Context, StableID) (Run, error) { return Run{}, nil }

func (*readerContractStub) LaneStatus(context.Context, Kind) (LaneHistoryStatus, error) {
	return LaneHistoryStatus{}, nil
}

func mustInt64ID(t *testing.T, kind Kind, id int64) StableID {
	t.Helper()
	stableID, err := NewInt64ID(kind, id)
	require.NoError(t, err)
	return stableID
}

func mustTextID(t *testing.T, kind Kind, id string) StableID {
	t.Helper()
	stableID, err := NewTextID(kind, id)
	require.NoError(t, err)
	return stableID
}

func runIDs(runs []Run) []StableID {
	ids := make([]StableID, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	return ids
}

func sourceSyncFailedError() *PublicError {
	return &PublicError{
		Code:    PublicErrorSourceSyncFailed,
		Message: "Source sync failed.",
	}
}

func operationRunFixture(t *testing.T, kind Kind, state State, publicError *PublicError) Run {
	t.Helper()

	started := time.Date(2026, 8, 28, 12, 34, 56, 0, time.UTC)
	run := Run{State: state, StartedAt: started, Error: publicError}
	switch kind {
	case KindSourceSync:
		run.ID = mustInt64ID(t, kind, 1)
		run.Lane = LaneMessages
		run.Counters = []PublicCounter{{Name: CounterProcessed, Unit: CounterUnitMessages, Value: 1}}
	case KindPersonSweep:
		run.ID = mustTextID(t, kind, "run-fixture")
		run.Lane = LanePersonFacts
		run.Counters = []PublicCounter{{Name: CounterAttempted, Unit: CounterUnitPeople, Value: 1}}
		trigger := TriggerManual
		run.Trigger = &trigger
	case KindCardDAVSync:
		run.ID = mustInt64ID(t, kind, 1)
		run.Lane = LaneContacts
		run.Counters = []PublicCounter{{Name: CounterBooks, Unit: CounterUnitBooks, Value: 1}}
		trigger := TriggerScheduled
		run.Trigger = &trigger
	default:
		require.FailNow(t, "unsupported operation run fixture kind", string(kind))
	}
	if state != StateRunning {
		finished := started.Add(time.Minute)
		run.FinishedAt = &finished
	}
	return run
}

func personFailureForTest() *PublicError {
	value := ProjectPersonSweepFailure(peoplesweep.FailureInternal)
	return &value
}
