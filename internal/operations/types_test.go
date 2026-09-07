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
				"failed", "projected_writes", "books", "created", "removed", "truncated",
				"skipped", "requested", "started", "suppressed", "identity_rejected",
			},
			validate: func(value string) error { return CounterName(value).Validate() },
		},
		{
			name:     "counter unit",
			valid:    []string{"messages", "people", "writes", "books", "contacts", "documents", "chunks", "attachments"},
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

func TestOperationPublicEnumDomainsEnumerateEveryRuntimeValue(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []CounterName{
		CounterAdded, CounterAttempted, CounterBooks, CounterCreated, CounterFailed,
		CounterIdentityRejected, CounterItemErrors, CounterProcessed, CounterProjectedWrites,
		CounterRemoved, CounterRequested, CounterSkipped, CounterStarted, CounterSucceeded,
		CounterSuppressed, CounterTruncated, CounterUpdated,
	}, CounterNames())
	assert.Equal(t, []CounterUnit{
		CounterUnitAttachments, CounterUnitBooks, CounterUnitChunks, CounterUnitContacts,
		CounterUnitDocuments, CounterUnitMessages, CounterUnitPeople, CounterUnitWrites,
	}, CounterUnits())
	assert.Equal(t, []PublicErrorCode{
		PublicErrorArchiveGap, PublicErrorAuthenticationFailed, PublicErrorBudget,
		PublicErrorCancelled, PublicErrorCardDAVSyncFailed, PublicErrorDaemonRestarted,
		PublicErrorInternal, PublicErrorInvalidOutput, PublicErrorInvocationArchiveDrift,
		PublicErrorInvocationAuthenticationFailed, PublicErrorInvocationCancelled,
		PublicErrorInvocationDaemonRestarted, PublicErrorInvocationInternal,
		PublicErrorInvocationInvalidOutput, PublicErrorInvocationRateLimited,
		PublicErrorInvocationSafetyLimit, PublicErrorInvocationTimeout,
		PublicErrorInvocationUnsafeErrorRedacted, PublicErrorInvocationUpstreamFailed,
		PublicErrorLeaseLost, PublicErrorPersonSweepFailed, PublicErrorPolicy,
		PublicErrorProviderHTTP, PublicErrorRateLimited, PublicErrorRetryAfter,
		PublicErrorSafetyLimit, PublicErrorSourceSyncFailed, PublicErrorSyncFailed,
		PublicErrorTimeout, PublicErrorUnsafeErrorRedacted, PublicErrorUpstreamFailed,
	}, PublicErrorCodes())
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
	messageEmbedding, err := NewInt64ID(KindMessageEmbedding, 10)
	require.NoError(err)
	assert.Equal(KindMessageEmbedding, messageEmbedding.Kind())
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

func TestOperationCounterMatricesAreClosedForEveryKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind Kind
		want []PublicCounter
	}{
		{KindSourceSync, []PublicCounter{
			{Name: CounterProcessed, Unit: CounterUnitMessages, Value: 1},
			{Name: CounterAdded, Unit: CounterUnitMessages, Value: 1},
			{Name: CounterUpdated, Unit: CounterUnitMessages, Value: 1},
			{Name: CounterItemErrors, Unit: CounterUnitMessages, Value: 1},
		}},
		{KindPersonSweep, []PublicCounter{
			{Name: CounterAttempted, Unit: CounterUnitPeople, Value: 1},
			{Name: CounterSucceeded, Unit: CounterUnitPeople, Value: 1},
			{Name: CounterFailed, Unit: CounterUnitPeople, Value: 1},
			{Name: CounterProjectedWrites, Unit: CounterUnitWrites, Value: 1},
		}},
		{KindCardDAVSync, []PublicCounter{
			{Name: CounterBooks, Unit: CounterUnitBooks, Value: 1},
			{Name: CounterCreated, Unit: CounterUnitContacts, Value: 1},
			{Name: CounterUpdated, Unit: CounterUnitContacts, Value: 1},
			{Name: CounterRemoved, Unit: CounterUnitContacts, Value: 1},
		}},
		{KindMessageEmbedding, []PublicCounter{
			{Name: CounterAttempted, Unit: CounterUnitMessages, Value: 1},
			{Name: CounterSucceeded, Unit: CounterUnitMessages, Value: 1},
			{Name: CounterFailed, Unit: CounterUnitMessages, Value: 1},
			{Name: CounterTruncated, Unit: CounterUnitMessages, Value: 1},
		}},
		{KindPersonEmbedding, []PublicCounter{
			{Name: CounterAttempted, Unit: CounterUnitPeople, Value: 1},
			{Name: CounterSucceeded, Unit: CounterUnitPeople, Value: 1},
			{Name: CounterFailed, Unit: CounterUnitPeople, Value: 1},
			{Name: CounterTruncated, Unit: CounterUnitPeople, Value: 1},
		}},
		{KindDocumentExtraction, []PublicCounter{
			{Name: CounterAttempted, Unit: CounterUnitDocuments, Value: 1},
			{Name: CounterSucceeded, Unit: CounterUnitDocuments, Value: 1},
			{Name: CounterFailed, Unit: CounterUnitDocuments, Value: 1},
		}},
		{KindDocumentEmbedding, []PublicCounter{
			{Name: CounterAttempted, Unit: CounterUnitChunks, Value: 1},
			{Name: CounterSucceeded, Unit: CounterUnitChunks, Value: 1},
			{Name: CounterFailed, Unit: CounterUnitChunks, Value: 1},
		}},
		{KindVisualEmbedding, []PublicCounter{
			{Name: CounterAttempted, Unit: CounterUnitAttachments, Value: 1},
			{Name: CounterSucceeded, Unit: CounterUnitAttachments, Value: 1},
			{Name: CounterFailed, Unit: CounterUnitAttachments, Value: 1},
			{Name: CounterSkipped, Unit: CounterUnitAttachments, Value: 1},
		}},
		{KindPersonEnrichment, []PublicCounter{
			{Name: CounterRequested, Unit: CounterUnitPeople, Value: 1},
			{Name: CounterStarted, Unit: CounterUnitPeople, Value: 1},
			{Name: CounterSucceeded, Unit: CounterUnitPeople, Value: 1},
			{Name: CounterFailed, Unit: CounterUnitPeople, Value: 1},
			{Name: CounterSuppressed, Unit: CounterUnitPeople, Value: 1},
			{Name: CounterIdentityRejected, Unit: CounterUnitPeople, Value: 1},
		}},
	}

	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			t.Parallel()
			require.NoError(t, ValidateCounters(test.kind, test.want))
			wrong := append([]PublicCounter(nil), test.want...)
			wrong = append(wrong, PublicCounter{Name: CounterCreated, Unit: CounterUnitContacts, Value: 1})
			require.Error(t, ValidateCounters(test.kind, wrong))
		})
	}
}

func TestInvocationContractValidatesKeysCheckpointsAndFinalOutcomes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()

	instant := time.Date(2026, 8, 30, 12, 0, 0, 0, time.FixedZone("synthetic", 2*60*60))
	spec := InvocationSpec{
		Kind: KindMessageEmbedding, Key: "scheduler:2026-08-30T12:00:00Z",
		Trigger: TriggerScheduled, StartedAt: instant,
	}
	require.NoError(spec.Validate())
	assert.Equal(time.UTC, spec.Normalized().StartedAt.Location())

	for _, key := range []string{"", " leading", strings.Repeat("x", MaxInvocationKeyBytes+1), string([]byte{0xff})} {
		invalid := spec
		invalid.Key = key
		require.Error(invalid.Validate())
	}

	previous := InvocationCounters{Attempted: 3, Succeeded: 2, Failed: 1}
	checkpoint := InvocationCounters{Attempted: 4, Succeeded: 3, Failed: 1}
	require.NoError(checkpoint.ValidateCheckpoint(KindMessageEmbedding, previous))
	require.Error(previous.ValidateCheckpoint(KindMessageEmbedding, checkpoint))
	require.Error((InvocationCounters{Requested: 1}).Validate(KindMessageEmbedding))
	assert.Equal(InvocationCounters{Attempted: 3, Succeeded: 2, Failed: 1}, previous)

	require.NoError(previous.ValidateFinal(KindMessageEmbedding))
	require.Error((InvocationCounters{Attempted: 4, Succeeded: 2, Failed: 1}).ValidateFinal(KindMessageEmbedding))
	require.NoError((InvocationCounters{Attempted: 4, Succeeded: 2, Failed: 1, Skipped: 1}).ValidateFinal(KindVisualEmbedding))
	require.Error((InvocationCounters{Attempted: 4, Succeeded: 2, Failed: 1}).ValidateFinal(KindVisualEmbedding))

	partialError := FixedPublicError(PublicErrorInvocationUpstreamFailed)
	state, err := DeriveInvocationState(KindMessageEmbedding, previous, partialError)
	require.NoError(err)
	assert.Equal(StatePartial, state)
	state, err = DeriveInvocationState(KindMessageEmbedding, InvocationCounters{}, partialError)
	require.NoError(err)
	assert.Equal(StateFailed, state)
	state, err = DeriveInvocationState(KindMessageEmbedding, InvocationCounters{}, FixedPublicError(PublicErrorInvocationCancelled))
	require.NoError(err)
	assert.Equal(StateCancelled, state)
	_, err = DeriveInvocationState(KindMessageEmbedding, previous, FixedPublicError(PublicErrorProviderHTTP))
	require.Error(err, "a people-sweep error must not enter an invocation ledger")
	_, err = DeriveInvocationState(KindDocumentExtraction, previous, &PublicError{
		Code: "not_registered", Message: "Not registered.",
	})
	require.Error(err, "an unknown error must not enter an invocation ledger")

	require.Error((&PublicError{Code: PublicErrorUpstreamFailed, Message: "provider returned a private detail"}).Validate())
}

func TestOperationRunQueuedStateIsLimitedToPersonEnrichment(t *testing.T) {
	require := require.New(t)
	t.Parallel()

	started := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	enrichmentID, err := NewInt64ID(KindPersonEnrichment, 1)
	require.NoError(err)
	queued := Run{
		ID: enrichmentID, Lane: LanePersonFacts, State: StateQueued,
		StartedAt: started,
	}
	require.NoError(queued.Validate())

	for _, kind := range []Kind{
		KindMessageEmbedding, KindPersonEmbedding, KindDocumentExtraction,
		KindDocumentEmbedding, KindVisualEmbedding,
	} {
		id, idErr := NewInt64ID(kind, 1)
		require.NoError(idErr)
		candidate := queued
		candidate.ID = id
		definition, ok := laneDefinition(kind)
		require.True(ok)
		candidate.Lane = definition.Lane
		require.Error(candidate.Validate(), kind)
	}
}

func TestInvocationPublicErrorKindCodeMatrix(t *testing.T) {
	t.Parallel()
	kinds := []Kind{
		KindMessageEmbedding, KindPersonEmbedding, KindDocumentExtraction,
		KindDocumentEmbedding, KindVisualEmbedding, KindPersonEnrichment,
	}
	allowed := []PublicErrorCode{
		PublicErrorInvocationCancelled, PublicErrorInvocationTimeout,
		PublicErrorInvocationRateLimited, PublicErrorInvocationAuthenticationFailed,
		PublicErrorInvocationUpstreamFailed, PublicErrorInvocationInvalidOutput,
		PublicErrorInvocationSafetyLimit, PublicErrorInvocationArchiveDrift,
		PublicErrorInvocationDaemonRestarted, PublicErrorInvocationInternal,
		PublicErrorInvocationUnsafeErrorRedacted,
	}
	for _, kind := range kinds {
		for _, code := range allowed {
			require.NoError(t, ValidateInvocationPublicError(kind, FixedPublicError(code)))
		}
		for _, code := range []PublicErrorCode{PublicErrorProviderHTTP, PublicErrorUpstreamFailed, "not_registered"} {
			publicError := FixedPublicError(code)
			if publicError == nil {
				publicError = &PublicError{Code: code, Message: "Not registered."}
			}
			require.Error(t, ValidateInvocationPublicError(kind, publicError))
		}
	}
	require.Error(t, ValidateInvocationPublicError(KindCardDAVSync,
		FixedPublicError(PublicErrorInvocationUpstreamFailed)))
}

func TestTerminalReplayOutcomePreservesFixedNonSuccessSemantics(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 30, 14, 0, 0, 0, time.UTC)
	finished := started.Add(time.Minute)
	trigger := TriggerManual
	id := mustInt64ID(t, KindMessageEmbedding, 41)
	tests := []struct {
		name      string
		state     State
		counters  InvocationCounters
		publicErr *PublicError
		wantCode  PublicErrorCode
		wantIs    error
		wantErr   bool
	}{
		{name: "succeeded replay", state: StateSucceeded,
			counters: InvocationCounters{Attempted: 1, Succeeded: 1}},
		{name: "partial replay remains a useful completed outcome", state: StatePartial,
			counters:  InvocationCounters{Attempted: 2, Succeeded: 1, Failed: 1},
			publicErr: FixedPublicError(PublicErrorInvocationUpstreamFailed)},
		{name: "failed replay", state: StateFailed,
			counters:  InvocationCounters{Attempted: 1, Failed: 1},
			publicErr: FixedPublicError(PublicErrorInvocationUpstreamFailed),
			wantCode:  PublicErrorInvocationUpstreamFailed, wantErr: true},
		{name: "cancelled replay", state: StateCancelled,
			publicErr: FixedPublicError(PublicErrorInvocationCancelled),
			wantCode:  PublicErrorInvocationCancelled, wantIs: context.Canceled, wantErr: true},
		{name: "timed out replay", state: StateFailed,
			publicErr: FixedPublicError(PublicErrorInvocationTimeout),
			wantCode:  PublicErrorInvocationTimeout, wantIs: context.DeadlineExceeded, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			run := &Run{
				ID: id, Lane: LaneMessages, State: test.state, Trigger: &trigger,
				StartedAt: started, FinishedAt: &finished,
				Counters: test.counters.PublicCounters(KindMessageEmbedding), Error: test.publicErr,
			}
			err := TerminalReplayOutcome(run)
			if !test.wantErr {
				require.NoError(err)
				return
			}
			require.Error(err)
			var terminalErr *TerminalReplayError
			require.ErrorAs(err, &terminalErr)
			assert.Equal(test.state, terminalErr.State())
			assert.Equal(test.wantCode, terminalErr.Code())
			assert.Equal(FixedPublicError(test.wantCode).Message, terminalErr.Error())
			assert.NotContains(terminalErr.Error(), "private provider response")
			if test.wantIs != nil {
				assert.ErrorIs(err, test.wantIs)
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
	startedFrom := position.StartedAt.Add(-time.Hour)
	startedBefore := position.StartedAt.Add(time.Hour)
	valid := Query{
		Kinds:         []Kind{KindCardDAVSync, KindSourceSync},
		States:        []State{StateFailed, StatePartial},
		StartedFrom:   &startedFrom,
		StartedBefore: &startedBefore,
		Position:      position,
		Limit:         25,
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
	nonUTCFrom := startedFrom.In(time.FixedZone("synthetic", 2*60*60))
	require.Error((Query{StartedFrom: &nonUTCFrom, Limit: 25}).Validate(), "lower date bounds must be normalized to UTC")
	nonUTCBefore := startedBefore.In(time.FixedZone("synthetic", -7*60*60))
	require.Error((Query{StartedBefore: &nonUTCBefore, Limit: 25}).Validate(), "upper date bounds must be normalized to UTC")
	equalBound := startedFrom
	require.Error((Query{StartedFrom: &startedFrom, StartedBefore: &equalBound, Limit: 25}).Validate(),
		"date bounds form a nonempty half-open interval")
	reversedBound := startedFrom.Add(-time.Second)
	require.Error((Query{StartedFrom: &startedFrom, StartedBefore: &reversedBound, Limit: 25}).Validate(),
		"the lower date bound must precede the upper date bound")

	queryType := reflect.TypeFor[Query]()
	fields := make([]string, 0, queryType.NumField())
	for field := range queryType.Fields() {
		fields = append(fields, field.Name)
	}
	assert.Equal([]string{"Kinds", "States", "StartedFrom", "StartedBefore", "Position", "Limit"}, fields)
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
		Kind: KindPersonSweep, Lane: LanePersonFacts,
		HistoryAvailability: HistoryUnavailable,
		UnavailableCode:     "person_sweep_history_unavailable",
	}).Validate(), "a normally available native history may become dynamically unavailable")
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
		{name: "unavailable carries available runs", mutate: func(_ *testing.T, status *LaneHistoryStatus) { status.HistoryAvailability = HistoryUnavailable }},
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
	t.Run("dynamic unavailable wrong code", func(t *testing.T) {
		require := newRequirements(t)
		require.Error((LaneHistoryStatus{
			Kind: KindPersonSweep, Lane: LanePersonFacts,
			HistoryAvailability: HistoryUnavailable,
			UnavailableCode:     "generic_unavailable",
		}).Validate())
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

func (*readerContractStub) ListRuns(context.Context, Query) (HistorySnapshot, error) {
	return HistorySnapshot{}, nil
}

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
