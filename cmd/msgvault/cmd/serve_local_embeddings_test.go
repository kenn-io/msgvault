//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
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
	"go.kenn.io/kit/embedlocal"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/providercredentials"
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
	beforeBuild func()
}

func (r *localBuildTestRunner) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	r.calls = append(r.calls, slices.Clone(req.Args))
	r.contexts = append(r.contexts, ctx)
	args := req.Args[2:]
	path := invocationFromContext(ctx).cfg.ConfigFilePath()
	if index := slices.Index(args, "--config"); index >= 0 {
		path = args[index+1]
		args = args[:index]
	}
	snapshot, err := config.ReadConfigFile(path)
	if err != nil {
		return err
	}
	cfg, err := config.LoadConfigFile(snapshot, r.home)
	if err != nil {
		return err
	}
	if !assert.Equal(r.t, r.home, cfg.HomeDir) {
		return errors.New("unexpected test config home")
	}
	if r.beforeBuild != nil {
		r.beforeBuild()
	}
	state := *invocationFromContext(ctx)
	state.cfg = cfg
	oldYes, oldRebuild, oldRetire, oldBackstop := embedYes, embedFullRebuild, embeddingsRetireYes, embedBackstop
	defer func() {
		embedYes, embedFullRebuild, embeddingsRetireYes, embedBackstop = oldYes, oldRebuild, oldRetire, oldBackstop
	}()
	command := newEmbeddingsBuildCmd("build")
	command.RunE = func(cmd *cobra.Command, _ []string) error { return runEmbeddingsBuildLocal(cmd) }
	if req.Args[1] == "retire" {
		command = &cobra.Command{Use: "retire", RunE: runEmbeddingsRetire}
		command.Flags().BoolVar(&embeddingsRetireYes, "yes", false, "Confirm retirement")
	}
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs(args)
	if err := command.ExecuteContext(withInvocation(ctx, &state)); err != nil {
		return fmt.Errorf("build test index: %w", err)
	}
	return nil
}

type localActivationFixture struct {
	cfg           *config.Config
	store         *store.Store
	state         *invocation
	ctx           context.Context
	handle        *vectorInitHandle
	server        *api.Server
	scheduler     *scheduler.Scheduler
	controller    *localEmbeddingsController
	runner        *localBuildTestRunner
	onEmbedding   func([]string) error
	inputs        atomic.Int32
	installs      int
	ollamaTags    bool
	ollamaCatalog string
}

func writeFakeEmbeddingResponse(w http.ResponseWriter, inputs, dimension int) error {
	data := make([]map[string]any, inputs)
	for i := range data {
		values := make([]float32, dimension)
		values[0] = 1
		data[i] = map[string]any{"index": i, "embedding": values}
	}
	w.Header().Set("Content-Type", "application/json")
	return json.MarshalWrite(w, map[string]any{"data": data, "model": "test-model"})
}

func newLocalActivationFixture(t *testing.T, model string) *localActivationFixture {
	t.Helper()
	f := &localActivationFixture{ollamaTags: model != ""}
	dir := t.TempDir()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			if !f.ollamaTags {
				http.NotFound(w, r)
				return
			}
			models := []map[string]string{}
			if f.ollamaCatalog != "" {
				_, err := io.WriteString(w, f.ollamaCatalog)
				assert.NoError(t, err)
				return
			}
			ollamaModel := model
			if ollamaModel == "" {
				ollamaModel = "nomic-embed-text"
			}
			models = append(models, map[string]string{"name": ollamaModel + ":latest"})
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"models": models}))
			return
		}
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			http.Error(w, "invalid embedding request", http.StatusBadRequest)
			return
		}
		if f.onEmbedding != nil {
			if err := f.onEmbedding(body.Input); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		f.inputs.Add(int32(len(body.Input)))
		assert.NoError(t, writeFakeEmbeddingResponse(w, len(body.Input), 768))
	}))
	t.Cleanup(endpoint.Close)
	oldEndpoint, oldDir := localEmbeddingEndpoint, localEmbeddingsDir
	localEmbeddingEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
		if !opts.SkipOllama && !f.ollamaTags {
			return "", embedlocal.ErrNotFound
		}
		return endpoint.URL + "/v1", nil
	}
	localEmbeddingsDir = func() (string, error) { return filepath.Join(dir, "shared"), nil }
	t.Cleanup(func() { localEmbeddingEndpoint, localEmbeddingsDir = oldEndpoint, oldDir })
	content := "[data]\ndata_dir = " + strconv.Quote(dir) + "\n[vector]\nenabled = false\n"
	if model != "" {
		content = "[data]\ndata_dir = " + strconv.Quote(dir) + "\n[vector]\nenabled = true\n[vector.embeddings]\nmodel = " + strconv.Quote(model) + "\ndimension = 768\nendpoint = " + strconv.Quote(endpoint.URL+"/v1") + "\ndocument_prefix = \"document: \"\nquery_prefix = \"query: \"\nmax_input_chars = 1000\n"
	}
	content += "\n[chat]\nserver = " + strconv.Quote(endpoint.URL) + "\n"
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	snapshot, err := config.ReadConfigFile(path)
	require.NoError(t, err)
	f.cfg, err = config.LoadConfigFile(snapshot, dir)
	require.NoError(t, err)
	f.cfg.Server.APIKey = "owner"
	f.store, err = store.Open(f.cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.store.Close() })
	require.NoError(t, f.store.InitSchema())
	_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), f.store, "archive@example.test", "first", "Orchard", "The orchard has apples.")
	require.NoError(t, err)
	f.state = &invocation{cfg: f.cfg, logger: testDiscardLogger()}
	f.ctx = withInvocation(t.Context(), f.state)
	f.runner = &localBuildTestRunner{t: t, home: dir}
	f.scheduler = scheduler.New(nil)
	gate := api.NewSerialOperationGate()
	f.server = api.NewServerWithOptions(api.ServerOptions{OperationGate: gate, Config: f.cfg, Store: &storeAPIAdapter{store: f.store, config: f.cfg}, OperationHistoryReader: &storeAPIAdapter{store: f.store, config: f.cfg}, Logger: f.state.logger, VectorCfg: f.cfg.Vector})
	t.Cleanup(func() {
		<-f.scheduler.Stop().Done()
		_ = f.server.Shutdown(context.WithoutCancel(t.Context()))
		if f.handle != nil {
			f.handle.CloseFeatures(f.state.logger)
		}
	})
	f.controller = &localEmbeddingsController{gate: gate, state: f.state, store: f.store, dbPath: f.cfg.DatabaseDSN(), server: f.server, scheduler: f.scheduler, runner: f.runner,
		inspect: func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
			return embedlocal.Status{DownloadBytes: 290000000}, nil
		},
		install: func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
			f.installs++
			return embedlocal.Installation{DataDir: dir}, nil
		},
	}
	return f
}

func (f *localActivationFixture) start(t *testing.T) {
	t.Helper()
	if f.cfg.Vector.Enabled {
		require.NoError(t, f.runner.RunCLICommand(f.ctx, api.CLIRunRequest{Args: []string{"embeddings", "build", "--yes", "--full-rebuild", "--config", f.cfg.ConfigFilePath()}}, nil))
	}
	f.handle = startVectorInit(f.ctx, f.store, f.cfg.DatabaseDSN(), nil, f.server, f.scheduler)
	require.True(t, f.handle.WaitTimeout(5*time.Second))
	f.controller.handle = f.handle
}

func TestLocalEmbeddingsRestoresSavedBuiltinWithOllamaAvailable(t *testing.T) {
	for _, startupFailed := range []bool{false, true} {
		t.Run(fmt.Sprintf("startup_failed=%t", startupFailed), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newLocalActivationFixture(t, "nomic-embed-text")
			f.controller.inspect = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
				return embedlocal.Status{OllamaAvailable: true, DownloadBytes: 290000000}, nil
			}
			snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			_, err = config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"vector"}, Values: map[string]any{"enabled": startupFailed}}, {Path: []string{"vector", "embeddings"}, Values: map[string]any{"local_only": true, "endpoint": ""}}})
			require.NoError(err)
			f.cfg.Vector.Enabled = startupFailed
			f.cfg.Vector.Embeddings.LocalOnly = true
			f.cfg.Vector.Embeddings.Endpoint = ""
			fixtureEndpoint := localEmbeddingEndpoint
			localEmbeddingEndpoint = embedlocal.Endpoint
			f.handle = startVectorInit(f.ctx, f.store, f.cfg.DatabaseDSN(), nil, f.server, f.scheduler)
			require.True(f.handle.WaitTimeout(5 * time.Second))
			f.controller.handle = f.handle

			status, err := f.controller.Inspect(f.ctx)
			require.NoError(err)
			require.Equal("builtin", status.Provider)
			require.False(status.Enabled, "missing model files must leave Turn on available")
			assert.Empty(status.Error)
			localEmbeddingEndpoint = fixtureEndpoint
			require.NoError(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
			require.Equal(1, f.installs)
			status, err = f.controller.Inspect(f.ctx)
			require.NoError(err)
			assert.Empty(status.Error)
			require.True(status.Enabled)
			require.Equal("builtin", status.Provider)
			messages, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 5})
			require.NoError(err)
			require.Len(messages, 1)
		})
	}
}

func TestLocalEmbeddingsPreservesConfiguredLocalProvider(t *testing.T) {
	for _, tc := range []struct {
		name, catalog, wantProvider, wantError string
		savedOnly, savedEnabled, savedBuiltin  bool
	}{
		{name: "local without Ollama API", wantProvider: "configured"},
		{name: "enabled Ollama", catalog: `{"models":[]}`, wantProvider: "ollama"},
		{name: "local without catalog", catalog: `{}`, wantProvider: "configured"},
		{name: "saved disabled builtin model", savedOnly: true, savedBuiltin: true, wantProvider: "builtin", wantError: "vector.embeddings.local_only: requires openai format, nomic-embed-text, and 768 dimensions"},
		{name: "saved remote after startup", savedOnly: true, wantProvider: "configured"},
		{name: "saved enabled remote after startup", savedOnly: true, savedEnabled: true, wantProvider: "configured"},
		{name: "saved enabled Ollama after startup", catalog: `{"models":[]}`, savedOnly: true, savedEnabled: true, wantProvider: "configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			model := "custom-model"
			if tc.savedOnly {
				model = ""
			}
			if tc.savedBuiltin {
				model = "nomic-embed-text"
			}
			f := newLocalActivationFixture(t, model)
			f.ollamaTags = tc.catalog != ""
			f.ollamaCatalog = tc.catalog
			if tc.savedBuiltin {
				f.useBuiltin(t)
			}
			if !tc.savedBuiltin {
				f.start(t)
			}
			var before []byte
			if tc.savedOnly {
				snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
				require.NoError(err)
				endpoint := "https://api.example.test/v1"
				if tc.catalog != "" {
					endpoint = f.cfg.Chat.Server + "/v1"
				}
				embeddings := map[string]any{"endpoint": endpoint, "model": "custom-model", "dimension": 768}
				if tc.savedBuiltin {
					embeddings = map[string]any{"model": "custom-model"}
				}
				written, err := config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"vector"}, Values: map[string]any{"enabled": tc.savedEnabled}}, {Path: []string{"vector", "embeddings"}, Values: embeddings}})
				require.NoError(err)
				before = written.Content
				if tc.savedBuiltin {
					saved, err := f.cfg.ReloadConfigFile(written)
					require.NoError(err)
					*f.cfg = *saved
					f.start(t)
				}
			}
			status, err := f.controller.Inspect(f.ctx)
			require.NoError(err)
			if tc.savedOnly {
				request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/search?q=orchard&mode=vector", nil)
				request.Header.Set("Authorization", "Bearer owner")
				response := httptest.NewRecorder()
				f.server.Router().ServeHTTP(response, request)
				require.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
				require.False(status.Enabled, "saved settings cannot enable search before runtime publication")
				err = f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {})
				require.Error(err)
				if tc.wantError != "" {
					require.ErrorContains(err, tc.wantError)
				}
				after, err := os.ReadFile(f.cfg.ConfigFilePath())
				require.NoError(err)
				assert.Equal(before, after)
			} else {
				assert.True(status.Enabled)
			}
			assert.Equal(tc.wantProvider, status.Provider)
		})
	}
}

func TestLocalEmbeddingsReusesOrReplacesMessageGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, building string
		reuse          bool
		changedRecipe  bool
	}{
		{name: "matching active", reuse: true},
		{name: "different active", changedRecipe: true},
		{name: "matching build", building: "matching"},
		{name: "different build", building: "obsolete-build", changedRecipe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLocalActivationFixture(t, "nomic-embed-text")
			f.useBuiltin(t)
			f.start(t)
			old, err := f.handle.vf.Backend.ActiveGeneration(f.ctx)
			require.NoError(t, err)
			if tc.reuse {
				// Leave a pending message below the saved watermark.
				_, err = f.store.DB().ExecContext(t.Context(), "UPDATE messages SET embed_gen = NULL WHERE embed_gen = ?", old.ID)
				require.NoError(t, err)
			}
			if tc.changedRecipe {
				snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
				require.NoError(t, err)
				_, err = config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"vector", "embeddings"}, Values: map[string]any{"document_prefix": "updated: "}}})
				require.NoError(t, err)
			}
			_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), f.store, "archive@example.test", "second", "Pear", "A pear tree is growing.")
			require.NoError(t, err)
			var building vector.GenerationID
			if tc.building != "" {
				fingerprint := tc.building
				if tc.building == "matching" {
					fingerprint = f.handle.vf.Cfg.GenerationFingerprint()
				}
				building, err = f.handle.vf.Backend.CreateGeneration(f.ctx, "nomic-embed-text", 768, fingerprint)
				require.NoError(t, err)
			}
			before := f.inputs.Load()
			require.NoError(t, f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
			active, err := f.handle.vf.Backend.ActiveGeneration(f.ctx)
			require.NoError(t, err)
			if tc.reuse {
				assert.Equal(t, old.ID, active.ID)
				assert.Equal(t, int32(2), f.inputs.Load()-before)
			} else {
				assert.NotEqual(t, old.ID, active.ID)
			}
			if tc.building == "matching" {
				assert.Equal(t, building, active.ID)
			}
			if tc.building == "obsolete-build" {
				require.GreaterOrEqual(t, len(f.runner.calls), 3)
				assert.Equal(t, "retire", f.runner.calls[len(f.runner.calls)-2][1])
				assert.Contains(t, f.runner.calls[len(f.runner.calls)-1], "--full-rebuild")
			}
		})
	}
}

func TestLocalEmbeddingsCLIUsesPublishedRecipe(t *testing.T) {
	for _, operation := range []string{"activation plan", "optimize"} {
		t.Run(operation, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := newLocalActivationFixture(t, "")
			f.start(t)
			require.NoError(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
			path, body := "/api/v1/cli/run", `{"args":["embeddings","optimize","--drop"]}`
			var generation vector.GenerationID
			if operation == "activation plan" {
				vf := f.handle.vf
				var err error
				generation, err = vf.Backend.CreateGeneration(f.ctx, vf.Cfg.Embeddings.Model, vf.Cfg.Embeddings.Dimension, vf.Cfg.GenerationFingerprint())
				require.NoError(err)
				result, err := vf.Runner.RunOnce(f.ctx, generation, testCLIEmbeddingPassScope())
				require.NoError(err)
				require.Equal(1, result.Succeeded)
				path, body = "/api/v1/cli/embeddings/plan", fmt.Sprintf(`{"operation":"activate","generation_id":%d}`, generation)
			}
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer owner")
			request.Header.Set("Content-Type", "application/json")
			request.RemoteAddr = "127.0.0.1:1234"
			response := httptest.NewRecorder()
			f.server.Router().ServeHTTP(response, request)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			if operation == "activation plan" {
				var plan api.CLIEmbeddingsPlanResponse
				require.NoError(json.Unmarshal(response.Body.Bytes(), &plan))
				assert.True(plan.NeedsConfirmation)
				assert.Contains(plan.Prompt, fmt.Sprintf("Activate generation %d", generation))
			} else {
				assert.Contains(response.Body.String(), "exact vectors retained")
			}
		})
	}
}

func TestLocalEmbeddingsCommandError(t *testing.T) {
	for _, tc := range []struct {
		name, stderr, want string
		err                error
		wantCanceled       bool
	}{
		{name: "canceled without output", err: context.Canceled, wantCanceled: true, want: "context canceled"},
		{name: "plain failure", err: errors.New(cliSubprocessExitSentinel), stderr: "index progress\nError: model unavailable\n", want: "model unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			err := localEmbeddingsCommandError(t.Context(), tc.err, tc.stderr)
			require.EqualError(err, tc.want)
			require.Equal(tc.wantCanceled, errors.Is(err, context.Canceled))
		})
	}
}

func TestLocalEmbeddingsSetupDeadlineCancelsWaitingInitialization(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		controller := &localEmbeddingsController{handle: &vectorInitHandle{done: make(chan struct{})}}
		finished := make(chan error, 1)
		go func() { finished <- controller.Enable(t.Context(), func(api.LocalEmbeddingsStatus) {}) }()
		synctest.Wait()
		time.Sleep(24 * time.Hour)
		require.ErrorIs(<-finished, context.DeadlineExceeded)
	})
}

func TestLocalEmbeddingsSetupFailureKeepsSavedSettings(t *testing.T) {
	for _, tc := range []struct {
		name, wantError, suffix, wantTheme string
		wantConflict                       bool
		arrange                            func(*testing.T, *localActivationFixture, []byte)
	}{
		{
			name: "download", wantError: "connection interrupted", wantTheme: "system",
			arrange: func(_ *testing.T, f *localActivationFixture, _ []byte) {
				f.controller.install = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
					return embedlocal.Installation{}, errors.New("connection interrupted")
				}
			},
		},
		{
			name: "config conflict", wantError: "Settings changed during setup. Turn on to retry.",
			wantConflict: true, suffix: "\n[web]\ntheme = \"dark\"\n", wantTheme: "dark",
			arrange: func(t *testing.T, f *localActivationFixture, expected []byte) {
				t.Helper()
				install := f.controller.install
				f.controller.install = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
					require.NoError(t, os.WriteFile(f.cfg.ConfigFilePath(), expected, 0o600))
					return install(ctx, opts)
				}
			},
		},
		{
			name: "endpoint gone", wantError: "local server unavailable", wantTheme: "system",
			arrange: func(_ *testing.T, f *localActivationFixture, _ []byte) {
				endpoint := localEmbeddingEndpoint
				f.controller.install = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
					// Preparation resolves once; runtime initialization then finds its server gone.
					calls := 0
					localEmbeddingEndpoint = func(ctx context.Context, opts embedlocal.Options) (string, error) {
						calls++
						if calls > 1 {
							return "", errors.New("local server unavailable")
						}
						return endpoint(ctx, opts)
					}
					return embedlocal.Installation{}, nil
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VOYAGE_API_KEY", "synthetic-unused-key")
			require := require.New(t)
			f := newLocalActivationFixture(t, "")
			f.start(t)
			expected, err := os.ReadFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			expected = append(expected, []byte(tc.suffix)...)
			install, endpoint := f.controller.install, localEmbeddingEndpoint
			tc.arrange(t, f, expected)
			err = f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {})
			require.Nil(f.handle.vf)
			require.ErrorContains(err, tc.wantError)
			require.Equal(tc.wantConflict, errors.Is(err, config.ErrConfigConflict))
			actual, err := os.ReadFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			require.Equal(expected, actual)
			f.controller.install, localEmbeddingEndpoint = install, endpoint
			require.NoError(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
			snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			loaded, err := f.cfg.ReloadConfigFile(snapshot)
			require.NoError(err)
			require.Equal(tc.wantTheme, loaded.Web.Theme)
			require.True(loaded.Vector.Embeddings.LocalOnly)
		})
	}
}

func (f *localActivationFixture) useBuiltin(t *testing.T) {
	t.Helper()
	snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
	require.NoError(t, err)
	_, err = config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"vector", "embeddings"}, Values: map[string]any{"local_only": true, "endpoint": ""}}})
	require.NoError(t, err)
	f.cfg.Vector.Embeddings.LocalOnly = true
	f.cfg.Vector.Embeddings.Endpoint = ""
}

func TestLocalEmbeddingsPublishesScheduledBuildWithoutBlockingSyncDuringInstall(t *testing.T) {
	f := newLocalActivationFixture(t, "")
	f.start(t)
	gate, ok := f.controller.gate.(*api.SerialOperationGate)
	require.True(t, ok)
	install := f.controller.install
	f.controller.install = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		_, _, held := gate.Holder()
		assert.False(t, held, "installation leaves the archive available to sync")
		release, ok := gate.BeginLabeledWorkContext(ctx, "scheduled sync")
		require.True(t, ok)
		release()
		return install(ctx, opts)
	}
	f.runner.beforeBuild = func() {
		label, _, held := gate.Holder()
		assert.True(t, held)
		assert.Equal(t, "msgvault embeddings build", label)
	}
	require.NoError(t, f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
	_, _, err := testutil.CreateIndexedSourceMessage(t.Context(), f.store, "archive@example.test", "later", "Later orchard", "Another orchard message.")
	require.NoError(t, err)
	require.NoError(t, f.scheduler.TriggerJob(jobctx.ScheduledEmbeddingLabel))
	results, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 10})
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

func TestLocalEmbeddingsActivationAppliesSavedTextSettings(t *testing.T) {
	for _, setting := range []string{"preprocessing", "account scope"} {
		t.Run(setting, func(t *testing.T) {
			require := require.New(t)
			f := newLocalActivationFixture(t, "")
			f.start(t)
			var wantID int64
			require.NoError(f.store.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&wantID))
			edit := config.TableEdit{Path: []string{"vector", "preprocess"}, Values: map[string]any{"strip_quotes": false}}
			if setting == "account scope" {
				_, _, err := testutil.CreateIndexedSourceMessage(t.Context(), f.store, "excluded@example.test", "excluded", "Excluded orchard", "Another orchard message.")
				require.NoError(err)
				edit = config.TableEdit{Path: []string{"vector", "embed", "scope"}, Values: map[string]any{"accounts": []string{"archive@example.test"}}}
			}
			snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			_, err = config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{edit})
			require.NoError(err)
			require.NoError(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
			results, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 10})
			require.NoError(err, "activation must publish the same text settings used by its index build")
			require.Len(results, 1)
			assert.Equal(t, wantID, results[0].MessageID)
		})
	}
}

func TestLocalEmbeddingsFirstOllamaActivationRetriesAfterCredentialRepair(t *testing.T) {
	f := newLocalActivationFixture(t, "")
	f.ollamaTags = true
	f.start(t)
	f.controller.inspect = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		return embedlocal.Status{OllamaAvailable: true}, nil
	}
	status, err := f.controller.Inspect(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, "ollama", status.Provider)
	assert.False(t, status.Enabled)
	before, err := os.ReadFile(f.cfg.ConfigFilePath())
	require.NoError(t, err)
	credentials, err := providercredentials.Read(f.cfg.TokensDir())
	require.NoError(t, err)
	credentials, err = providercredentials.Put(f.cfg.TokensDir(), credentials.ETag, providercredentials.VectorEmbeddingsID, "https://previous.example.test/v1", "synthetic-previous-key")
	require.NoError(t, err)
	err = f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {})
	require.ErrorIs(t, err, providercredentials.ErrOriginMismatch)
	require.Nil(t, f.handle.vf)
	snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
	require.NoError(t, err)
	require.Equal(t, before, snapshot.Content)
	_, err = providercredentials.Delete(f.cfg.TokensDir(), credentials.ETag, providercredentials.VectorEmbeddingsID)
	require.NoError(t, err)
	require.NoError(t, f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
	assert.Zero(t, f.installs)
	snapshot, err = config.ReadConfigFile(f.cfg.ConfigFilePath())
	require.NoError(t, err)
	saved, err := f.cfg.ReloadConfigFile(snapshot)
	require.NoError(t, err)
	assert.False(t, saved.Vector.Embeddings.LocalOnly)
	assert.Equal(t, f.cfg.Chat.Server+"/v1", saved.Vector.Embeddings.Endpoint)
	assert.Len(t, f.runner.calls, 1)
	results, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 5})
	require.NoError(t, err)
	assert.Len(t, results, 1)
}
