//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docbankdocument "go.kenn.io/docbank/document"
	"go.kenn.io/kit/embedlocal"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

type localBuildTestRunner struct {
	t           *testing.T
	home        string
	calls       [][]string
	contexts    []context.Context
	inspectOnly bool
	failBuild   bool
}

func (r *localBuildTestRunner) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	r.calls = append(r.calls, slices.Clone(req.Args))
	r.contexts = append(r.contexts, ctx)
	index := slices.Index(req.Args, "--config")
	require.Positive(r.t, index)
	snapshot, err := config.ReadConfigFile(req.Args[index+1])
	require.NoError(r.t, err)
	home := ""
	if homeIndex := slices.Index(req.Args, "--home"); homeIndex >= 0 {
		home = req.Args[homeIndex+1]
	}
	cfg, err := config.LoadConfigFile(snapshot, home)
	require.NoError(r.t, err)
	require.Equal(r.t, r.home, cfg.HomeDir)
	if r.inspectOnly {
		return nil
	}
	if r.failBuild && req.Args[1] == "build" {
		r.failBuild = false
		if emit != nil {
			require.NoError(r.t, emit(api.CLIRunEvent{Type: cliStreamStderr, Data: strings.Repeat("index progress\n", 1024)}))
			require.NoError(r.t, emit(api.CLIRunEvent{Type: cliStreamStderr, Data: "embedding scope matched 0 live messages; "}))
			require.NoError(r.t, emit(api.CLIRunEvent{Type: cliStreamStderr, Data: "sync the scoped account(s) first\n"}))
		}
		return errors.New(cliSubprocessExitSentinel)
	}
	state := *invocationFromContext(ctx)
	state.cfg = cfg
	oldYes, oldRebuild, oldRetire := embedYes, embedFullRebuild, embeddingsRetireYes
	defer func() { embedYes, embedFullRebuild, embeddingsRetireYes = oldYes, oldRebuild, oldRetire }()
	command := newEmbeddingsBuildCmd("build")
	command.RunE = func(cmd *cobra.Command, _ []string) error { return runEmbed(cmd) }
	if req.Args[1] == "retire" {
		command = &cobra.Command{Use: "retire", RunE: runEmbeddingsRetire}
		command.Flags().BoolVar(&embeddingsRetireYes, "yes", false, "Confirm retirement")
	}
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs(req.Args[2:index])
	if err := command.ExecuteContext(withInvocation(ctx, &state)); err != nil {
		return fmt.Errorf("build test index: %w", err)
	}
	return nil
}

func TestLocalEmbeddingsActivatesAndPreservesMatchingMessageIndex(t *testing.T) {
	for _, initialModel := range []string{"", "nomic-embed-text", "different-model", "available-ollama", "matching-build", "other-build", "documents", "documents-conflict", "people", "people-unconsented", "build-failure"} {
		t.Run("initial="+initialModel, func(t *testing.T) {
			pendingKind := initialModel
			if strings.HasPrefix(pendingKind, "documents") || strings.HasPrefix(pendingKind, "people") {
				initialModel = "different-model"
			}
			if pendingKind == "build-failure" {
				initialModel = ""
			}
			if pendingKind == "matching-build" {
				initialModel = "nomic-embed-text"
			}
			if pendingKind == "other-build" {
				initialModel = "different-model"
			}
			useOllama := initialModel == "available-ollama"
			if useOllama {
				initialModel = ""
			}
			dir := t.TempDir()
			var inputs atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/embeddings", r.URL.Path)
				var body struct {
					Input []string `json:"input"`
				}
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
					http.Error(w, "invalid embedding request", http.StatusBadRequest)
					return
				}
				inputs.Add(int32(len(body.Input)))
				data := make([]map[string]any, len(body.Input))
				for i := range data {
					vec := make([]float32, 768)
					vec[0] = 1
					data[i] = map[string]any{"index": i, "embedding": vec}
				}
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
			}))
			defer endpoint.Close()
			oldEndpoint, oldDir := localEmbeddingEndpoint, localEmbeddingsDir
			localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return endpoint.URL + "/v1", nil }
			localEmbeddingsDir = func() (string, error) { return filepath.Join(dir, "shared"), nil }
			t.Cleanup(func() { localEmbeddingEndpoint, localEmbeddingsDir = oldEndpoint, oldDir })
			content := "[data]\ndata_dir = " + strconv.Quote(dir) + "\n[vector]\nenabled = false\n"
			if initialModel != "" {
				content = "[data]\ndata_dir = " + strconv.Quote(dir) + "\n[vector]\nenabled = true\n[vector.embeddings]\nmodel = " + strconv.Quote(initialModel) + "\ndimension = 768\nendpoint = " + strconv.Quote(endpoint.URL+"/v1") + "\ndocument_prefix = \"document: \"\nquery_prefix = \"query: \"\nmax_input_chars = 1000\n"
			}
			content += "\n[chat]\nserver = " + strconv.Quote(endpoint.URL) + "\n"
			if strings.HasPrefix(pendingKind, "documents") {
				content += "\n[attachments.documents]\nenabled = true\n[attachments.documents.index]\nenabled = true\n[attachments.documents.index.embeddings]\nenabled = true\nprofile = \"vector.embeddings\"\n"
			}
			if strings.HasPrefix(pendingKind, "people") {
				content += "\n[vector.people]\nenabled = true\nretention_posture = \"zero_data_retention\"\ntraining_posture = \"no_training\"\n"
			}
			path := filepath.Join(dir, "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			snapshot, err := config.ReadConfigFile(path)
			require.NoError(t, err)
			cfg, err := config.LoadConfigFile(snapshot, dir)
			require.NoError(t, err)
			cfg.Server.APIKey = "owner"
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(t, err)
			defer func() { _ = st.Close() }()
			require.NoError(t, st.InitSchema())
			_, messageID, err := testutil.CreateIndexedSourceMessage(st, "archive@example.test", "first", "Orchard", "The orchard has apples.")
			require.NoError(t, err)
			state := &invocation{cfg: cfg, logger: slog.New(slog.DiscardHandler)}
			runner := &localBuildTestRunner{t: t, home: dir}
			ctx := withInvocation(t.Context(), state)
			if strings.HasPrefix(pendingKind, "people") {
				participant, err := st.EnsureParticipantByIdentifier("email", "curated@example.test", "Curated Orchard Person")
				require.NoError(t, err)
				_, _, err = st.CreatePersonFromParticipantContext(ctx, participant)
				require.NoError(t, err)
				if pendingKind == "people" {
					profile, err := cfg.Vector.SemanticPersonEmbeddingProfile()
					require.NoError(t, err)
					_, err = st.EnsurePersonSemanticEmbeddingProfile(ctx, profile)
					require.NoError(t, err)
					_, _, err = st.GrantPersonSemanticEmbeddingConsent(ctx, profile.Fingerprint, "test")
					require.NoError(t, err)
				}
			}
			if strings.HasPrefix(pendingKind, "documents") {
				seedLocalSearchDocument(t, st, messageID)
				spec, err := configuredDocumentVectorSpec(ctx, st)
				require.NoError(t, err)
				for _, purpose := range []string{"document_embedding", "query_embedding"} {
					consent, err := configuredDocumentVectorConsentSpec(spec, state, purpose)
					require.NoError(t, err)
					_, _, err = st.RecordDocumentVectorConsent(ctx, consent, time.Now())
					require.NoError(t, err)
				}
			}
			if initialModel != "" {
				file, err := os.CreateTemp(dir, "initial-*.toml")
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(file.Name(), []byte(content), 0o600))
				require.NoError(t, file.Close())
				require.NoError(t, runner.RunCLICommand(ctx, api.CLIRunRequest{Args: []string{"embeddings", "build", "--yes", "--full-rebuild", "--config", file.Name()}}, nil))
			}
			srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg}, Logger: state.logger, VectorCfg: cfg.Vector})
			defer func() { _ = srv.Shutdown(context.Background()) }()
			mcpCommand := &cobra.Command{Use: "serve"}
			addServeConfigFlags(mcpCommand)
			require.NoError(t, installServeMCP(ctx, mcpCommand, srv, state))
			serve := func(w http.ResponseWriter, request *http.Request) {
				request.Header.Set("Authorization", "Bearer owner")
				srv.Router().ServeHTTP(w, request)
			}
			mcpCall := func(method, params string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+params+`}`))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Accept", "application/json, text/event-stream")
				r.Header.Set("Mcp-Protocol-Version", "2025-11-25")
				w := httptest.NewRecorder()
				serve(w, r)
				return w
			}
			if initialModel == "" {
				assert.NotContains(t, mcpCall("tools/list", `{}`).Body.String(), `"find_similar_messages"`)
			}
			sched := scheduler.New(nil)
			defer func() { <-sched.Stop().Done() }()
			handle := startVectorInit(ctx, st, cfg.DatabaseDSN(), nil, srv, sched)
			require.True(t, handle.WaitTimeout(5*time.Second))
			if pendingKind == "people" {
				results, err := handle.vf.PersonSearchEngine.Search(ctx, "orchard", 5)
				require.NoError(t, err)
				require.Len(t, results, 1)
			}
			var oldDocumentID int64
			if strings.HasPrefix(pendingKind, "documents") {
				require.NoError(t, runScheduledDocumentVectorGeneration(jobctx.WithProgress(ctx), st, handle.vf, 100))
				generation, err := st.GetActiveDocumentVectorGeneration(ctx)
				require.NoError(t, err)
				require.NotNil(t, generation)
				oldDocumentID = generation.ID
			}
			defer func() { _ = srv.Shutdown(context.Background()); handle.CloseFeatures(state.logger) }()
			var oldGen vector.Generation
			var oldVector []float32
			if handle.vf != nil {
				oldGen, err = handle.vf.Backend.ActiveGeneration(ctx)
				require.NoError(t, err)
				oldVector, err = handle.vf.Backend.LoadVector(ctx, messageID)
				require.NoError(t, err)
			}
			_, _, err = testutil.CreateIndexedSourceMessage(st, "archive@example.test", "second", "Pear", "A pear tree is growing.")
			require.NoError(t, err)
			var matchingBuild vector.GenerationID
			if pendingKind == "matching-build" || pendingKind == "other-build" {
				fingerprint := handle.vf.Cfg.GenerationFingerprint()
				if pendingKind == "other-build" {
					fingerprint = "obsolete-build"
				}
				matchingBuild, err = handle.vf.Backend.CreateGeneration(ctx, initialModel, 768, fingerprint)
				require.NoError(t, err)
			}
			before := inputs.Load()
			installs := 0
			var runtimeContext context.Context
			controller := &localEmbeddingsController{state: state, store: st, dbPath: cfg.DatabaseDSN(), handle: handle, server: srv, scheduler: sched, runner: runner,
				refreshMCP: func(ctx context.Context, active *invocation) error {
					runtimeContext = ctx
					return installServeMCP(ctx, mcpCommand, srv, active)
				},
				inspect: func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
					return embedlocal.Status{DownloadBytes: 290000000, OllamaAvailable: useOllama}, nil
				},
				install: func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
					installs++
					if pendingKind == "documents-conflict" {
						require.NoError(t, os.WriteFile(path, []byte(content+"\n[web]\ntheme = \"dark\"\n"), 0o600))
					}
					yes, err := opts.Confirm(ctx, 290000000)
					require.True(t, yes)
					require.NoError(t, err)
					opts.Progress(5, 10)
					return embedlocal.Installation{DataDir: dir}, nil
				},
			}
			srv.SetLocalEmbeddings(controller)
			contextsBefore := len(runner.contexts)
			runner.failBuild = pendingKind == "build-failure"
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings/local", strings.NewReader(`{"use_builtin":`+strconv.FormatBool(!useOllama)+`}`))
			request.Header.Set("Content-Type", "application/json")
			serve(response, request)
			require.Equal(t, http.StatusAccepted, response.Code)
			require.Eventually(t, func() bool {
				status := httptest.NewRecorder()
				serve(status, httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/local", nil))
				var value api.LocalEmbeddingsStatus
				require.NoError(t, json.Unmarshal(status.Body.Bytes(), &value))
				return value.Phase == "idle" && !value.Running
			}, 10*time.Second, 100*time.Millisecond)
			status := httptest.NewRecorder()
			serve(status, httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/local", nil))
			if pendingKind == "documents-conflict" {
				var failure api.LocalEmbeddingsStatus
				require.NoError(t, json.Unmarshal(status.Body.Bytes(), &failure))
				require.NotEmpty(t, failure.Error)
				var consents int
				require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM document_vector_consents").Scan(&consents))
				assert.Equal(t, 2, consents)
				snapshot, err := config.ReadConfigFile(path)
				require.NoError(t, err)
				saved, err := config.LoadConfigFile(snapshot, dir)
				require.NoError(t, err)
				assert.Equal(t, "different-model", saved.Vector.Embeddings.Model)
				return
			}
			require.NotNil(t, runtimeContext)
			require.NoError(t, runtimeContext.Err())
			for _, buildContext := range runner.contexts[contextsBefore:] {
				_, bounded := buildContext.Deadline()
				assert.True(t, bounded)
				require.ErrorIs(t, buildContext.Err(), context.Canceled)
			}
			if pendingKind == "build-failure" {
				var failure api.LocalEmbeddingsStatus
				require.NoError(t, json.Unmarshal(status.Body.Bytes(), &failure))
				assert.Contains(t, failure.Error, "embedding scope matched 0 live messages; sync the scoped account(s) first")
				assert.LessOrEqual(t, len(failure.Error), len(cliSubprocessExitSentinel)+2+8192)
				// The saved provider survives a daemon restart, and the existing CLI
				// recovery command can finish the same index.
				persisted, err := config.ReadConfigFile(path)
				require.NoError(t, err)
				reopened, err := config.LoadConfigFile(persisted, dir)
				require.NoError(t, err)
				assert.True(t, reopened.Vector.Enabled)
				restartedState := *state
				restartedState.cfg = reopened
				_, err = buildCache(reopened.DatabaseDSN(), reopened.AnalyticsDir(), true)
				require.NoError(t, err)
				engine, err := query.NewDuckDBEngine(reopened.AnalyticsDir(), reopened.DatabaseDSN(), st.DB())
				require.NoError(t, err)
				defer func() { _ = engine.Close() }()
				restartedServer := api.NewServerWithOptions(api.ServerOptions{Config: reopened, Engine: engine, Store: &storeAPIAdapter{store: st, config: reopened}, Logger: state.logger, VectorCfg: reopened.Vector})
				restarted := startVectorInit(withInvocation(ctx, &restartedState), st, cfg.DatabaseDSN(), nil, restartedServer, sched)
				require.True(t, restarted.WaitTimeout(5*time.Second))
				coverage := httptest.NewRecorder()
				coverageRequest := httptest.NewRequest(http.MethodPost, "/api/v1/search/coverage", strings.NewReader(`{}`))
				coverageRequest.Header.Set("Content-Type", "application/json")
				restartedServer.Router().ServeHTTP(coverage, coverageRequest)
				require.Equal(t, http.StatusOK, coverage.Code, coverage.Body.String())
				assert.Contains(t, coverage.Body.String(), "build_index")
				require.NoError(t, restartedServer.Shutdown(ctx))
				restarted.CloseFeatures(state.logger)
				require.NoError(t, runner.RunCLICommand(ctx, api.CLIRunRequest{Args: []string{"embeddings", "build", "--yes", "--full-rebuild", "--config", path}}, nil))
			} else {
				assert.NotContains(t, status.Body.String(), `"error"`, status.Body.String())
			}
			if useOllama {
				require.Zero(t, installs)
			} else {
				require.Equal(t, 1, installs)
			}
			active, err := handle.vf.Backend.ActiveGeneration(ctx)
			require.NoError(t, err)
			assert.Equal(t, "nomic-embed-text", active.Model)
			if initialModel == "nomic-embed-text" && pendingKind != "matching-build" {
				assert.Equal(t, oldGen.ID, active.ID)
				after, err := handle.vf.Backend.LoadVector(ctx, messageID)
				require.NoError(t, err)
				assert.Equal(t, oldVector, after)
				assert.Equal(t, int32(1), inputs.Load()-before)
			} else {
				assert.NotEqual(t, oldGen.ID, active.ID)
			}
			if pendingKind == "matching-build" {
				assert.Equal(t, matchingBuild, active.ID)
			}
			if pendingKind == "other-build" {
				require.GreaterOrEqual(t, len(runner.calls), 3)
				assert.Equal(t, "retire", runner.calls[len(runner.calls)-2][1])
				assert.Contains(t, runner.calls[len(runner.calls)-1], "--full-rebuild")
			}
			search := httptest.NewRecorder()
			serve(search, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=orchard&mode=vector", nil))
			require.Equal(t, http.StatusOK, search.Code, search.Body.String())
			assert.Contains(t, search.Body.String(), "Orchard")
			if strings.HasPrefix(pendingKind, "people") {
				results, err := handle.vf.PersonSearchEngine.Search(ctx, "orchard", 5)
				if pendingKind == "people" {
					require.NoError(t, err)
					require.Len(t, results, 1)
				} else {
					require.ErrorIs(t, err, vector.ErrSemanticPersonEmbeddingConsentRequired)
				}
			}
			catalog := mcpCall("tools/list", `{}`)
			require.Equal(t, http.StatusOK, catalog.Code, catalog.Body.String())
			assert.Contains(t, catalog.Body.String(), `"find_similar_messages"`)
			semantic := mcpCall("tools/call", `{"name":"semantic_search_messages","arguments":{"query":"orchard","mode":"vector"}}`)
			require.Equal(t, http.StatusOK, semantic.Code, semantic.Body.String())
			assert.Contains(t, semantic.Body.String(), "Orchard")
			assert.NotContains(t, semantic.Body.String(), `"isError":true`)
			if strings.HasPrefix(pendingKind, "documents") {
				require.Eventually(t, func() bool {
					generation, err := st.GetActiveDocumentVectorGeneration(ctx)
					return err == nil && generation != nil && generation.ID != oldDocumentID
				}, 10*time.Second, 50*time.Millisecond)
				result := httptest.NewRecorder()
				serve(result, httptest.NewRequest(http.MethodGet, "/api/v1/documents/search?q=orchard&mode=semantic", nil))
				require.Equal(t, http.StatusOK, result.Code, result.Body.String())
				assert.Contains(t, result.Body.String(), "orchard.pdf")
				assert.Contains(t, result.Body.String(), "orchard document evidence")
			}
		})
	}
}

func TestLocalEmbeddingsSetupDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		controller := &localEmbeddingsController{handle: &vectorInitHandle{done: make(chan struct{})}}
		finished := make(chan error, 1)
		go func() { finished <- controller.Enable(context.Background(), false, func(api.LocalEmbeddingsStatus) {}) }()
		synctest.Wait()
		time.Sleep(24 * time.Hour)
		require.ErrorIs(<-finished, context.DeadlineExceeded)
	})
}

func TestLocalEmbeddingsDownloadFailureAndConfigConflictKeepSavedSettings(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(strconv.FormatBool(conflict), func(t *testing.T) {
			fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
			oldEndpoint := localEmbeddingEndpoint
			localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:9999/v1", nil }
			t.Cleanup(func() { localEmbeddingEndpoint = oldEndpoint })
			cfg := fixture.load(t)
			require.NoError(t, os.MkdirAll(filepath.Dir(cfg.DatabaseDSN()), 0o700))
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(t, err)
			defer func() { _ = st.Close() }()
			require.NoError(t, st.InitSchema())
			before, err := os.ReadFile(fixture.path)
			require.NoError(t, err)
			handle := &vectorInitHandle{done: make(chan struct{})}
			close(handle.done)
			controller := &localEmbeddingsController{state: &invocation{cfg: cfg, logger: testDiscardLogger()}, store: st, dbPath: cfg.DatabaseDSN(), handle: handle,
				inspect: func(context.Context, embedlocal.Options) (embedlocal.Status, error) { return embedlocal.Status{}, nil },
				install: func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
					if !conflict {
						return embedlocal.Installation{}, errors.New("connection interrupted")
					}
					require.NoError(t, os.WriteFile(fixture.path, append(slices.Clone(before), []byte("\n[web]\ntheme = \"dark\"\n")...), 0o600))
					return embedlocal.Installation{DataDir: fixture.dir}, nil
				},
			}
			err = controller.Enable(t.Context(), true, func(api.LocalEmbeddingsStatus) {})
			require.Error(t, err)
			after := fixture.load(t)
			assert.False(t, after.Vector.Enabled)
			if conflict {
				assert.Equal(t, "dark", after.Web.Theme)
			} else {
				actual, err := os.ReadFile(fixture.path)
				require.NoError(t, err)
				assert.Equal(t, before, actual)
			}
		})
	}
}

func TestLocalEmbeddingsPreservesPausedConfiguredProviders(t *testing.T) {
	for _, endpoint := range []string{"https://api.example.test/v1", "http://127.0.0.1:19000/v1"} {
		t.Run(endpoint, func(t *testing.T) {
			fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig+fmt.Sprintf(`
[vector]
enabled = false
[vector.embeddings]
endpoint = %q
model = "configured-model"
api_key_env = "EXAMPLE_EMBEDDING_KEY"
`, endpoint))
			cfg := fixture.load(t)
			before, err := os.ReadFile(fixture.path)
			require.NoError(t, err)
			handle := &vectorInitHandle{done: make(chan struct{})}
			close(handle.done)
			controller := &localEmbeddingsController{state: &invocation{cfg: cfg}, handle: handle,
				inspect: func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
					return embedlocal.Status{DownloadBytes: 290000000}, nil
				},
			}
			status, err := controller.Inspect(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "configured", status.Provider)
			for _, builtin := range []bool{false, true} {
				require.ErrorContains(t, controller.Enable(t.Context(), builtin, func(api.LocalEmbeddingsStatus) {}), "already uses a configured provider")
			}
			after, err := os.ReadFile(fixture.path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestLocalEmbeddingsReenablesPausedOllamaRecipe(t *testing.T) {
	for _, schedule := range []struct {
		name, toml, cron string
		afterSync        bool
	}{
		{name: "defaults", cron: setupEmbedCron, afterSync: true},
		{name: "cron disabled", toml: "cron = \"\"", afterSync: true},
		{name: "sync disabled", toml: "run_after_sync = false", cron: setupEmbedCron},
	} {
		t.Run(schedule.name, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/embeddings", r.URL.Path)
				var body struct {
					Model string   `json:"model"`
					Input []string `json:"input"`
				}
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) || !assert.Len(t, body.Input, 1) {
					http.Error(w, "invalid embedding request", http.StatusBadRequest)
					return
				}
				assert.Equal(t, "my-model", body.Model)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0,0,0]}]}`)
			}))
			defer endpoint.Close()
			fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig+fmt.Sprintf(`
[vector]
enabled = false
[vector.embeddings]
endpoint = %q
model = "my-model"
dimension = 4
document_prefix = "passage: "
query_prefix = "question: "
max_input_chars = 777
[chat]
server = %q
[vector.embed.schedule]
%s
`, endpoint.URL+"/v1", endpoint.URL, schedule.toml))
			cfg := fixture.load(t)
			before := cfg.Vector.Embeddings
			require.NoError(t, os.MkdirAll(filepath.Dir(cfg.DatabaseDSN()), 0o700))
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(t, err)
			defer func() { _ = st.Close() }()
			require.NoError(t, st.InitSchema())
			_, _, err = testutil.CreateIndexedSourceMessage(st, "archive@example.test", "first", "Orchard", "The orchard has apples.")
			require.NoError(t, err)
			state := &invocation{cfg: cfg, logger: testDiscardLogger()}
			ctx := withInvocation(t.Context(), state)
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: state.logger})
			defer func() { _ = server.Shutdown(context.Background()) }()
			handle := &vectorInitHandle{done: make(chan struct{})}
			close(handle.done)
			defer handle.CloseFeatures(state.logger)
			runner := &localBuildTestRunner{t: t, home: cfg.HomeDir}
			sched := scheduler.New(func(context.Context, string) error {
				_, _, err := testutil.CreateIndexedSourceMessage(st, "archive@example.test", "later", "Later orchard", "Another orchard message.")
				return err
			})
			defer func() { <-sched.Stop().Done() }()
			require.NoError(t, sched.AddAccount("archive@example.test", "0 0 * * *"))
			controller := &localEmbeddingsController{state: state, store: st, dbPath: cfg.DatabaseDSN(), handle: handle, server: server, scheduler: sched, runner: runner, inspect: func(context.Context, embedlocal.Options) (embedlocal.Status, error) { return embedlocal.Status{}, nil }}
			require.NoError(t, controller.Enable(ctx, false, func(api.LocalEmbeddingsStatus) {}))
			after := fixture.load(t)
			assert.True(t, after.Vector.Enabled)
			assert.Equal(t, before, after.Vector.Embeddings)
			assert.Equal(t, schedule.cron, after.Vector.Embed.Schedule.Cron)
			assert.Equal(t, schedule.afterSync, after.Vector.Embed.Schedule.RunAfterSync)
			require.NoError(t, sched.TriggerSync("archive@example.test", true))
			if schedule.afterSync {
				require.Eventually(t, func() bool {
					results, _, err := handle.vf.HybridEngine.Search(ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 10})
					return err == nil && len(results) == 2
				}, 10*time.Second, 50*time.Millisecond)
			}
		})
	}
}

func seedLocalSearchDocument(t *testing.T, st *store.Store, messageID int64) {
	t.Helper()
	require := require.New(t)
	ctx := t.Context()
	profile := store.DocumentExtractionProfile{ID: "local-search-profile", Fingerprint: strings.Repeat("a", 64), Provider: "synthetic", Endpoint: "https://documents.example.test/v1", Region: "local", Model: "extract-test", RetentionPosture: "standard", TrainingPosture: "opted-out", AllowedMediaTypes: []string{"application/pdf"}, PolicyJSON: []byte(`{"policy":1}`)}
	_, err := st.EnsureDocumentExtractionProfile(ctx, profile)
	require.NoError(err)
	require.NoError(st.RecordDocumentProviderConsent(ctx, store.DocumentProviderConsent{ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint, RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture}))
	_, err = st.DB().Exec(st.Rebind(`UPDATE document_index_state SET target_profile_id = ? WHERE singleton = 1`), profile.ID)
	require.NoError(err)
	hash := strings.Repeat("b", 64)
	require.NoError(st.UpsertAttachmentRecord(ctx, messageID, store.AttachmentWrite{Filename: "orchard.pdf", MIMEType: "application/pdf", Size: 128, StoragePath: hash[:2] + "/" + hash, ContentHash: hash, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "synthetic:document"}))
	var attachmentID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM attachments WHERE message_id = ?`), messageID).Scan(&attachmentID))
	_, eligible, err := st.ReconcileDocumentOccurrence(ctx, attachmentID, 1)
	require.NoError(err)
	require.True(eligible)
	claim, err := st.ClaimDocumentExtraction(ctx, store.DocumentExtractionClaimInput{ExtractionID: "local-search-extraction", ProfileID: profile.ID, CanonicalBlobHash: hash, ExtractionInputKey: "original", OccurrenceAttachmentID: attachmentID, OccurrenceMIMEType: "application/pdf", OccurrenceMessageType: "email", LeaseOwner: "local-search-test", LeaseUntil: time.Now().Add(time.Minute), LocalBytes: 128, SourceSequence: 1})
	require.NoError(err)
	text := "orchard document evidence"
	policy, err := docbankdocument.NewNormalizePolicy(10_000)
	require.NoError(err)
	normalized, err := docbankdocument.NormalizeDocument(docbankdocument.SourceDocument{Family: "pdf", UnitKind: "page", Units: []docbankdocument.SourceUnit{{Index: 0, Markdown: text}}}, policy)
	require.NoError(err)
	unit, chunk := normalized.Units[0], normalized.Chunks[0]
	require.NoError(st.PublishDocumentExtraction(ctx, store.DocumentExtractionPublication{ExtractionID: claim.ExtractionID, ProfileID: claim.ProfileID, CanonicalBlobHash: claim.CanonicalBlobHash, ExtractionInputKey: claim.ExtractionInputKey, OccurrenceAttachmentID: claim.OccurrenceAttachmentID, OccurrenceMIMEType: claim.OccurrenceMIMEType, OccurrenceMessageType: claim.OccurrenceMessageType, LeaseOwner: claim.LeaseOwner, LeaseFence: claim.LeaseFence, ReturnedModel: "extract-test", UnitsProcessed: 1, UploadCount: 1, RequestCount: 1, ProviderLatencyMS: 25, ManifestChecksum: normalized.Checksum, NormalizationVersion: policy.Identity().Version, DocumentFamily: "pdf", UnitKind: "page", Units: []store.DocumentPublishedUnit{{Index: 0, Kind: "page", Text: unit.Text, Checksum: unit.Checksum, CharCount: unit.CharCount}}, Chunks: []store.DocumentPublishedChunk{{Key: chunk.Key, Ordinal: chunk.Ordinal, Text: chunk.Text, FirstUnitIndex: 0, LastUnitIndex: 0, Checksum: chunk.Checksum, CharCount: chunk.CharCount, Spans: []store.DocumentPublishedSpan{{UnitIndex: chunk.Spans[0].UnitIndex, CharStart: chunk.Spans[0].CharStart, CharEnd: chunk.Spans[0].CharEnd}}}}}))
}
