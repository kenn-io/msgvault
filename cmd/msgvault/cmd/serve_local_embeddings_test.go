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
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedlocal"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
)

type localBuildTestRunner struct {
	t     *testing.T
	home  string
	calls [][]string
}

func (r *localBuildTestRunner) RunCLICommand(ctx context.Context, req api.CLIRunRequest, _ func(api.CLIRunEvent) error) error {
	r.calls = append(r.calls, slices.Clone(req.Args))
	index := slices.Index(req.Args, "--config")
	require.Positive(r.t, index)
	snapshot, err := config.ReadConfigFile(req.Args[index+1])
	require.NoError(r.t, err)
	cfg, err := config.LoadConfigFile(snapshot, r.home)
	require.NoError(r.t, err)
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
	for _, initialModel := range []string{"", "nomic-embed-text", "different-model", "available-ollama", "matching-build", "other-build"} {
		t.Run("initial="+initialModel, func(t *testing.T) {
			pendingKind := initialModel
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
			path := filepath.Join(dir, "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			snapshot, err := config.ReadConfigFile(path)
			require.NoError(t, err)
			cfg, err := config.LoadConfigFile(snapshot, dir)
			require.NoError(t, err)
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(t, err)
			defer func() { _ = st.Close() }()
			require.NoError(t, st.InitSchema())
			_, messageID, err := testutil.CreateIndexedSourceMessage(st, "archive@example.test", "first", "Orchard", "The orchard has apples.")
			require.NoError(t, err)
			state := &invocation{cfg: cfg, logger: slog.New(slog.DiscardHandler)}
			runner := &localBuildTestRunner{t: t, home: dir}
			ctx := withInvocation(t.Context(), state)
			if initialModel != "" {
				file, err := os.CreateTemp(dir, "initial-*.toml")
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(file.Name(), []byte(content), 0o600))
				require.NoError(t, file.Close())
				require.NoError(t, runner.RunCLICommand(ctx, api.CLIRunRequest{Args: []string{"embeddings", "build", "--yes", "--full-rebuild", "--config", file.Name()}}, nil))
			}
			srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg}, Logger: state.logger, VectorCfg: cfg.Vector})
			defer func() { _ = srv.Shutdown(context.Background()) }()
			sched := scheduler.New(nil)
			handle := startVectorInit(ctx, st, cfg.DatabaseDSN(), nil, srv, sched)
			require.True(t, handle.WaitTimeout(5*time.Second))
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
			controller := &localEmbeddingsController{state: state, store: st, dbPath: cfg.DatabaseDSN(), handle: handle, server: srv, scheduler: sched, runner: runner,
				inspect: func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
					return embedlocal.Status{DownloadBytes: 290000000, OllamaAvailable: useOllama}, nil
				},
				install: func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
					installs++
					yes, err := opts.Confirm(ctx, 290000000)
					require.True(t, yes)
					require.NoError(t, err)
					opts.Progress(5, 10)
					return embedlocal.Installation{DataDir: dir}, nil
				},
			}
			srv.SetLocalEmbeddings(controller)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings/local", strings.NewReader(`{"use_builtin":`+strconv.FormatBool(!useOllama)+`}`))
			request.Header.Set("Content-Type", "application/json")
			srv.Router().ServeHTTP(response, request)
			require.Equal(t, http.StatusAccepted, response.Code)
			require.Eventually(t, func() bool {
				status := httptest.NewRecorder()
				srv.Router().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/local", nil))
				var value api.LocalEmbeddingsStatus
				require.NoError(t, json.Unmarshal(status.Body.Bytes(), &value))
				return value.Phase == "idle"
			}, 10*time.Second, 100*time.Millisecond)
			status := httptest.NewRecorder()
			srv.Router().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/local", nil))
			assert.NotContains(t, status.Body.String(), `"error"`, status.Body.String())
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
			srv.Router().ServeHTTP(search, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=orchard&mode=vector", nil))
			require.Equal(t, http.StatusOK, search.Code, search.Body.String())
			assert.Contains(t, search.Body.String(), "Orchard")
		})
	}
}

func TestLocalEmbeddingsDownloadFailureAndConfigConflictKeepSavedSettings(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(strconv.FormatBool(conflict), func(t *testing.T) {
			fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
			oldEndpoint := localEmbeddingEndpoint
			localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:9999/v1", nil }
			t.Cleanup(func() { localEmbeddingEndpoint = oldEndpoint })
			cfg := fixture.load(t)
			before, err := os.ReadFile(fixture.path)
			require.NoError(t, err)
			handle := &vectorInitHandle{done: make(chan struct{})}
			close(handle.done)
			controller := &localEmbeddingsController{state: &invocation{cfg: cfg}, handle: handle,
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
	for _, endpoint := range []string{"https://api.example.test/v1", "http://localhost:11434/v1"} {
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
			require.NoError(t, controller.Enable(t.Context(), false, func(api.LocalEmbeddingsStatus) {}))
			after, err := os.ReadFile(fixture.path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}
