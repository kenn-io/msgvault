package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/personsearch"
	"go.kenn.io/msgvault/internal/vector/visual"
)

// fakeCmdVectorBackend satisfies vector.Backend for the init tests. It only
// implements the generation lookups the init-time freshness check performs
// (vector.ResolveActiveForFingerprint); every other method is left to the
// embedded nil interface because these tests never call them. The zero value
// reports "no active generation, none building" so the freshness check
// resolves to ErrNotEnabled and leaves the freshly-installed ready status
// untouched.
type fakeCmdVectorBackend struct {
	vector.Backend

	active    *vector.Generation
	activeErr error
}

type vectorInitVisualOpener struct{}

func (vectorInitVisualOpener) OpenStream(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, visual.ErrContentUnavailable
}

type vectorInitPersonBackend struct {
	*fakeCmdVectorBackend
	vector.PersonBackend
}

func (b *vectorInitPersonBackend) SearchPeople(
	context.Context, vector.GenerationID, []float32, int,
) ([]vector.PersonHit, error) {
	return nil, errors.New("synthetic installed person engine reached")
}

type vectorInitPersonStore struct{}

func (vectorInitPersonStore) ResolvePersonSemanticCandidatesContext(
	context.Context, []store.PersonSemanticCandidate,
) ([]store.Person, error) {
	return nil, nil
}

type vectorInitPersonEmbedder struct{}

func (vectorInitPersonEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return []float32{1, 0}, nil
}

func (b *fakeCmdVectorBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	if b.active != nil {
		return *b.active, nil
	}
	if b.activeErr != nil {
		return vector.Generation{}, b.activeErr
	}
	return vector.Generation{}, vector.ErrNoActiveGeneration
}

func (b *fakeCmdVectorBackend) BuildingGeneration(context.Context) (*vector.Generation, error) {
	// (nil, nil) is the interface's "nothing building" signal, which
	// ResolveActiveForFingerprint checks for explicitly.
	return nil, nil //nolint:nilnil // "no building generation" is a valid nil-value/nil-error result here
}

func newVectorInitTestServer(t *testing.T) *api.Server {
	t.Helper()
	return api.NewServerWithOptions(api.ServerOptions{
		Config:       &config.Config{},
		Logger:       slog.New(slog.DiscardHandler),
		VectorStatus: api.VectorStatusInitializing,
	})
}

func overrideSetupVectorFeatures(t *testing.T, fn func(context.Context, *store.Store, string, bool) (*vectorFeatures, error)) {
	t.Helper()
	prev := setupVectorFeaturesForRun
	setupVectorFeaturesForRun = func(ctx context.Context, s *store.Store, path string, readOnly bool, _ ...visual.StreamOpener) (*vectorFeatures, error) {
		return fn(ctx, s, path, readOnly)
	}
	t.Cleanup(func() { setupVectorFeaturesForRun = prev })
}

func waitForVectorStatus(t *testing.T, srv *api.Server, want api.VectorStatus) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, msg := srv.VectorStatus()
		if status == want {
			return msg
		}
		time.Sleep(5 * time.Millisecond)
	}
	status, _ := srv.VectorStatus()
	require.Equal(t, want, status, "vector status never reached %s", want)
	return ""
}

func TestVectorInitHandleWaitContextReturnsTrueWhenFinished(t *testing.T) {
	h := &vectorInitHandle{done: make(chan struct{})}
	close(h.done)
	assert.True(t, h.WaitContext(context.Background()),
		"finished init should report done")
}

func TestVectorInitHandleWaitContextReturnsFalseWhenCancelled(t *testing.T) {
	h := &vectorInitHandle{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.False(t, h.WaitContext(ctx),
		"a done context must stop the wait before init finishes")
}

// TestVectorInitHandleWaitContextPrefersDoneWhenBothReady verifies that when
// both the init has finished and the shutdown context is already expired,
// WaitContext deterministically reports done (true). Without the preference,
// the plain select could pick the ctx case and skip CloseFeatures, leaking
// vectors.db. Iterated so a flaky random-select regression is caught.
func TestVectorInitHandleWaitContextPrefersDoneWhenBothReady(t *testing.T) {
	for range 200 {
		h := &vectorInitHandle{done: make(chan struct{})}
		close(h.done)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.True(t, h.WaitContext(ctx),
			"finished init must report done even when ctx is also ready")
	}
}

func TestStartVectorInitDisabledFinishesImmediately(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = false
	withTestConfig(t, c)

	h := startVectorInit(context.Background(), nil, "", nil, nil, nil)
	assert.True(t, h.WaitTimeout(time.Second))
}

func TestStartVectorInitRunsForIndependentMultimodalLane(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = false
	c.Vector.Multimodal.Enabled = true
	withTestConfig(t, c)

	called := false
	prev := setupVectorFeaturesForRun
	setupVectorFeaturesForRun = func(context.Context, *store.Store, string, bool, ...visual.StreamOpener) (*vectorFeatures, error) {
		called = true
		return &vectorFeatures{Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { setupVectorFeaturesForRun = prev })

	h := startVectorInit(context.Background(), nil, "/tmp/msgvault.db", nil,
		newVectorInitTestServer(t), scheduler.New(nil))
	require.True(t, h.WaitTimeout(5*time.Second))
	assert.True(t, called, "multimodal-only enablement must initialize vector infrastructure")
}

func TestStartVectorInitInstallsFeaturesOnSuccess(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	closed := false
	backend := &vectorInitPersonBackend{fakeCmdVectorBackend: &fakeCmdVectorBackend{
		active: &vector.Generation{ID: 1, Fingerprint: c.Vector.GenerationFingerprint()},
	}}
	personEngine := personsearch.NewEngine(
		backend, vectorInitPersonStore{}, vectorInitPersonEmbedder{},
		personsearch.Config{ExpectedFingerprint: c.Vector.GenerationFingerprint()},
	)
	overrideSetupVectorFeatures(t, func(context.Context, *store.Store, string, bool) (*vectorFeatures, error) {
		return &vectorFeatures{
			Backend: backend, PersonSearchEngine: personEngine,
			Cfg: c.Vector, Close: func() error { closed = true; return nil },
		}, nil
	})

	srv := newVectorInitTestServer(t)
	sched := scheduler.New(nil)
	h := startVectorInit(context.Background(), nil, "/tmp/msgvault.db", nil, srv, sched)

	requirements.True(h.WaitTimeout(5 * time.Second))
	waitForVectorStatus(t, srv, api.VectorStatusReady)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/people/search",
		strings.NewReader(`{"query":"synthetic"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	assertions.Equal(http.StatusServiceUnavailable, response.Code)
	assertions.Contains(response.Body.String(), "semantic_search_unavailable",
		"ready status must publish the person engine in the same installation")
	assertions.NotContains(response.Body.String(), "vector_not_enabled",
		"ready status must never precede person engine installation")
	h.CloseFeatures()
	assertions.True(closed, "CloseFeatures must close the opened backend")
}

type registeredDocumentVectorJobCapture struct {
	calls        int
	job          func(context.Context) error
	schedule     string
	runAfterSync bool
}

func (c *registeredDocumentVectorJobCapture) SetDocumentVectorJob(
	job func(context.Context) error, schedule string, runAfterSync bool,
) error {
	c.calls++
	c.job = job
	c.schedule = schedule
	c.runAfterSync = runAfterSync
	return nil
}

type startupDocumentSearchLedger struct{}

func (startupDocumentSearchLedger) SearchDocuments(context.Context, store.DocumentSearchRequest) (store.DocumentSearchResponse, error) {
	return store.DocumentSearchResponse{}, nil
}

func (startupDocumentSearchLedger) GetDocumentIndexRevision(context.Context) (int64, error) {
	return 1, nil
}

func (startupDocumentSearchLedger) GetActiveDocumentVectorGeneration(context.Context) (*store.DocumentVectorGeneration, error) {
	return &store.DocumentVectorGeneration{
		ID:          1,
		Fingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Dimension:   3, TargetExtractionProfileID: "profile-a",
	}, nil
}

func (startupDocumentSearchLedger) GetDocumentVectorTargetProfileID(context.Context) (string, error) {
	return "profile-a", nil
}

func (startupDocumentSearchLedger) ResolveDocumentVectorSearchOccurrences(
	context.Context, int64, []store.DocumentVectorSearchHit, store.DocumentSearchRequest, int,
) ([]store.DocumentSearchResult, bool, error) {
	return nil, false, nil
}

type startupDocumentSemanticClient struct{}

func (startupDocumentSemanticClient) EmbedQuery(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0}, nil
}

func (startupDocumentSemanticClient) EmbedDocuments(context.Context, []vector.DocumentInput) ([][][]float32, error) {
	return nil, nil
}

type startupDocumentBackend struct{}

func (startupDocumentBackend) PutUnpublished(context.Context, vectordocument.GenerationID, int, []vectordocument.Embedding) error {
	return nil
}

func (startupDocumentBackend) DeleteTokens(context.Context, vectordocument.GenerationID, []string) error {
	return nil
}

func (startupDocumentBackend) Search(context.Context, vectordocument.GenerationID, int, []float32, int) ([]vectordocument.Hit, error) {
	return nil, nil
}

func (startupDocumentBackend) SearchPage(context.Context, vectordocument.GenerationID, int, []float32, string, int) (vectordocument.HitPage, error) {
	return vectordocument.HitPage{Exhausted: true}, nil
}

func TestRegisterDocumentVectorJobRequiresDocumentEmbeddingsAndBackend(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c := config.NewDefaultConfig()
	c.Vector.Embed.Schedule.Cron = "*/7 * * * *"
	c.Vector.Embed.Schedule.RunAfterSync = true
	withTestConfig(t, c)
	available := &vectorFeatures{DocumentBackend: startupDocumentBackend{}, Cfg: c.Vector}

	disabled := &registeredDocumentVectorJobCapture{}
	requirements.NoError(registerDocumentVectorJob(disabled, available, nil))
	assertions.Zero(disabled.calls)

	c.Attachments.Documents.Index.Embeddings.Enabled = true
	capture := &registeredDocumentVectorJobCapture{}
	requirements.NoError(registerDocumentVectorJob(capture, available, nil))
	assertions.Equal(1, capture.calls)
	assertions.NotNil(capture.job)
	assertions.Equal("*/7 * * * *", capture.schedule)
	assertions.True(capture.runAfterSync)

	for name, features := range map[string]*vectorFeatures{
		"disabled":    nil,
		"nil backend": {Cfg: c.Vector},
	} {
		t.Run(name, func(t *testing.T) {
			unregistered := &registeredDocumentVectorJobCapture{}
			require.NoError(t, registerDocumentVectorJob(unregistered, features, nil))
			assert.Zero(t, unregistered.calls)
		})
	}
}

func TestStartVectorInitInstallsOnlyConsentedDocumentSearch(t *testing.T) {
	for _, test := range []struct {
		name        string
		service     *vectordocument.SearchService
		wantStatus  int
		wantPayload string
	}{
		{name: "unconsented", wantStatus: http.StatusServiceUnavailable, wantPayload: "semantic_search_unavailable"},
		{name: "consented", service: vectordocument.NewSearchService(vectordocument.SearchDeps{
			Ledger: startupDocumentSearchLedger{}, Embedder: startupDocumentSemanticClient{}, Backend: startupDocumentBackend{},
		}), wantStatus: http.StatusOK, wantPayload: `"effective_mode":"semantic"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := config.NewDefaultConfig()
			c.Vector.Enabled = true
			withTestConfig(t, c)
			overrideSetupVectorFeatures(t, func(context.Context, *store.Store, string, bool) (*vectorFeatures, error) {
				return &vectorFeatures{
					Backend: &fakeCmdVectorBackend{}, DocumentSearch: test.service,
					DocumentBackend: startupDocumentBackend{}, SemanticClient: startupDocumentSemanticClient{},
					Cfg: c.Vector, Close: func() error { return nil },
				}, nil
			})
			mainStore := testutil.NewTestStore(t)
			srv := api.NewServerWithOptions(api.ServerOptions{
				Config: c, Store: &storeAPIAdapter{store: mainStore}, Logger: slog.New(slog.DiscardHandler),
				VectorStatus: api.VectorStatusInitializing,
			})
			h := startVectorInit(t.Context(), mainStore, "/tmp/msgvault.db", nil, srv, scheduler.New(nil))
			require.True(t, h.WaitTimeout(5*time.Second))

			request := httptest.NewRequest(http.MethodGet, "/api/v1/documents/search?q=bounded&mode=semantic&candidate_limit=10", nil)
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			assert.Equal(t, test.wantStatus, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), test.wantPayload)
			h.CloseFeatures()
		})
	}
}

func TestStartVectorInitFlagsStaleIndex(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	// Active generation's fingerprint differs from the configured one, so
	// the same check the query path runs (ResolveActiveForFingerprint)
	// reports ErrIndexStale at init completion.
	overrideSetupVectorFeatures(t, func(context.Context, *store.Store, string, bool) (*vectorFeatures, error) {
		return &vectorFeatures{
			Backend: &fakeCmdVectorBackend{
				active: &vector.Generation{ID: 1, Fingerprint: "old-model:384:c6000:e1"},
			},
			Cfg:   c.Vector,
			Close: func() error { return nil },
		}, nil
	})

	srv := newVectorInitTestServer(t)
	h := startVectorInit(context.Background(), nil, "/tmp/msgvault.db", nil, srv, scheduler.New(nil))

	require.True(t, h.WaitTimeout(5*time.Second))
	detail := waitForVectorStatus(t, srv, api.VectorStatusStale)
	assert := assert.New(t)
	assert.Contains(detail, "old-model:384:c6000:e1", "detail names the stored fingerprint")
	assert.Contains(detail, c.Vector.GenerationFingerprint(), "detail names the configured fingerprint")
	assert.Contains(detail, "msgvault embeddings build --full-rebuild", "detail names the rebuild command")
}

func TestStartVectorInitReportsError(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	overrideSetupVectorFeatures(t, func(context.Context, *store.Store, string, bool) (*vectorFeatures, error) {
		return nil, errors.New("migration exploded")
	})

	srv := newVectorInitTestServer(t)
	h := startVectorInit(context.Background(), nil, "/tmp/msgvault.db", nil, srv, scheduler.New(nil))

	require.True(t, h.WaitTimeout(5*time.Second))
	msg := waitForVectorStatus(t, srv, api.VectorStatusError)
	assert.Contains(t, msg, "migration exploded")
}

func TestStartVectorInitHoldsWorkTracker(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	gate := api.NewSerialOperationGate()
	release := make(chan struct{})
	overrideSetupVectorFeatures(t, func(ctx context.Context, _ *store.Store, _ string, _ bool) (*vectorFeatures, error) {
		<-release
		return nil, ctx.Err()
	})

	srv := newVectorInitTestServer(t)
	h := startVectorInit(context.Background(), nil, "/tmp/msgvault.db", gate, srv, scheduler.New(nil))

	// While init runs, the gate must be held: BeginWorkContext with an
	// already-cancelled context must fail rather than acquire.
	assert.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		done, ok := gate.BeginWorkContext(ctx)
		if ok {
			done()
		}
		return !ok
	}, 2*time.Second, 10*time.Millisecond, "gate should be held during init")

	close(release)
	require.True(t, h.WaitTimeout(5*time.Second))
	done, ok := gate.BeginWork()
	require.True(t, ok, "gate must be released after init")
	done()
}

func TestStartVectorInitAbortsQuietlyOnCancel(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	overrideSetupVectorFeatures(t, func(ctx context.Context, _ *store.Store, _ string, _ bool) (*vectorFeatures, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	srv := newVectorInitTestServer(t)
	h := startVectorInit(ctx, nil, "/tmp/msgvault.db", nil, srv, scheduler.New(nil))
	cancel()

	require.True(t, h.WaitTimeout(5*time.Second))
	status, _ := srv.VectorStatus()
	assert.Equal(t, api.VectorStatusInitializing, status,
		"shutdown-cancelled init must not flip status to error")
}

func TestNewSchedulerEmbedJobThreadsGenerationRunnerAndConvergenceChecker(t *testing.T) {
	runner := &embed.GenerationWorker{}
	checker := &registeredConvergenceChecker{}
	vf := &vectorFeatures{
		Backend: &fakeCmdVectorBackend{}, Runner: runner, Convergence: checker,
		Cfg: config.NewDefaultConfig().Vector,
	}

	job := newSchedulerEmbedJob(vf, nil)
	assert.Same(t, runner, job.Worker)
	assert.Same(t, checker, job.Convergence)
}

type registeredEmbedJobCapture struct {
	job *scheduler.EmbedJob
}

func (c *registeredEmbedJobCapture) SetEmbedJob(job *scheduler.EmbedJob, _ string, _ bool) error {
	c.job = job
	return nil
}

type registeredContextRunner struct {
	runs []vector.GenerationID
}

func (r *registeredContextRunner) ReclaimStale(context.Context) (int, error) { return 0, nil }

func (r *registeredContextRunner) RunOnce(
	_ context.Context, gen vector.GenerationID, _ operations.PassScope,
) (embed.RunResult, error) {
	r.runs = append(r.runs, gen)
	return embed.RunResult{}, nil
}

func (r *registeredContextRunner) RunBackstop(
	context.Context, vector.GenerationID, operations.PassScope,
) (embed.RunResult, error) {
	return embed.RunResult{}, nil
}

type registeredConvergenceChecker struct {
	state scheduler.ConvergenceResult
}

func (c registeredConvergenceChecker) CheckConvergence(context.Context, vector.GenerationID) (scheduler.ConvergenceResult, error) {
	return c.state, nil
}

type registeredActivationBackend struct {
	vector.Backend

	building       *vector.Generation
	activateErr    error
	activateCalls  []vector.GenerationID
	activatedCalls []vector.GenerationID
	sequences      []int64
}

func (b *registeredActivationBackend) ActivateGenerationIfConverged(
	ctx context.Context, gen vector.GenerationID, expectedSequence int64,
) error {
	b.sequences = append(b.sequences, expectedSequence)
	return b.ActivateGeneration(ctx, gen, false)
}

func (b *registeredActivationBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	return vector.Generation{}, vector.ErrNoActiveGeneration
}

func (b *registeredActivationBackend) BuildingGeneration(context.Context) (*vector.Generation, error) {
	return b.building, nil
}

func (b *registeredActivationBackend) ActivateGeneration(_ context.Context, gen vector.GenerationID, force bool) error {
	if force {
		panic("registered daemon activation must never force")
	}
	b.activateCalls = append(b.activateCalls, gen)
	if b.activateErr != nil {
		return b.activateErr
	}
	b.activatedCalls = append(b.activatedCalls, gen)
	return nil
}

func contextualSchedulerConfig() vector.Config {
	c := config.NewDefaultConfig().Vector
	c.Embeddings.APIFormat = vector.APIFormatVoyageContextual
	c.Embeddings.Model = "voyage-context-4"
	c.Embeddings.Dimension = 4
	c.Embed.BackstopInterval = -1
	return c
}

func runRegisteredContextJob(
	t *testing.T,
	state scheduler.ConvergenceResult,
	buildingFingerprint string,
	activateErr error,
) (*registeredActivationBackend, *registeredContextRunner) {
	t.Helper()
	vectorCfg := contextualSchedulerConfig()
	testCfg := config.NewDefaultConfig()
	testCfg.Vector = vectorCfg
	withTestConfig(t, testCfg)
	if buildingFingerprint == "" {
		buildingFingerprint = vectorCfg.GenerationFingerprint()
	}
	backend := &registeredActivationBackend{
		building:    &vector.Generation{ID: 13, State: vector.GenerationBuilding, Fingerprint: buildingFingerprint},
		activateErr: activateErr,
	}
	runner := &registeredContextRunner{}
	capture := &registeredEmbedJobCapture{}
	vf := &vectorFeatures{
		Backend: backend, Runner: runner,
		Convergence: registeredConvergenceChecker{state: state}, Cfg: vectorCfg,
	}
	require.NoError(t, registerEmbedJob(capture, vf, nil, nil))
	require.NotNil(t, capture.job)
	capture.job.Run(t.Context())
	return backend, runner
}

func TestRegisterEmbedJob_ContextualActivationRequiresEveryConvergenceDimension(t *testing.T) {
	rootAssert := assert.New(t)
	complete := scheduler.ConvergenceResult{
		MessageCoverageComplete: true,
		PersonCoverageComplete:  true,
		LatestJournalSequence:   5,
		ConsumedJournalSequence: 5,
		ReconciliationComplete:  true,
	}
	tests := []struct {
		name   string
		mutate func(*scheduler.ConvergenceResult)
	}{
		{name: "message coverage incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.MessageCoverageComplete = false
			s.MessageCoverageMissing = 1
		}},
		{name: "person coverage incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.PersonCoverageComplete = false
			s.PersonCoverageMismatched = 1
		}},
		{name: "journal incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.ConsumedJournalSequence = 4
		}},
		{name: "reconciliation incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.ReconciliationComplete = false
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := assert.New(t)
			state := complete
			tt.mutate(&state)
			backend, runner := runRegisteredContextJob(t, state, "", nil)
			check.Equal([]vector.GenerationID{13}, runner.runs)
			check.Empty(backend.activateCalls)
			check.Empty(backend.activatedCalls)
		})
	}

	backend, runner := runRegisteredContextJob(t, complete, "", nil)
	rootAssert.Equal([]vector.GenerationID{13}, runner.runs)
	rootAssert.Equal([]vector.GenerationID{13}, backend.activateCalls)
	rootAssert.Equal([]vector.GenerationID{13}, backend.activatedCalls)
	rootAssert.Equal([]int64{5}, backend.sequences)
}

func TestRegisterEmbedJob_ContextualLifecycleRefusalsNeverActivateAnotherGeneration(t *testing.T) {
	complete := scheduler.ConvergenceResult{
		MessageCoverageComplete: true,
		PersonCoverageComplete:  true,
		LatestJournalSequence:   5,
		ConsumedJournalSequence: 5,
		ReconciliationComplete:  true,
	}
	t.Run("wrong fingerprint", func(t *testing.T) {
		check := assert.New(t)
		backend, runner := runRegisteredContextJob(t, complete, "wrong-contextual-generation", nil)
		check.Empty(runner.runs)
		check.Empty(backend.activateCalls)
		check.Empty(backend.activatedCalls)
	})
	t.Run("retired during activation", func(t *testing.T) {
		check := assert.New(t)
		backend, runner := runRegisteredContextJob(t, complete, "", vector.ErrGenerationRetired)
		check.Equal([]vector.GenerationID{13}, runner.runs)
		check.Equal([]vector.GenerationID{13}, backend.activateCalls)
		check.Equal([]int64{5}, backend.sequences)
		check.Empty(backend.activatedCalls)
	})
}

func TestRequireVisualConsentRejectsRetiredGeneration(t *testing.T) {
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	generation, err := st.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{
		Fingerprint: "visual-consent-state", Model: "voyage-multimodal-3.5", Dimension: 1024,
	})
	require.NoError(err)
	require.NoError(st.ConsentVisualGeneration(t.Context(), generation.ID, "policy-fp"))
	vf := &visualFeatures{Archive: st, Generation: generation, PolicyFingerprint: "policy-fp"}
	require.NoError(requireVisualConsent(t.Context(), vf))

	// Once retired, the installed resume and retry callbacks must refuse to
	// clean and repopulate the generation: activation can never expose it.
	require.NoError(st.RetireVisualGeneration(t.Context(), generation.ID))
	err = requireVisualConsent(t.Context(), vf)
	require.Error(err)
	require.Contains(err.Error(), "retired")
}

func TestVisualOperationRecordsOnePartialPassAndReusesTerminalRequest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	vf := &visualFeatures{Archive: st}
	scope := testOperationPassScope("visual:http-resume")
	wantErr := errors.New("provider failed after one durable publication")
	executions := 0
	execute := func(context.Context) (visual.WorkerResult, error) {
		executions++
		return visual.WorkerResult{Attempted: 2, Succeeded: 1, Failed: 1}, wantErr
	}

	err := runVisualOperation(t.Context(), vf, scope, execute)
	require.ErrorIs(err, wantErr)
	runs := operationRunsForKind(t, st, operations.KindVisualEmbedding)
	require.Len(runs, 1)
	assert.Equal(operations.StatePartial, runs[0].State)
	assert.Equal([]operations.PublicCounter{
		{Name: operations.CounterAttempted, Unit: operations.CounterUnitAttachments, Value: 2},
		{Name: operations.CounterSucceeded, Unit: operations.CounterUnitAttachments, Value: 1},
		{Name: operations.CounterFailed, Unit: operations.CounterUnitAttachments, Value: 1},
	}, runs[0].Counters)

	require.NoError(runVisualOperation(t.Context(), vf, scope, execute),
		"partial replay intentionally reports the useful completed outcome")
	assert.Equal(1, executions, "a terminal request-owned invocation must not execute again")
	assert.Len(operationRunsForKind(t, st, operations.KindVisualEmbedding), 1)
}

func TestVisualOperationTerminalReplayPreservesFixedNonSuccess(t *testing.T) {
	tests := []struct {
		name    string
		runErr  error
		wantIs  error
		message string
	}{
		{name: "failed", runErr: errors.New("private provider response"), message: "Upstream operation failed."},
		{name: "cancelled", runErr: context.Canceled, wantIs: context.Canceled, message: "Operation was cancelled."},
		{name: "timed out", runErr: context.DeadlineExceeded, wantIs: context.DeadlineExceeded, message: "Operation timed out."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewSQLiteTestStore(t)
			vf := &visualFeatures{Archive: st}
			scope := testOperationPassScope("visual:terminal-replay:" + test.name)
			executions := 0
			execute := func(context.Context) (visual.WorkerResult, error) {
				executions++
				return visual.WorkerResult{}, test.runErr
			}

			require.ErrorIs(runVisualOperation(t.Context(), vf, scope, execute), test.runErr)
			replayErr := runVisualOperation(t.Context(), vf, scope, execute)
			require.Error(replayErr)
			assert.Equal(test.message, replayErr.Error())
			assert.NotContains(replayErr.Error(), "private provider response")
			if test.wantIs != nil {
				require.ErrorIs(replayErr, test.wantIs)
			}
			assert.Equal(1, executions, "terminal replay must not re-execute the visual pass")
		})
	}
}

func TestVisualOperationRequiresRecorderBeforeExecution(t *testing.T) {
	executed := false
	err := runVisualOperation(t.Context(), &visualFeatures{}, testOperationPassScope("visual:missing-recorder"),
		func(context.Context) (visual.WorkerResult, error) {
			executed = true
			return visual.WorkerResult{}, nil
		})

	require.ErrorContains(t, err, "operation recorder is required")
	assert.False(t, executed)
}

func TestScheduledVisualPassRecordsPreflightFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	generation, err := st.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{
		Fingerprint: "scheduled-visual-pass", Model: "visual-test", Dimension: 1024,
	})
	require.NoError(err)
	reconciler, err := visual.NewReconciler(st, vectorInitVisualOpener{}, visual.ReconcileConfig{
		GenerationID: generation.ID, ConsumerKey: "visual-test/scheduled-operation-pass",
		LeaseOwner: "scheduled-operation-pass", LeaseDuration: time.Minute,
	})
	require.NoError(err)
	wantErr := errors.New("scheduled visual scope drift")
	vf := &visualFeatures{
		Archive: st, Reconciler: reconciler, Generation: generation,
		ScopeCheck: func(context.Context) error {
			return wantErr
		},
	}
	scope := operations.PassScope{
		Key: "scheduled:visual:test-pass", Trigger: operations.TriggerScheduled,
		StartedAt: time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC),
	}

	err = runVisualPass(t.Context(), vf, scope)
	require.ErrorIs(err, wantErr)
	runs := operationRunsForKind(t, st, operations.KindVisualEmbedding)
	require.Len(runs, 1)
	assert.Equal(operations.StateFailed, runs[0].State)
	require.NotNil(runs[0].Trigger)
	assert.Equal(operations.TriggerScheduled, *runs[0].Trigger)
}

func TestVisualHTTPProductionRegistrationRecordsBuildResumeAndRetryPasses(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c := config.NewDefaultConfig()
	c.Vector.Enabled = false
	c.Vector.Multimodal.Enabled = true
	c.Vector.Multimodal.Schedule.Cron = ""
	c.Vector.Multimodal.Schedule.RunAfterSync = false
	withTestConfig(t, c)
	st := testutil.NewSQLiteTestStore(t)
	generation, err := st.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{
		Fingerprint: "visual-http-production-registration", Model: "visual-test", Dimension: 1024,
	})
	require.NoError(err)
	reconciler := newVectorInitVisualReconciler(
		t, st, generation.ID, "visual-test/http-production-registration",
	)
	scopeChecks := 0
	visualRuntime := &visualFeatures{
		Archive: st, Reconciler: reconciler, Generation: generation,
		PolicyFingerprint: "private-policy-fingerprint",
		ScopeCheck: func(context.Context) error {
			scopeChecks++
			return nil
		},
	}
	overrideSetupVectorFeatures(t, func(context.Context, *store.Store, string, bool) (*vectorFeatures, error) {
		return &vectorFeatures{Cfg: c.Vector, Visual: visualRuntime, Close: func() error { return nil }}, nil
	})
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: c, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler),
		VectorStatus: api.VectorStatusInitializing,
	})
	handle := startVectorInit(t.Context(), st, "/tmp/msgvault.db", nil, srv, scheduler.New(nil))
	require.True(handle.WaitTimeout(5 * time.Second))
	t.Cleanup(handle.CloseFeatures)
	router := srv.Router()
	privateRequestID := "private-http-operation-request-owner"
	privateBlobHash := strings.Repeat("ab", 32)
	privateMessageID := "9000000000000000001"

	response := serveVisualOperationRequest(t, router, "/api/v1/multimodal/build",
		`{"consent":true}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	require.Len(operationRunsForKind(t, st, operations.KindVisualEmbedding), 1)
	assert.Equal(1, scopeChecks)

	response = serveVisualOperationRequest(t, router, "/api/v1/multimodal/build",
		`{"consent":true}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	require.Len(operationRunsForKind(t, st, operations.KindVisualEmbedding), 1,
		"terminal replay of the same request/action must reuse the existing row")
	assert.Equal(1, scopeChecks, "terminal replay must not re-execute the visual pass")

	response = serveVisualOperationRequest(t, router, "/api/v1/multimodal/run", "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	require.Len(operationRunsForKind(t, st, operations.KindVisualEmbedding), 2)

	response = serveVisualOperationRequest(t, router, "/api/v1/multimodal/retry",
		`{"message_id":`+privateMessageID+`,"blob_hash":"`+privateBlobHash+`"}`)
	require.Equal(http.StatusBadGateway, response.Code, response.Body.String())
	runs := operationRunsForKind(t, st, operations.KindVisualEmbedding)
	require.Len(runs, 3, "build, resume, and retry own distinct action-scoped rows")
	assert.Equal(3, scopeChecks)
	assert.ElementsMatch([]operations.State{
		operations.StateSucceeded, operations.StateSucceeded, operations.StateFailed,
	}, []operations.State{runs[0].State, runs[1].State, runs[2].State})
	for _, run := range runs {
		require.NotNil(run.Trigger)
		assert.Equal(operations.TriggerManual, *run.Trigger)
	}
	keys := visualInvocationKeys(t, st)
	require.Len(keys, 3)
	assert.Contains(keys[0], "http:visual:build:")
	assert.Contains(keys[1], "http:visual:resume:")
	assert.Contains(keys[2], "http:visual:retry:")
	for _, privateValue := range []string{privateRequestID, privateBlobHash, privateMessageID} {
		assert.NotContains(fmt.Sprint(runs), privateValue)
		assert.NotContains(strings.Join(keys, "\n"), privateValue)
	}
}

func TestRegisterVisualJobSkipsUnconsentedScheduledPassWithoutRow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c := config.NewDefaultConfig()
	c.Vector.Multimodal.Schedule.Cron = "0 0 1 1 *"
	c.Vector.Multimodal.Schedule.RunAfterSync = false
	withTestConfig(t, c)
	st := testutil.NewSQLiteTestStore(t)
	generation, err := st.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{
		Fingerprint: "visual-scheduled-unconsented", Model: "visual-test", Dimension: 1024,
	})
	require.NoError(err)
	vf := &visualFeatures{
		Archive: st, Generation: generation, PolicyFingerprint: "private-policy-fingerprint",
		Reconciler: newVectorInitVisualReconciler(t, st, generation.ID, "visual-test/scheduled-unconsented"),
	}
	sched := scheduler.New(nil)
	require.NoError(registerVisualJob(sched, vf))

	require.NoError(sched.TriggerJob("multimodal-attachments"))
	assert.Empty(operationRunsForKind(t, st, operations.KindVisualEmbedding),
		"the outer consent gate skips before runVisualPass owns a row")
	_, err = st.GetAttachmentChangeConsumer(t.Context(), "visual-test/scheduled-unconsented")
	assert.ErrorIs(err, store.ErrAttachmentChangeConsumerMissing,
		"the skipped callback must not enter reconciliation")
}

func TestRegisterVisualJobGenerationStateGateSkipsBeforePass(t *testing.T) {
	for _, test := range []struct {
		name           string
		prepare        func(*testing.T, *store.Store, store.VisualGeneration) int64
		wantMissing    bool
		wantTriggerErr bool
	}{
		{
			name: "retired",
			prepare: func(t *testing.T, st *store.Store, generation store.VisualGeneration) int64 {
				t.Helper()
				require.NoError(t, st.ConsentVisualGeneration(
					t.Context(), generation.ID, "private-policy-fingerprint",
				))
				require.NoError(t, st.RetireVisualGeneration(t.Context(), generation.ID))
				return generation.ID
			},
		},
		{
			name: "missing",
			prepare: func(_ *testing.T, _ *store.Store, generation store.VisualGeneration) int64 {
				return generation.ID + 10_000
			},
			wantMissing: true, wantTriggerErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			c := config.NewDefaultConfig()
			c.Vector.Multimodal.Schedule.Cron = "0 0 1 1 *"
			c.Vector.Multimodal.Schedule.RunAfterSync = false
			withTestConfig(t, c)
			st := testutil.NewSQLiteTestStore(t)
			generation, err := st.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{
				Fingerprint: "visual-scheduled-state-gate-" + test.name,
				Model:       "visual-test", Dimension: 1024,
			})
			require.NoError(err)
			generationID := test.prepare(t, st, generation)
			consumerKey := "visual-test/scheduled-state-gate-" + test.name
			vf := &visualFeatures{
				Archive: st, Generation: store.VisualGeneration{ID: generationID},
				PolicyFingerprint: "private-policy-fingerprint",
				Reconciler:        newVectorInitVisualReconciler(t, st, generationID, consumerKey),
			}
			sched := scheduler.New(nil)
			require.NoError(registerVisualJob(sched, vf))

			triggerErr := sched.TriggerJob("multimodal-attachments")
			if test.wantTriggerErr {
				require.ErrorIs(triggerErr, sql.ErrNoRows)
			} else {
				require.NoError(triggerErr)
			}
			assert.Empty(operationRunsForKind(t, st, operations.KindVisualEmbedding),
				"an unavailable generation must return before runVisualPass owns a row")
			_, err = st.GetAttachmentChangeConsumer(t.Context(), consumerKey)
			require.ErrorIs(err, store.ErrAttachmentChangeConsumerMissing,
				"the generation state gate must not enter reconciliation")
			if test.wantMissing {
				_, err = st.GetVisualGeneration(t.Context(), generationID)
				assert.ErrorIs(err, sql.ErrNoRows)
			}
		})
	}
}

func TestRegisterVisualJobRecordsLaterPostActivationMaintenancePass(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c := config.NewDefaultConfig()
	c.Vector.Multimodal.Schedule.Cron = "0 0 1 1 *"
	c.Vector.Multimodal.Schedule.RunAfterSync = false
	withTestConfig(t, c)
	st := testutil.NewSQLiteTestStore(t)
	generation, err := st.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{
		Fingerprint: "visual-scheduled-post-activation", Model: "visual-test", Dimension: 1024,
	})
	require.NoError(err)
	require.NoError(st.ConsentVisualGeneration(t.Context(), generation.ID, "private-policy-fingerprint"))
	vf := &visualFeatures{
		Archive: st, Generation: generation, PolicyFingerprint: "private-policy-fingerprint",
		Reconciler: newVectorInitVisualReconciler(t, st, generation.ID, "visual-test/post-activation"),
	}
	sched := scheduler.New(nil)
	require.NoError(registerVisualJob(sched, vf))

	require.NoError(sched.TriggerJob("multimodal-attachments"))
	activated, err := st.GetVisualGeneration(t.Context(), generation.ID)
	require.NoError(err)
	require.Equal(store.VisualGenerationActive, activated.State)
	firstRuns := operationRunsForKind(t, st, operations.KindVisualEmbedding)
	require.Len(firstRuns, 1)

	require.NoError(sched.TriggerJob("multimodal-attachments"))
	afterMaintenance, err := st.GetVisualGeneration(t.Context(), generation.ID)
	require.NoError(err)
	assert.Equal(store.VisualGenerationActive, afterMaintenance.State,
		"post-activation maintenance must preserve the active generation")
	runs := operationRunsForKind(t, st, operations.KindVisualEmbedding)
	require.Len(runs, 2, "a later maintenance callback owns a fresh scheduled row")
	assert.NotEqual(runs[0].ID, runs[1].ID)
	for _, run := range runs {
		require.NotNil(run.Trigger)
		assert.Equal(operations.TriggerScheduled, *run.Trigger)
		assert.Equal(operations.StateSucceeded, run.State)
	}
	keys := visualInvocationKeys(t, st)
	require.Len(keys, 2)
	assert.NotEqual(keys[0], keys[1])
}

func newVectorInitVisualReconciler(
	t *testing.T, st *store.Store, generationID int64, consumerKey string,
) *visual.Reconciler {
	t.Helper()
	reconciler, err := visual.NewReconciler(st, vectorInitVisualOpener{}, visual.ReconcileConfig{
		GenerationID: generationID, ConsumerKey: consumerKey,
		LeaseOwner: consumerKey, LeaseDuration: time.Minute,
	})
	require.NoError(t, err)
	return reconciler
}

func serveVisualOperationRequest(
	t *testing.T, handler http.Handler, path, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("X-Request-Id", "private-http-operation-request-owner")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func visualInvocationKeys(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB().QueryContext(t.Context(),
		`SELECT invocation_key FROM visual_embedding_runs ORDER BY id`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var keys []string
	for rows.Next() {
		var key string
		require.NoError(t, rows.Scan(&key))
		keys = append(keys, key)
	}
	require.NoError(t, rows.Err())
	return keys
}
