package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/visual"
)

const operationTestArchiveUID = "1234567890abcdef1234567890abcdef"

type operationArchiveStore struct {
	*mockStore

	uid string
	err error
}

var operationTestTokenKey = store.OperationTokenKey{
	KeyID: strings.Repeat("1", 32), KeyBytes: bytes.Repeat([]byte{0x42}, 32),
	State: store.OperationTokenKeyActive,
}

type operationArchiveRealStore struct {
	*store.Store

	uid string
}

func (s *operationArchiveRealStore) ArchiveUIDContext(context.Context) (string, error) {
	return s.uid, nil
}

func (s *operationArchiveStore) ArchiveUIDContext(context.Context) (string, error) {
	return s.uid, s.err
}

func (*operationArchiveStore) ActiveOperationTokenKey(context.Context) (store.OperationTokenKey, error) {
	return operationTestTokenKey, nil
}

func (*operationArchiveStore) OperationTokenKey(context.Context, string) (store.OperationTokenKey, error) {
	return operationTestTokenKey, nil
}

type operationHistoryStub struct {
	runs          []operations.Run
	snapshots     []operations.HistorySnapshot
	run           operations.Run
	listErr       error
	getErr        error
	status        map[operations.Kind]operations.LaneHistoryStatus
	statusErr     map[operations.Kind]error
	statusQueries []operations.Kind
	queries       []operations.Query
}

func (*operationHistoryStub) Kinds() []operations.Kind {
	return []operations.Kind{operations.KindCardDAVSync, operations.KindPersonSweep, operations.KindSourceSync}
}

func (s *operationHistoryStub) ListRuns(_ context.Context, query operations.Query) (operations.HistorySnapshot, error) {
	index := len(s.queries)
	s.queries = append(s.queries, query)
	if index < len(s.snapshots) {
		return s.snapshots[index], s.listErr
	}
	available := query.Kinds
	if len(available) == 0 {
		available = s.Kinds()
	}
	snapshot := operations.HistorySnapshot{
		Runs: s.runs, AvailableKinds: available, MembershipRevision: 1,
	}
	if len(s.runs) > query.Limit {
		position := operations.Position{StartedAt: s.runs[query.Limit-1].StartedAt, ID: s.runs[query.Limit-1].ID}
		snapshot.Position = &position
	}
	return snapshot, s.listErr
}

func (s *operationHistoryStub) GetRun(context.Context, operations.StableID) (operations.Run, error) {
	return s.run, s.getErr
}

func (s *operationHistoryStub) LaneStatus(_ context.Context, kind operations.Kind) (operations.LaneHistoryStatus, error) {
	s.statusQueries = append(s.statusQueries, kind)
	if err := s.statusErr[kind]; err != nil {
		return operations.LaneHistoryStatus{}, err
	}
	if status, ok := s.status[kind]; ok {
		return status, nil
	}
	for _, definition := range operations.LaneRegistry() {
		if definition.Kind == kind {
			return operations.LaneHistoryStatus{
				Kind: kind, Lane: definition.Lane,
				HistoryAvailability: operations.HistoryAvailable,
			}, nil
		}
	}
	return operations.LaneHistoryStatus{}, errors.New("unknown operation kind")
}

func newOperationTestServer(reader operations.HistoryReader, archive ArchiveIdentifier) *Server {
	var messageStore MessageStore = &mockStore{}
	if archive != nil {
		var ok bool
		messageStore, ok = archive.(MessageStore)
		if !ok {
			panic("operation test archive must implement MessageStore")
		}
	}
	return NewServerWithOptions(ServerOptions{
		Config:                 &config.Config{},
		Store:                  messageStore,
		OperationHistoryReader: reader,
		Logger:                 testLogger(),
	})
}

func operationRunFixture(t *testing.T) operations.Run {
	t.Helper()
	id, err := operations.NewInt64ID(operations.KindSourceSync, 17)
	require.NoError(t, err)
	finished := time.Date(2026, 8, 29, 12, 0, 1, 0, time.UTC)
	return operations.Run{
		ID: id, Lane: operations.LaneMessages, State: operations.StateSucceeded,
		StartedAt: finished.Add(-time.Second), FinishedAt: &finished,
		Counters: []operations.PublicCounter{
			{Name: operations.CounterProcessed, Unit: operations.CounterUnitMessages, Value: 4},
			{Name: operations.CounterAdded, Unit: operations.CounterUnitMessages, Value: 2},
			{Name: operations.CounterUpdated, Unit: operations.CounterUnitMessages, Value: 1},
			{Name: operations.CounterItemErrors, Unit: operations.CounterUnitMessages, Value: 0},
		},
	}
}

func operationRunForKindFixture(t *testing.T, kind operations.Kind, state operations.State) operations.Run {
	t.Helper()
	run := operationRunFixture(t)
	var lane operations.Lane
	for _, definition := range operations.LaneRegistry() {
		if definition.Kind == kind {
			lane = definition.Lane
			break
		}
	}
	require.NotEmpty(t, lane)
	run.ID = mustOperationIntID(t, kind, 17)
	run.Lane = lane
	run.State = state
	run.Counters = []operations.PublicCounter{}
	if state == operations.StateRunning || state == operations.StateQueued {
		run.FinishedAt = nil
	}
	return run
}

func TestOperationStatusReturnsExactRegistryAndNonNullActions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Vector.Enabled = true
	cfg.Vector.People.Enabled = true
	cfg.Attachments.Documents.Enabled = true
	cfg.Attachments.Documents.Index.Embeddings.Enabled = true
	cfg.People.Sweep.Enabled = true
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("fixture", "synthetic-source")
	require.NoError(err)
	reader := &operationHistoryStub{}
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg, Store: st, OperationHistoryReader: reader, Logger: testLogger(),
	})

	w := doGet(srv, "/api/v1/operations/status")
	require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var body OperationStatusResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(body.Lanes, 9)
	assert.Equal([]operations.Kind{
		operations.KindCardDAVSync,
		operations.KindDocumentEmbedding,
		operations.KindDocumentExtraction,
		operations.KindMessageEmbedding,
		operations.KindPersonEmbedding,
		operations.KindPersonEnrichment,
		operations.KindPersonSweep,
		operations.KindSourceSync,
		operations.KindVisualEmbedding,
	}, []operations.Kind{
		body.Lanes[0].Kind, body.Lanes[1].Kind, body.Lanes[2].Kind,
		body.Lanes[3].Kind, body.Lanes[4].Kind, body.Lanes[5].Kind,
		body.Lanes[6].Kind, body.Lanes[7].Kind, body.Lanes[8].Kind,
	})
	assert.Equal([]operations.Lane{
		operations.LaneContacts,
		operations.LaneDocuments,
		operations.LaneDocuments,
		operations.LaneMessages,
		operations.LanePersonFacts,
		operations.LanePersonFacts,
		operations.LanePersonFacts,
		operations.LaneMessages,
		operations.LaneVisualAttachments,
	}, []operations.Lane{
		body.Lanes[0].Lane, body.Lanes[1].Lane, body.Lanes[2].Lane,
		body.Lanes[3].Lane, body.Lanes[4].Lane, body.Lanes[5].Lane,
		body.Lanes[6].Lane, body.Lanes[7].Lane, body.Lanes[8].Lane,
	})
	assert.Equal([]string{"", "", "", "", "", "", "", "", ""}, []string{
		body.Lanes[0].UnavailableCode, body.Lanes[1].UnavailableCode,
		body.Lanes[2].UnavailableCode, body.Lanes[3].UnavailableCode,
		body.Lanes[4].UnavailableCode, body.Lanes[5].UnavailableCode,
		body.Lanes[6].UnavailableCode, body.Lanes[7].UnavailableCode,
		body.Lanes[8].UnavailableCode,
	})
	for _, lane := range body.Lanes {
		assert.NotNil(lane.SupportedActions, "lane %s", lane.Kind)
	}
	assert.Equal([]operations.Kind{
		operations.KindCardDAVSync,
		operations.KindDocumentEmbedding,
		operations.KindDocumentExtraction,
		operations.KindMessageEmbedding,
		operations.KindPersonEmbedding,
		operations.KindPersonEnrichment,
		operations.KindPersonSweep,
		operations.KindSourceSync,
		operations.KindVisualEmbedding,
	}, reader.statusQueries)
	for _, lane := range body.Lanes {
		assert.Equal(operations.HistoryAvailable, lane.HistoryAvailability, "lane %s", lane.Kind)
	}
	assert.False(body.Lanes[0].Configured)
	assert.True(body.Lanes[1].Configured)
	assert.False(body.Lanes[2].Configured, "a document config without a durable selected profile is not configured")
	assert.True(body.Lanes[3].Configured)
	assert.True(body.Lanes[4].Configured)
	assert.False(body.Lanes[5].Configured, "external enrichment remains inactive")
	assert.True(body.Lanes[6].Configured)
	assert.True(body.Lanes[7].Configured)
	assert.False(body.Lanes[8].Configured)
	assert.Equal(operations.RelatedStatusCardDAV, *body.Lanes[0].RelatedStatus)
	assert.Equal(operations.RelatedStatusDocumentVector, *body.Lanes[1].RelatedStatus)
	assert.Equal(operations.RelatedStatusDocumentIndex, *body.Lanes[2].RelatedStatus)
	assert.Nil(body.Lanes[3].RelatedStatus)
	assert.Nil(body.Lanes[4].RelatedStatus)
	assert.Nil(body.Lanes[5].RelatedStatus)
	assert.Nil(body.Lanes[6].RelatedStatus)
	assert.Equal(operations.RelatedStatusSource, *body.Lanes[7].RelatedStatus)
	assert.Equal(operations.RelatedStatusVisual, *body.Lanes[8].RelatedStatus)
	assert.NotContains(w.Body.String(), `"supported_actions":null`)
}

func TestOperationStatusDocumentExtractionConfigurationUsesDurableSelectedScope(t *testing.T) {
	tests := []struct {
		name             string
		documentsEnabled bool
		scopeErr         error
		wantConfigured   bool
		wantScopeCalls   int
	}{
		{
			name: "durably selected", documentsEnabled: true,
			wantConfigured: true, wantScopeCalls: 1,
		},
		{
			name: "missing durable target", documentsEnabled: true,
			scopeErr: store.ErrDocumentIndexStatusScopeUnavailable, wantScopeCalls: 1,
		},
		{
			name: "generic durable scope failure remains retryable", documentsEnabled: true,
			scopeErr: errors.New("corrupt durable scope record"), wantConfigured: true, wantScopeCalls: 1,
		},
		{
			name: "disabled document configuration", scopeErr: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			server, catalog := newTestServerWithMockStore(t)
			server.cfg.Attachments.Documents.Enabled = test.documentsEnabled
			calls := 0
			catalog.documentCurrentScopeFunc = func(context.Context) (string, []string, error) {
				calls++
				return "private-selected-document-profile", []string{"application/pdf"}, test.scopeErr
			}

			response := doGet(server, "/api/v1/operations/status")
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			var body OperationStatusResponse
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			var document *OperationLaneStatus
			for index := range body.Lanes {
				if body.Lanes[index].Kind == operations.KindDocumentExtraction {
					document = &body.Lanes[index]
					break
				}
			}
			requirements.NotNil(document)
			assertions.Equal(test.wantConfigured, document.Configured)
			assertions.Equal(test.wantScopeCalls, calls)
			assertions.NotContains(response.Body.String(), "private-selected-document-profile")
		})
	}
}

func TestOperationStatusPersonEnrichmentConfiguredRequiresEnabledProviderRuntime(t *testing.T) {
	tests := []struct {
		name             string
		enabled          bool
		providerEnabled  bool
		runtimeScheduled bool
		want             bool
	}{
		{
			name: "configured", enabled: true, providerEnabled: true,
			runtimeScheduled: true, want: true,
		},
		{
			name: "globally disabled", providerEnabled: true,
			runtimeScheduled: true,
		},
		{
			name: "provider disabled", enabled: true,
			runtimeScheduled: true,
		},
		{
			name: "runtime unavailable", enabled: true, providerEnabled: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cfg := config.NewDefaultConfig()
			cfg.People.Enrichment.Enabled = test.enabled
			cfg.People.Enrichment.Providers = []personenrichment.ProviderConfig{{
				Name: "private-provider-name", Kind: personenrichment.ProviderExa,
				Enabled: test.providerEnabled,
			}}
			scheduler := newMockScheduler()
			scheduler.scheduledJobs = map[string]bool{"person-enrichment": test.runtimeScheduled}
			srv := NewServerWithOptions(ServerOptions{
				Config: cfg, Store: &mockStore{}, Scheduler: scheduler,
				OperationHistoryReader: &operationHistoryStub{}, Logger: testLogger(),
			})

			w := doGet(srv, "/api/v1/operations/status")
			require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())
			var body OperationStatusResponse
			require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
			require.Len(body.Lanes, 9)
			assert.Equal(test.want, body.Lanes[5].Configured)
			assert.NotContains(w.Body.String(), "private-provider-name")
			assert.NotContains(w.Body.String(), personenrichment.ProviderExa)
		})
	}
}

func TestOperationStatusProjectsRealStoreRunsAndDegradesOnlyOneLane(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("fixture", "private-source-identifier")
	require.NoError(err)
	started := time.Date(2026, 8, 29, 15, 0, 0, 0, time.UTC)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO sync_runs (
		source_id, started_at, completed_at, status, messages_processed,
		messages_added, messages_updated, errors_count, error_message
	) VALUES (?, ?, ?, 'completed', 2, 1, 0, 0, ?)`), source.ID,
		operationAPITimestamp(st, started, false),
		operationAPITimestamp(st, started.Add(time.Second), false), "private-ledger-error")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO sync_runs (
		source_id, started_at, status, messages_processed, messages_added,
		messages_updated, errors_count
	) VALUES (?, ?, 'running', 0, 0, 0, 0)`), source.ID,
		operationAPITimestamp(st, started.Add(2*time.Second), false))
	require.NoError(err)

	srv := NewServerWithOptions(ServerOptions{
		Config: config.NewDefaultConfig(), Store: st, OperationHistoryReader: st, Logger: testLogger(),
	})
	w := doGet(srv, "/api/v1/operations/status")
	require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var body OperationStatusResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	sourceLane := body.Lanes[7]
	assert.True(sourceLane.Configured)
	assert.Equal(operations.HistoryAvailable, sourceLane.HistoryAvailability)
	require.NotNil(sourceLane.Active)
	require.NotNil(sourceLane.Latest)
	require.NotNil(sourceLane.LatestSuccessful)
	assert.Equal(operations.StateRunning, sourceLane.Active.State)
	assert.Equal(sourceLane.Active.ID, sourceLane.Latest.ID)
	assert.Equal(operations.StateSucceeded, sourceLane.LatestSuccessful.State)
	assert.NotContains(w.Body.String(), "private-source-identifier")
	assert.NotContains(w.Body.String(), "private-ledger-error")

	reader := &operationHistoryStub{statusErr: map[operations.Kind]error{
		operations.KindPersonSweep: errors.New("private person status failure"),
	}}
	degraded := NewServerWithOptions(ServerOptions{
		Config: config.NewDefaultConfig(), Store: st, OperationHistoryReader: reader, Logger: testLogger(),
	})
	w = doGet(degraded, "/api/v1/operations/status")
	require.Equal(http.StatusOK, w.Code)
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(operations.HistoryAvailable, body.Lanes[0].HistoryAvailability)
	assert.Equal(operations.HistoryUnavailable, body.Lanes[6].HistoryAvailability)
	assert.Equal("person_sweep_history_unavailable", body.Lanes[6].UnavailableCode)
	assert.Equal(operations.HistoryAvailable, body.Lanes[7].HistoryAvailability)
	assert.NotContains(w.Body.String(), "private person status failure")
}

func TestOperationStatusAdvertisesOnlySafeTypedActions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg, st, _ := savedCardDAVFixture(t)
	controller, err := NewCardDAVController(cfg, st, slog.New(slog.DiscardHandler))
	require.NoError(err)
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg, Store: st, CardDAV: controller, OperationHistoryReader: st, Logger: testLogger(),
	})
	srv.SetVisualOperations(func(context.Context, operations.PassScope) error { return nil }, func(context.Context, operations.PassScope) error { return nil },
		nil, func(context.Context, bool) (visual.Status, error) {
			return visual.Status{
				Generation: store.VisualGeneration{
					ID: 987654321, State: store.VisualGenerationBuilding,
					Fingerprint: "private-visual-fingerprint", Model: "private-visual-model",
				},
				Formats: []visual.FormatCoverage{{MIMEType: "private/visual-format", Eligible: 123456789}},
			}, nil
		}, nil)

	w := doGet(srv, "/api/v1/operations/status")
	require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var body OperationStatusResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal([]operations.ActionID{operations.ActionCardDAVSync}, body.Lanes[0].SupportedActions)
	assert.Equal([]operations.ActionID{operations.ActionVisualBuild}, body.Lanes[8].SupportedActions)
	for _, marker := range []string{
		"987654321", "private-visual-fingerprint", "private-visual-model",
		"private/visual-format", "123456789", "formats", "generation",
	} {
		assert.NotContains(w.Body.String(), marker)
	}
	srv.SetVisualOperations(func(context.Context, operations.PassScope) error { return nil }, func(context.Context, operations.PassScope) error { return nil },
		nil, func(context.Context, bool) (visual.Status, error) {
			return visual.Status{Generation: store.VisualGeneration{
				State: store.VisualGenerationBuilding, Consented: true,
			}}, nil
		}, nil)
	w = doGet(srv, "/api/v1/operations/status")
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal([]operations.ActionID{operations.ActionVisualResume}, body.Lanes[8].SupportedActions)
	controller.mu.Lock()
	controller.cfg.CardDAV.Username = "mismatched-user"
	controller.mu.Unlock()
	w = doGet(srv, "/api/v1/operations/status")
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	assert.False(body.Lanes[0].Configured)
	assert.Empty(body.Lanes[0].SupportedActions)
	controller.mu.Lock()
	controller.cfg.CardDAV.Username = "old-user"
	controller.mu.Unlock()

	srv.SetVisualOperations(func(context.Context, operations.PassScope) error { return nil }, func(context.Context, operations.PassScope) error { return nil },
		nil, func(context.Context, bool) (visual.Status, error) {
			return visual.Status{
				Generation:             store.VisualGeneration{State: store.VisualGenerationActive},
				ReconciliationComplete: true, JournalLag: 1,
			}, nil
		}, nil)
	w = doGet(srv, "/api/v1/operations/status")
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal([]operations.ActionID{operations.ActionVisualResume}, body.Lanes[8].SupportedActions)

	srv.SetVisualOperations(func(context.Context, operations.PassScope) error { return nil }, func(context.Context, operations.PassScope) error { return nil },
		nil, func(context.Context, bool) (visual.Status, error) {
			return visual.Status{
				Generation:             store.VisualGeneration{State: store.VisualGenerationActive},
				ReconciliationComplete: true, Converged: 2, ConvergenceTotal: 2,
			}, nil
		}, nil)
	w = doGet(srv, "/api/v1/operations/status")
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	assert.Empty(body.Lanes[8].SupportedActions)

	srv.SetVisualOperations(func(context.Context, operations.PassScope) error { return nil }, func(context.Context, operations.PassScope) error { return nil },
		nil, func(context.Context, bool) (visual.Status, error) {
			return visual.Status{}, errors.New("private visual provider failure")
		}, nil)
	w = doGet(srv, "/api/v1/operations/status")
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	assert.True(body.Lanes[8].Configured)
	assert.Empty(body.Lanes[8].SupportedActions)
	assert.NotContains(w.Body.String(), "private visual provider failure")

	activeCardDAV := operationRunForKindFixture(t, operations.KindCardDAVSync, operations.StateRunning)
	reader := &operationHistoryStub{status: map[operations.Kind]operations.LaneHistoryStatus{
		operations.KindCardDAVSync: {
			Kind: operations.KindCardDAVSync, Lane: operations.LaneContacts,
			HistoryAvailability: operations.HistoryAvailable, Active: &activeCardDAV,
			Latest: &activeCardDAV,
		},
	}}
	srv.operationHistoryReader = reader
	w = doGet(srv, "/api/v1/operations/status")
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	assert.Empty(body.Lanes[0].SupportedActions, "active CardDAV sync is not eligible for another sync")

	for _, lane := range body.Lanes[1:8] {
		assert.Empty(lane.SupportedActions, "lane %s", lane.Kind)
	}
	for _, forbidden := range []string{
		`"method"`, `"path"`, `"url"`, `"args"`, "/cli/run", "cancel",
		"source_sync_action", "people", "document_build", "visual_retry",
		"scheduler-account", "scheduler-job", "private-holder-label",
	} {
		assert.NotContains(strings.ToLower(w.Body.String()), forbidden)
	}
}

func TestOperationStatusBypassesGateAndHandlesNilDependencies(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	gate := NewSerialOperationGate()
	release, acquired := gate.BeginLabeledWorkContext(t.Context(), "private-holder-label")
	require.True(acquired)
	defer release()
	srv := NewServerWithOptions(ServerOptions{
		Config: config.NewDefaultConfig(), Store: &mockStore{}, OperationGate: gate, Logger: testLogger(),
	})

	w := doGet(srv, "/api/v1/operations/status")
	require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var body OperationStatusResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(body.Lanes, 9)
	for _, index := range []int{0, 6, 7} {
		assert.Equal(operations.HistoryUnavailable, body.Lanes[index].HistoryAvailability)
		assert.Empty(body.Lanes[index].SupportedActions)
	}
	assert.NotContains(w.Body.String(), "private-holder-label")
	assert.False(body.Lanes[5].Configured)
	assert.Nil(body.Lanes[5].Active)
	assert.Nil(body.Lanes[5].Latest)
	assert.Nil(body.Lanes[5].LatestSuccessful)
}

func TestOperationRunsPaginatesAndDeclaresUnavailableKinds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	newest := operationRunFixture(t)
	older := operationRunFixture(t)
	older.ID = mustOperationIntID(t, operations.KindSourceSync, 16)
	older.StartedAt = newest.StartedAt.Add(-time.Hour)
	position := operations.Position{StartedAt: newest.StartedAt, ID: newest.ID}
	reader := &operationHistoryStub{snapshots: []operations.HistorySnapshot{
		{
			Runs: []operations.Run{newest, older}, Position: &position,
			AvailableKinds:     []operations.Kind{operations.KindSourceSync},
			UnavailableKinds:   []operations.Kind{operations.KindMessageEmbedding},
			MembershipRevision: 7,
		},
		{
			Runs: []operations.Run{older}, AvailableKinds: []operations.Kind{operations.KindSourceSync},
			UnavailableKinds:   []operations.Kind{operations.KindMessageEmbedding},
			MembershipRevision: 7,
		},
	}}
	srv := newOperationTestServer(reader, &operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID})

	w := doGet(srv, "/api/v1/operations/runs?limit=1")
	require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var body OperationRunsResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(body.Runs, 1)
	assert.NotEmpty(body.NextCursor)
	assert.True(strings.HasPrefix(body.NextCursor, "op2."))
	assert.Equal([]OperationUnavailableKind{
		{Kind: operations.KindMessageEmbedding, Lane: operations.LaneMessages, UnavailableCode: "message_embedding_history_unavailable"},
	}, body.UnavailableKinds)
	assert.Equal(int64(7), body.MembershipRevision)
	assert.Equal(1, reader.queries[0].Limit)
	assert.Equal(operations.KindSourceSync, body.Runs[0].Kind)
	assert.NotNil(body.Runs[0].Counters)
	assert.NotContains(w.Body.String(), operationTestArchiveUID)

	w = doGet(srv, "/api/v1/operations/runs?limit=1&cursor="+body.NextCursor)
	require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var next OperationRunsResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &next))
	require.Len(next.Runs, 1)
	assert.Empty(next.NextCursor)
	require.NotNil(reader.queries[1].Position)
	assert.Equal(position, *reader.queries[1].Position)
	assert.Equal(body.UnavailableKinds, next.UnavailableKinds)
	assert.Equal(body.MembershipRevision, next.MembershipRevision)
}

func TestOperationRunsPaginatesSQLiteEnrichmentAtNanosecondPrecision(t *testing.T) {
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	startedAt := time.Date(2026, 9, 5, 12, 0, 0, 123456789, time.UTC)
	var ids []int64
	for _, seed := range []struct {
		key string
		at  time.Time
	}{
		{"first", startedAt},
		{"newest", startedAt.Add(time.Nanosecond)},
		{"tied", startedAt},
	} {
		run, created, err := st.StartRun(t.Context(), personenrichment.RunStart{
			Kind: "manual", RequestedBy: seed.key, RequestedAt: seed.at,
		})
		require.NoError(err)
		require.True(created)
		ids = append(ids, run.ID)
	}
	archive := &operationArchiveRealStore{Store: st, uid: operationTestArchiveUID}
	srv := newOperationTestServer(st, archive)
	codec := newOperationTokenCodec(st)
	cursor := ""
	// Timestamp order wins over ID order; equal timestamps use descending IDs.
	for index, wantID := range []int64{ids[1], ids[2], ids[0]} {
		target := "/api/v1/operations/runs?kind=person_enrichment&limit=1"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		response := doGet(srv, target)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		var page OperationRunsResponse
		require.NoError(json.Unmarshal(response.Body.Bytes(), &page))
		require.Len(page.Runs, 1)
		id, err := codec.decodeRunReference(t.Context(), page.Runs[0].ID, operationTestArchiveUID)
		require.NoError(err)
		require.Equal(mustOperationIntID(t, operations.KindPersonEnrichment, wantID), id)
		if index < len(ids)-1 {
			require.NotEmpty(page.NextCursor)
		} else {
			require.Empty(page.NextCursor, "pagination must end after exposing every run once")
		}
		cursor = page.NextCursor
	}
}

func TestOperationRunsRejectsInvalidQueriesAndUnavailableKinds(t *testing.T) {
	srv := newOperationTestServer(&operationHistoryStub{}, &operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID})
	tests := []struct {
		name   string
		target string
		status int
		code   string
	}{
		{name: "zero limit", target: "/api/v1/operations/runs?limit=0", status: 400, code: "invalid_limit"},
		{name: "over max", target: "/api/v1/operations/runs?limit=101", status: 400, code: "invalid_limit"},
		{name: "duplicate", target: "/api/v1/operations/runs?kind=source_sync&kind=source_sync", status: 400, code: "invalid_kind"},
		{name: "unknown parameter", target: "/api/v1/operations/runs?provider=private", status: 400, code: "invalid_query"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := doGet(srv, test.target)
			assert.Equal(t, test.status, w.Code)
			assert.Equal(t, test.code, decodeErrorEnvelope(t, w).Error)
		})
	}
}

func TestOperationRunsRejectsSingleDynamicallyUnavailableKind(t *testing.T) {
	reader := &operationHistoryStub{snapshots: []operations.HistorySnapshot{{
		AvailableKinds: []operations.Kind{}, UnavailableKinds: []operations.Kind{operations.KindMessageEmbedding},
		MembershipRevision: 4,
	}}}
	srv := newOperationTestServer(reader,
		&operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID})
	w := doGet(srv, "/api/v1/operations/runs?kind=message_embedding")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "operation_history_unavailable", decodeErrorEnvelope(t, w).Error)
}

func TestOperationRunsRejectsCursorSnapshotDrift(t *testing.T) {
	newest := operationRunFixture(t)
	older := operationRunFixture(t)
	older.ID = mustOperationIntID(t, operations.KindSourceSync, 16)
	older.StartedAt = newest.StartedAt.Add(-time.Hour)
	position := operations.Position{StartedAt: newest.StartedAt, ID: newest.ID}
	first := operations.HistorySnapshot{
		Runs: []operations.Run{newest, older}, Position: &position,
		AvailableKinds:     []operations.Kind{operations.KindSourceSync},
		UnavailableKinds:   []operations.Kind{operations.KindMessageEmbedding},
		MembershipRevision: 7,
	}
	tests := []struct {
		name string
		next operations.HistorySnapshot
	}{
		{name: "revision", next: operations.HistorySnapshot{
			Runs: []operations.Run{older}, AvailableKinds: []operations.Kind{operations.KindSourceSync},
			UnavailableKinds: []operations.Kind{operations.KindMessageEmbedding}, MembershipRevision: 8,
		}},
		{name: "available kinds", next: operations.HistorySnapshot{
			Runs:             []operations.Run{older},
			AvailableKinds:   []operations.Kind{operations.KindCardDAVSync, operations.KindSourceSync},
			UnavailableKinds: []operations.Kind{operations.KindMessageEmbedding}, MembershipRevision: 7,
		}},
		{name: "unavailable kinds", next: operations.HistorySnapshot{
			Runs: []operations.Run{older}, AvailableKinds: []operations.Kind{operations.KindSourceSync},
			UnavailableKinds: []operations.Kind{operations.KindDocumentEmbedding}, MembershipRevision: 7,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			reader := &operationHistoryStub{snapshots: []operations.HistorySnapshot{first, test.next}}
			srv := newOperationTestServer(reader,
				&operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID})
			pageOne := doGet(srv, "/api/v1/operations/runs?limit=1")
			require.Equalf(http.StatusOK, pageOne.Code, "body: %s", pageOne.Body.String())
			var body OperationRunsResponse
			require.NoError(json.Unmarshal(pageOne.Body.Bytes(), &body))
			require.NotEmpty(body.NextCursor)

			pageTwo := doGet(srv, "/api/v1/operations/runs?limit=1&cursor="+body.NextCursor)
			assert.Equal(http.StatusConflict, pageTwo.Code)
			assert.Equal("operation_history_conflict", decodeErrorEnvelope(t, pageTwo).Error)
		})
	}

	t.Run("requested kind becomes unavailable", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		firstForKind := first
		firstForKind.UnavailableKinds = []operations.Kind{}
		secondForKind := operations.HistorySnapshot{
			AvailableKinds:     []operations.Kind{},
			UnavailableKinds:   []operations.Kind{operations.KindSourceSync},
			MembershipRevision: 7,
		}
		reader := &operationHistoryStub{snapshots: []operations.HistorySnapshot{firstForKind, secondForKind}}
		srv := newOperationTestServer(reader,
			&operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID})
		pageOne := doGet(srv, "/api/v1/operations/runs?kind=source_sync&limit=1")
		require.Equalf(http.StatusOK, pageOne.Code, "body: %s", pageOne.Body.String())
		var body OperationRunsResponse
		require.NoError(json.Unmarshal(pageOne.Body.Bytes(), &body))

		pageTwo := doGet(srv, "/api/v1/operations/runs?kind=source_sync&limit=1&cursor="+body.NextCursor)
		assert.Equal(http.StatusConflict, pageTwo.Code)
		assert.Equal("operation_history_conflict", decodeErrorEnvelope(t, pageTwo).Error)
	})
}

func TestOperationRunsRejectsCursorDateFilterDrift(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	newest := operationRunFixture(t)
	older := operationRunFixture(t)
	older.ID = mustOperationIntID(t, operations.KindSourceSync, 16)
	older.StartedAt = newest.StartedAt.Add(-time.Hour)
	position := operations.Position{StartedAt: newest.StartedAt, ID: newest.ID}
	reader := &operationHistoryStub{snapshots: []operations.HistorySnapshot{{
		Runs: []operations.Run{newest, older}, Position: &position,
		AvailableKinds: []operations.Kind{operations.KindSourceSync}, MembershipRevision: 7,
	}}}
	srv := newOperationTestServer(reader,
		&operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID})
	pageOne := doGet(srv, "/api/v1/operations/runs?kind=source_sync&limit=1&started_from=2026-08-01T00%3A00%3A00Z")
	require.Equalf(http.StatusOK, pageOne.Code, "body: %s", pageOne.Body.String())
	var body OperationRunsResponse
	require.NoError(json.Unmarshal(pageOne.Body.Bytes(), &body))

	pageTwo := doGet(srv, "/api/v1/operations/runs?kind=source_sync&limit=1&started_from=2026-08-02T00%3A00%3A00Z&cursor="+body.NextCursor)
	assert.Equal(http.StatusBadRequest, pageTwo.Code)
	assert.Equal("invalid_cursor", decodeErrorEnvelope(t, pageTwo).Error)
}

func TestOperationRunsRejectsBoundCursorAndFailsAtomically(t *testing.T) {
	assert := assert.New(t)
	run := operationRunFixture(t)
	position := operations.Position{StartedAt: run.StartedAt, ID: run.ID}
	archive := &operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID}
	cursor, err := newOperationTokenCodec(archive).encodeCursor(t.Context(), operationCursorBinding{
		Position: position, MembershipRevision: 1,
		AvailableKinds: []operations.Kind{operations.KindSourceSync}, UnavailableKinds: []operations.Kind{},
	}, operationHistoryFilter{}, operationTestArchiveUID)
	require.NoError(t, err)

	reader := &operationHistoryStub{runs: []operations.Run{run}, listErr: errors.New("synthetic read failed")}
	srv := newOperationTestServer(reader, archive)
	w := doGet(srv, "/api/v1/operations/runs?cursor="+cursor+"&kind=source_sync")
	assert.Equal(http.StatusBadRequest, w.Code)
	assert.Equal("invalid_cursor", decodeErrorEnvelope(t, w).Error)

	w = doGet(srv, "/api/v1/operations/runs")
	assert.Equal(http.StatusInternalServerError, w.Code)
	assert.Equal("operation_history_failed", decodeErrorEnvelope(t, w).Error)
	assert.NotContains(w.Body.String(), "runs")
	assert.NotContains(w.Body.String(), "next_cursor")
}

func TestOperationRunDetailUsesOpaqueIdentityAndExactErrors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	run := operationRunFixture(t)
	reader := &operationHistoryStub{run: run}
	archive := &operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID}
	ref, err := newOperationTokenCodec(archive).encodeRunReference(t.Context(), run.ID, operationTestArchiveUID)
	require.NoError(err)
	srv := newOperationTestServer(reader, archive)
	w := doGet(srv, "/api/v1/operations/runs/"+ref)
	require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var detail OperationRunDetail
	require.NoError(json.Unmarshal(w.Body.Bytes(), &detail))
	assert.Equal(ref, detail.ID)
	assert.Equal(operations.KindSourceSync, detail.Kind)

	w = doGet(srv, "/api/v1/operations/runs/not-a-reference")
	assert.Equal(http.StatusBadRequest, w.Code)
	assert.Equal("invalid_operation_run_id", decodeErrorEnvelope(t, w).Error)

	reader.getErr = store.ErrOperationRunNotFound
	w = doGet(srv, "/api/v1/operations/runs/"+ref)
	assert.Equal(http.StatusNotFound, w.Code)
	assert.Equal("operation_run_not_found", decodeErrorEnvelope(t, w).Error)

	reader.getErr = errors.New("private ordinary reader failure")
	w = doGet(srv, "/api/v1/operations/runs/"+ref)
	assert.Equal(http.StatusInternalServerError, w.Code)
	assert.Equal("operation_history_failed", decodeErrorEnvelope(t, w).Error)
	assert.NotContains(w.Body.String(), "private ordinary reader failure")

	reader.getErr = nil
	crossArchive := newOperationTestServer(reader, &operationArchiveStore{
		mockStore: &mockStore{}, uid: "abcdef1234567890abcdef1234567890",
	})
	w = doGet(crossArchive, "/api/v1/operations/runs/"+ref)
	assert.Equal(http.StatusBadRequest, w.Code)
	assert.Equal("invalid_operation_run_id", decodeErrorEnvelope(t, w).Error)

	badPair := "1." + base64.RawURLEncoding.EncodeToString([]byte(`{"kind":"person_sweep","id_type":"int64","int_id":17,"archive_uid":"`+operationTestArchiveUID+`"}`))
	w = doGet(srv, "/api/v1/operations/runs/"+badPair)
	assert.Equal(http.StatusBadRequest, w.Code)
	assert.Equal("invalid_operation_run_id", decodeErrorEnvelope(t, w).Error)
}

func TestOperationRunDetailAddsOnlyRegistryStatusAndEligibleActions(t *testing.T) {
	t.Run("CardDAV", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		cfg, st, _ := savedCardDAVFixture(t)
		controller, err := NewCardDAVController(cfg, st, slog.New(slog.DiscardHandler))
		require.NoError(err)
		run := operationRunForKindFixture(t, operations.KindCardDAVSync, operations.StateSucceeded)
		reader := &operationHistoryStub{run: run}
		srv := NewServerWithOptions(ServerOptions{
			Config: cfg, Store: st, CardDAV: controller,
			OperationHistoryReader: reader, Logger: testLogger(),
		})
		archiveUID, err := st.ArchiveUIDContext(t.Context())
		require.NoError(err)
		ref, err := newOperationTokenCodec(st).encodeRunReference(t.Context(), run.ID, archiveUID)
		require.NoError(err)

		response := doGet(srv, "/api/v1/operations/runs/"+ref)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		var detail OperationRunDetail
		require.NoError(json.Unmarshal(response.Body.Bytes(), &detail))
		require.NotNil(detail.RelatedStatus)
		assert.Equal(operations.RelatedStatusCardDAV, *detail.RelatedStatus)
		assert.Equal([]operations.ActionID{operations.ActionCardDAVSync}, detail.SupportedActions)

		active := operationRunForKindFixture(t, operations.KindCardDAVSync, operations.StateRunning)
		reader.status = map[operations.Kind]operations.LaneHistoryStatus{
			operations.KindCardDAVSync: {
				Kind: operations.KindCardDAVSync, Lane: operations.LaneContacts,
				HistoryAvailability: operations.HistoryAvailable, Active: &active, Latest: &active,
			},
		}
		response = doGet(srv, "/api/v1/operations/runs/"+ref)
		require.NoError(json.Unmarshal(response.Body.Bytes(), &detail))
		assert.Empty(detail.SupportedActions)
	})

	t.Run("visual", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		run := operationRunForKindFixture(t, operations.KindVisualEmbedding, operations.StateSucceeded)
		reader := &operationHistoryStub{run: run}
		srv := NewServerWithOptions(ServerOptions{
			Config: config.NewDefaultConfig(), Store: st,
			OperationHistoryReader: reader, Logger: testLogger(),
		})
		srv.SetVisualOperations(
			func(context.Context, operations.PassScope) error { return nil },
			func(context.Context, operations.PassScope) error { return nil }, nil,
			func(context.Context, bool) (visual.Status, error) {
				return visual.Status{Generation: store.VisualGeneration{
					State: store.VisualGenerationBuilding,
				}}, nil
			}, nil,
		)
		archiveUID, err := st.ArchiveUIDContext(t.Context())
		require.NoError(err)
		ref, err := newOperationTokenCodec(st).encodeRunReference(t.Context(), run.ID, archiveUID)
		require.NoError(err)

		response := doGet(srv, "/api/v1/operations/runs/"+ref)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		var detail OperationRunDetail
		require.NoError(json.Unmarshal(response.Body.Bytes(), &detail))
		require.NotNil(detail.RelatedStatus)
		assert.Equal(operations.RelatedStatusVisual, *detail.RelatedStatus)
		assert.Equal([]operations.ActionID{operations.ActionVisualBuild}, detail.SupportedActions)
		assert.NotContains(response.Body.String(), "generation")
	})
}

func TestOperationActionsKeepExistingMutationBoundaries(t *testing.T) {
	assert := assert.New(t)
	const apiKey = "synthetic-operations-api-key"
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: apiKey}},
		Store:  &mockStore{}, Logger: testLogger(),
	})
	for _, target := range []string{
		"/api/v1/carddav/sync",
		"/api/v1/multimodal/build",
		"/api/v1/multimodal/run",
	} {
		request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		assert.Equal(http.StatusUnauthorized, response.Code, target)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/multimodal/build", strings.NewReader(`{}`))
	request.Header.Set("X-Api-Key", apiKey)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	assert.Equal(http.StatusBadRequest, response.Code)
	assert.Equal("visual_consent_required", decodeErrorEnvelope(t, response).Error)

	for _, target := range []string{
		"/api/v1/operations/runs/opaque/retry",
		"/api/v1/operations/document_embedding",
	} {
		request = httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
		request.Header.Set("X-Api-Key", apiKey)
		response = httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		assert.Equal(http.StatusNotFound, response.Code, target)
	}

	sessionServer := newSessionTestServer(t, apiKey)
	login := performSessionRequest(t, sessionServer, http.MethodPost, sessionLoginPath,
		[]byte(`{"api_key":"`+apiKey+`"}`), nil, false)
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	sessionStatus := decodeSessionStatus(t, login)
	cookie := requireSessionCookie(t, login)
	for _, target := range []string{
		"/api/v1/carddav/sync",
		"/api/v1/multimodal/build",
		"/api/v1/multimodal/run",
	} {
		response = performSessionRequest(t, sessionServer, http.MethodPost, target, []byte(`{}`), http.Header{
			"Cookie":       []string{cookie.String()},
			"Origin":       []string{"http://example.com"},
			csrfHeaderName: []string{sessionStatus.CSRFToken + "-wrong"},
		}, false)
		assert.Equal(http.StatusForbidden, response.Code, target)
	}
}

func TestOperationHistoryAPIBypassesHeldOperationGate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	run := operationRunFixture(t)
	reader := &operationHistoryStub{runs: []operations.Run{run}, run: run}
	gate := NewSerialOperationGate()
	release, acquired := gate.BeginLabeledWorkContext(t.Context(), "private-holder-label")
	require.True(acquired)
	defer release()
	var logs bytes.Buffer
	srv := NewServerWithOptions(ServerOptions{
		Config:                 &config.Config{},
		Store:                  &operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID},
		OperationHistoryReader: reader,
		OperationGate:          gate,
		Logger:                 slog.New(slog.NewTextHandler(&logs, nil)),
	})

	list := doGet(srv, "/api/v1/operations/runs")
	require.Equalf(http.StatusOK, list.Code, "body: %s", list.Body.String())
	keyring, ok := srv.store.(operationTokenKeyring)
	require.True(ok)
	ref, err := newOperationTokenCodec(keyring).encodeRunReference(
		t.Context(), run.ID, operationTestArchiveUID)
	require.NoError(err)
	detail := doGet(srv, "/api/v1/operations/runs/"+ref)
	require.Equalf(http.StatusOK, detail.Code, "body: %s", detail.Body.String())
	assert.NotContains(list.Body.String()+detail.Body.String(), "private-holder-label")
	assert.NotContains(logs.String(), "private-holder-label")
}

func TestOperationHistoryAPIDependencyFailures(t *testing.T) {
	tests := []struct {
		name    string
		reader  operations.HistoryReader
		archive ArchiveIdentifier
		target  string
		status  int
		code    string
	}{
		{name: "nil reader", archive: &operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID}, target: "/api/v1/operations/runs", status: 503, code: "operation_history_unavailable"},
		{name: "nil reader detail", archive: &operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID}, target: "/api/v1/operations/runs/opaque", status: 503, code: "operation_history_unavailable"},
		{name: "nil archive", reader: &operationHistoryStub{}, target: "/api/v1/operations/runs", status: 503, code: "operation_history_unavailable"},
		{name: "nil archive detail", reader: &operationHistoryStub{}, target: "/api/v1/operations/runs/opaque", status: 503, code: "operation_history_unavailable"},
		{name: "archive failure", reader: &operationHistoryStub{}, archive: &operationArchiveStore{mockStore: &mockStore{}, err: errors.New("private archive failure")}, target: "/api/v1/operations/runs", status: 503, code: "operation_history_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newOperationTestServer(test.reader, test.archive)
			w := doGet(srv, test.target)
			assert.Equal(t, test.status, w.Code)
			env := decodeErrorEnvelope(t, w)
			assert.Equal(t, test.code, env.Error)
			assert.NotContains(t, strings.ToLower(w.Body.String()), "private")
		})
	}
}

func TestOperationHistoryAPIRealStoreSameSecondWalkAndPrivacy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	sentinels := loadBrowserOperationPrivateSentinels(t)
	st := testutil.NewTestStore(t)
	started := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	source, err := st.GetOrCreateSource("gmail", sentinels.Address)
	require.NoError(err)
	identityOverride := ""
	if st.IsPostgreSQL() {
		identityOverride = " OVERRIDING SYSTEM VALUE"
	}
	var sourceRunID int64
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO sync_runs (
		id, source_id, started_at, completed_at, status, messages_processed, messages_added,
		messages_updated, errors_count, error_message, cursor_before, cursor_after
	)`+identityOverride+` VALUES (?, ?, ?, ?, 'completed', 7, 2, 1, 0, ?, ?, ?) RETURNING id`),
		sentinels.numericDatabaseID(t), source.ID,
		operationAPITimestamp(st, started, false), operationAPITimestamp(st, started.Add(time.Second), false),
		sentinels.RawError, sentinels.Credential, sentinels.Endpoint).Scan(&sourceRunID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO sync_run_items (
		sync_run_id, source_message_id, phase, status, error_kind, error_message
	) VALUES (?, ?, 'fetch', 'error', 'private-source-item-kind', ?)`),
		sourceRunID, sentinels.Filename, "private-source-item-error")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_sweep_runs (
		id, kind, mode, status, program_fingerprint, catalog_fingerprint,
		provider_fingerprint, attempt_count, success_count, failure_count,
		projected_write_count, started_at, completed_at
	) VALUES ('person-run', 'manual', 'incremental', 'succeeded', ?, ?, ?, 2, 2, 0, 1, ?, ?)`),
		sentinels.GenerationFingerprint, "private-person-catalog", sentinels.Provider,
		operationAPITimestamp(st, started, true), operationAPITimestamp(st, started.Add(time.Second), true))
	require.NoError(err)
	var personID int64
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO persons (
		vcard_uid, display_name
	) VALUES (?, ?) RETURNING id`), "private-person-uid", sentinels.Name).Scan(&personID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_sweep_attempts (
		id, run_id, person_id, lease_fence, mode, status, failure_class,
		cursor_envelope_json, envelope_hash, program_fingerprint, catalog_fingerprint,
		provider_fingerprint, generation_key, provider_request_id, input_tokens,
		output_tokens, estimated_cost_micro_usd, started_at, completed_at
	) VALUES (?, 'person-run', ?, 1, 'incremental', 'succeeded', '', ?, ?, ?, ?, ?, ?, ?, 987654321, 876543210, 765432109, ?, ?)`),
		"private-person-attempt-id", personID, `{"endpoint":"`+sentinels.Endpoint+`"}`,
		sentinels.GenerationFingerprint, "private-person-attempt-program",
		"private-person-attempt-catalog", sentinels.Provider,
		sentinels.Model, sentinels.Credential,
		operationAPITimestamp(st, started, true), operationAPITimestamp(st, started.Add(time.Second), true))
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_sync_runs (
		trigger, state, started_at, finished_at, books, created, updated, removed, error_code, error_message
	) VALUES ('manual', 'failed', ?, ?, 1, 2, 3, 4, 'sync_failed', ?)`),
		operationAPITimestamp(st, started, false), operationAPITimestamp(st, started.Add(time.Second), false), sentinels.RawError)
	require.NoError(err)

	archiveStore := &operationArchiveRealStore{Store: st, uid: sentinels.ArchiveUID}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{}, Store: archiveStore, OperationHistoryReader: st, Logger: testLogger(),
	})
	privateMarkers := append(sentinels.values(),
		"private-source-item-kind", "private-source-item-error", "private-person-catalog",
		"private-person-uid", "private-person-attempt-id", "private-person-attempt-program",
		"private-person-attempt-catalog", "987654321", "876543210", "765432109",
	)
	var summaries []OperationRunSummary
	cursor := ""
	firstCursor := ""
	for {
		target := "/api/v1/operations/runs?limit=1"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		w := doGet(srv, target)
		require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())
		for _, marker := range privateMarkers {
			assert.NotContains(w.Body.String(), marker)
		}
		var page OperationRunsResponse
		require.NoError(json.Unmarshal(w.Body.Bytes(), &page))
		summaries = append(summaries, page.Runs...)
		cursor = page.NextCursor
		if firstCursor == "" {
			firstCursor = cursor
		}
		if cursor == "" {
			break
		}
	}
	require.Len(summaries, 3)
	assert.Equal([]operations.Kind{
		operations.KindCardDAVSync, operations.KindPersonSweep, operations.KindSourceSync,
	}, []operations.Kind{summaries[0].Kind, summaries[1].Kind, summaries[2].Kind})
	for _, summary := range summaries {
		w := doGet(srv, "/api/v1/operations/runs/"+summary.ID)
		require.Equal(http.StatusOK, w.Code)
		for _, marker := range privateMarkers {
			assert.NotContains(w.Body.String(), marker)
		}
		var detail OperationRunDetail
		require.NoError(json.Unmarshal(w.Body.Bytes(), &detail))
		assert.Equal(summary, detail.OperationRunSummary)
	}

	for _, test := range []struct {
		query       string
		kind        operations.Kind
		unavailable []OperationUnavailableKind
	}{
		{query: "kind=source_sync&state=succeeded", kind: operations.KindSourceSync, unavailable: []OperationUnavailableKind{}},
		{query: "lane=contacts&state=failed", kind: operations.KindCardDAVSync, unavailable: []OperationUnavailableKind{}},
		{
			query: "lane=messages", kind: operations.KindSourceSync,
			unavailable: []OperationUnavailableKind{},
		},
		{
			query: "lane=person_facts", kind: operations.KindPersonSweep,
			unavailable: []OperationUnavailableKind{},
		},
	} {
		w := doGet(srv, "/api/v1/operations/runs?"+test.query)
		require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())
		var page OperationRunsResponse
		require.NoError(json.Unmarshal(w.Body.Bytes(), &page))
		require.Len(page.Runs, 1)
		assert.Equal(test.kind, page.Runs[0].Kind)
		assert.Equal(test.unavailable, page.UnavailableKinds)
	}

	for _, lane := range []operations.Lane{operations.LaneDocuments, operations.LaneVisualAttachments} {
		w := doGet(srv, "/api/v1/operations/runs?lane="+string(lane))
		require.Equal(http.StatusOK, w.Code)
		var page OperationRunsResponse
		require.NoError(json.Unmarshal(w.Body.Bytes(), &page))
		assert.Empty(page.Runs)
		assert.Empty(page.UnavailableKinds)
	}
	require.NotEmpty(firstCursor)
	crossArchive := NewServerWithOptions(ServerOptions{
		Config:                 &config.Config{},
		Store:                  &operationArchiveRealStore{Store: st, uid: "abcdef1234567890abcdef1234567890"},
		OperationHistoryReader: st,
		Logger:                 testLogger(),
	})
	w := doGet(crossArchive, "/api/v1/operations/runs?limit=1&cursor="+firstCursor)
	assert.Equal(http.StatusBadRequest, w.Code)
	assert.Equal("invalid_cursor", decodeErrorEnvelope(t, w).Error)
}

func operationAPITimestamp(st *store.Store, value time.Time, milliseconds bool) any {
	if st.IsPostgreSQL() {
		return value.UTC()
	}
	if milliseconds {
		return value.UTC().Format("2006-01-02 15:04:05.000")
	}
	return value.UTC().Format("2006-01-02 15:04:05")
}
