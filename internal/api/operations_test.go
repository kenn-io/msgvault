package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/operations"
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

type operationHistoryStub struct {
	runs          []operations.Run
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

func (s *operationHistoryStub) ListRuns(_ context.Context, query operations.Query) ([]operations.Run, error) {
	s.queries = append(s.queries, query)
	return s.runs, s.listErr
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
				HistoryAvailability: definition.HistoryAvailability,
				UnavailableCode:     definition.UnavailableCode,
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
	assert.Equal([]string{
		"", "document_embedding_history_unavailable", "document_extraction_history_unavailable",
		"message_embedding_history_unavailable", "person_embedding_history_unavailable",
		"person_enrichment_history_unavailable", "", "", "visual_embedding_history_unavailable",
	}, []string{
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
		operations.KindCardDAVSync, operations.KindPersonSweep, operations.KindSourceSync,
	}, reader.statusQueries)
	assert.False(body.Lanes[0].Configured)
	assert.True(body.Lanes[1].Configured)
	assert.True(body.Lanes[2].Configured)
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
	srv.SetVisualOperations(func(context.Context) error { return nil }, func(context.Context) error { return nil },
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
	srv.SetVisualOperations(func(context.Context) error { return nil }, func(context.Context) error { return nil },
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

	srv.SetVisualOperations(func(context.Context) error { return nil }, func(context.Context) error { return nil },
		nil, func(context.Context, bool) (visual.Status, error) {
			return visual.Status{
				Generation:             store.VisualGeneration{State: store.VisualGenerationActive},
				ReconciliationComplete: true, JournalLag: 1,
			}, nil
		}, nil)
	w = doGet(srv, "/api/v1/operations/status")
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal([]operations.ActionID{operations.ActionVisualResume}, body.Lanes[8].SupportedActions)

	srv.SetVisualOperations(func(context.Context) error { return nil }, func(context.Context) error { return nil },
		nil, func(context.Context, bool) (visual.Status, error) {
			return visual.Status{
				Generation:             store.VisualGeneration{State: store.VisualGenerationActive},
				ReconciliationComplete: true, Converged: 2, ConvergenceTotal: 2,
			}, nil
		}, nil)
	w = doGet(srv, "/api/v1/operations/status")
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	assert.Empty(body.Lanes[8].SupportedActions)

	srv.SetVisualOperations(func(context.Context) error { return nil }, func(context.Context) error { return nil },
		nil, func(context.Context, bool) (visual.Status, error) {
			return visual.Status{}, errors.New("private visual provider failure")
		}, nil)
	w = doGet(srv, "/api/v1/operations/status")
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	assert.True(body.Lanes[8].Configured)
	assert.Empty(body.Lanes[8].SupportedActions)
	assert.NotContains(w.Body.String(), "private visual provider failure")

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
	require := require.New(t)
	assert := assert.New(t)
	run := operationRunFixture(t)
	reader := &operationHistoryStub{runs: []operations.Run{run, run}}
	srv := newOperationTestServer(reader, &operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID})

	w := doGet(srv, "/api/v1/operations/runs?limit=1")
	require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var body OperationRunsResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(body.Runs, 1)
	assert.NotEmpty(body.NextCursor)
	assert.Len(body.UnavailableKinds, 6)
	assert.Equal([]OperationUnavailableKind{
		{Kind: operations.KindDocumentEmbedding, Lane: operations.LaneDocuments, UnavailableCode: "document_embedding_history_unavailable"},
		{Kind: operations.KindDocumentExtraction, Lane: operations.LaneDocuments, UnavailableCode: "document_extraction_history_unavailable"},
		{Kind: operations.KindMessageEmbedding, Lane: operations.LaneMessages, UnavailableCode: "message_embedding_history_unavailable"},
		{Kind: operations.KindPersonEmbedding, Lane: operations.LanePersonFacts, UnavailableCode: "person_embedding_history_unavailable"},
		{Kind: operations.KindPersonEnrichment, Lane: operations.LanePersonFacts, UnavailableCode: "person_enrichment_history_unavailable"},
		{Kind: operations.KindVisualEmbedding, Lane: operations.LaneVisualAttachments, UnavailableCode: "visual_embedding_history_unavailable"},
	}, body.UnavailableKinds)
	assert.Equal(1, reader.queries[0].Limit)
	assert.Equal(operations.KindSourceSync, body.Runs[0].Kind)
	assert.NotNil(body.Runs[0].Counters)
	assert.NotContains(w.Body.String(), "archive")

	w = doGet(srv, "/api/v1/operations/runs")
	require.Equal(http.StatusOK, w.Code)
	assert.Equal(operationRunsDefaultLimit, reader.queries[1].Limit)
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
		{name: "unavailable kind", target: "/api/v1/operations/runs?kind=message_embedding", status: 503, code: "operation_history_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := doGet(srv, test.target)
			assert.Equal(t, test.status, w.Code)
			assert.Equal(t, test.code, decodeErrorEnvelope(t, w).Error)
		})
	}
}

func TestOperationRunsRejectsBoundCursorAndFailsAtomically(t *testing.T) {
	assert := assert.New(t)
	run := operationRunFixture(t)
	position := operations.Position{StartedAt: run.StartedAt, ID: run.ID}
	cursor, err := encodeOperationCursor(position, operationHistoryFilter{}, operationTestArchiveUID)
	require.NoError(t, err)

	reader := &operationHistoryStub{runs: []operations.Run{run}, listErr: errors.New("synthetic read failed")}
	srv := newOperationTestServer(reader, &operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID})
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
	srv := newOperationTestServer(reader, &operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID})
	ref, err := encodeOperationRunReference(run.ID, operationTestArchiveUID)
	require.NoError(err)
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
	ref, err := encodeOperationRunReference(run.ID, operationTestArchiveUID)
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
	st := testutil.NewTestStore(t)
	started := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	source, err := st.GetOrCreateSource("gmail", "private-source-identifier@example.invalid")
	require.NoError(err)
	var sourceRunID int64
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO sync_runs (
		source_id, started_at, completed_at, status, messages_processed, messages_added,
		messages_updated, errors_count, error_message, cursor_before, cursor_after
	) VALUES (?, ?, ?, 'completed', 7, 2, 1, 0, ?, ?, ?) RETURNING id`), source.ID,
		operationAPITimestamp(st, started, false), operationAPITimestamp(st, started.Add(time.Second), false),
		"private-source-error", "private-source-before", "private-source-after").Scan(&sourceRunID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO sync_run_items (
		sync_run_id, source_message_id, phase, status, error_kind, error_message
	) VALUES (?, ?, 'fetch', 'error', 'private-source-item-kind', ?)`),
		sourceRunID, "private-source-message-id", "private-source-item-error")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_sweep_runs (
		id, kind, mode, status, program_fingerprint, catalog_fingerprint,
		provider_fingerprint, attempt_count, success_count, failure_count,
		projected_write_count, started_at, completed_at
	) VALUES ('person-run', 'manual', 'incremental', 'succeeded', ?, ?, ?, 2, 2, 0, 1, ?, ?)`),
		"private-person-program", "private-person-catalog", "private-person-provider",
		operationAPITimestamp(st, started, true), operationAPITimestamp(st, started.Add(time.Second), true))
	require.NoError(err)
	var personID int64
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO persons (
		vcard_uid, display_name
	) VALUES (?, ?) RETURNING id`), "private-person-uid", "private-person-display").Scan(&personID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_sweep_attempts (
		id, run_id, person_id, lease_fence, mode, status, failure_class,
		cursor_envelope_json, envelope_hash, program_fingerprint, catalog_fingerprint,
		provider_fingerprint, generation_key, provider_request_id, input_tokens,
		output_tokens, estimated_cost_micro_usd, started_at, completed_at
	) VALUES (?, 'person-run', ?, 1, 'incremental', 'succeeded', '', ?, ?, ?, ?, ?, ?, ?, 987654321, 876543210, 765432109, ?, ?)`),
		"private-person-attempt-id", personID, `{"private":"person-cursor-envelope"}`,
		"private-person-envelope-hash", "private-person-attempt-program",
		"private-person-attempt-catalog", "private-person-attempt-provider",
		"private-person-model", "private-person-request-id",
		operationAPITimestamp(st, started, true), operationAPITimestamp(st, started.Add(time.Second), true))
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_sync_runs (
		trigger, state, started_at, finished_at, books, created, updated, removed, error_code, error_message
	) VALUES ('manual', 'failed', ?, ?, 1, 2, 3, 4, 'sync_failed', ?)`),
		operationAPITimestamp(st, started, false), operationAPITimestamp(st, started.Add(time.Second), false), "private-carddav-error")
	require.NoError(err)

	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, OperationHistoryReader: st, Logger: testLogger()})
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
		for _, marker := range []string{
			"private-source-identifier", "private-source-error", "private-source-before",
			"private-source-after", "private-source-message-id", "private-source-item-kind",
			"private-source-item-error", "private-person-program", "private-person-catalog",
			"private-person-provider", "private-person-uid", "private-person-display",
			"private-person-attempt-id", "person-cursor-envelope", "private-person-envelope-hash",
			"private-person-attempt-program", "private-person-attempt-catalog",
			"private-person-attempt-provider", "private-person-model", "private-person-request-id",
			"987654321", "876543210", "765432109", "private-carddav-error",
		} {
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
			unavailable: []OperationUnavailableKind{{
				Kind: operations.KindMessageEmbedding, Lane: operations.LaneMessages,
				UnavailableCode: "message_embedding_history_unavailable",
			}},
		},
		{
			query: "lane=person_facts", kind: operations.KindPersonSweep,
			unavailable: []OperationUnavailableKind{
				{Kind: operations.KindPersonEmbedding, Lane: operations.LanePersonFacts, UnavailableCode: "person_embedding_history_unavailable"},
				{Kind: operations.KindPersonEnrichment, Lane: operations.LanePersonFacts, UnavailableCode: "person_enrichment_history_unavailable"},
			},
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
		assert.Equal(http.StatusServiceUnavailable, w.Code)
		assert.Equal("operation_history_unavailable", decodeErrorEnvelope(t, w).Error)
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
