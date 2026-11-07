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
	oldYes, oldRebuild, oldRetire := embedYes, embedFullRebuild, embeddingsRetireYes
	defer func() { embedYes, embedFullRebuild, embeddingsRetireYes = oldYes, oldRebuild, oldRetire }()
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
		install: func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
			f.installs++
			_, _, held := gate.Holder()
			assert.False(t, held, "installation leaves the archive available to sync")
			release, ok := gate.BeginLabeledWorkContext(ctx, "scheduled sync")
			if !assert.True(t, ok) {
				return embedlocal.Installation{}, errors.New("test sync could not acquire archive")
			}
			release()
			yes, err := opts.Confirm(ctx, 290000000)
			if err != nil {
				return embedlocal.Installation{}, fmt.Errorf("confirm local model download: %w", err)
			}
			assert.True(t, yes)
			opts.Progress(5, 10)
			return embedlocal.Installation{DataDir: dir}, nil
		},
	}
	f.runner.beforeBuild = func() {
		label, _, held := gate.Holder()
		assert.True(t, held)
		assert.Equal(t, "msgvault embeddings build", label)
	}
	return f
}

func (f *localActivationFixture) start(t *testing.T) {
	t.Helper()
	if f.cfg.Vector.Enabled {
		hook := f.runner.beforeBuild
		f.runner.beforeBuild = nil
		require.NoError(t, f.runner.RunCLICommand(f.ctx, api.CLIRunRequest{Args: []string{"embeddings", "build", "--yes", "--full-rebuild", "--config", f.cfg.ConfigFilePath()}}, nil))
		f.runner.beforeBuild = hook
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
			var startupError string
			if startupFailed {
				startup, detail := f.server.VectorStatus()
				require.Equal(api.VectorStatusError, startup)
				require.Contains(detail, "local embeddings need setup")
				startupError = "Local model files need setup. Turn on to retry."
			}
			status, err := f.controller.Inspect(f.ctx)
			require.NoError(err)
			require.Equal("builtin", status.Provider)
			require.False(status.Enabled, "missing model files must leave Turn on available")
			assert.Equal(startupError, status.Error)
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
		name, catalog, wantProvider string
		pausedRemote, savedOnly     bool
	}{
		{name: "local without Ollama API", wantProvider: "configured"},
		{name: "enabled Ollama", catalog: `{"models":[]}`, wantProvider: "ollama"},
		{name: "local without catalog", catalog: `{}`, wantProvider: "configured"},
		{name: "local with invalid catalog", catalog: `invalid json`, wantProvider: "configured"},
		{name: "paused remote", pausedRemote: true, wantProvider: "configured"},
		{name: "saved remote after startup", savedOnly: true, wantProvider: "configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			model := "custom-model"
			if tc.savedOnly {
				model = ""
			}
			f := newLocalActivationFixture(t, model)
			f.ollamaTags = tc.catalog != ""
			f.ollamaCatalog = tc.catalog
			before, err := os.ReadFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			if tc.pausedRemote {
				before = []byte(strings.Replace(string(before), "enabled = true", "enabled = false", 1))
				before = []byte(strings.Replace(string(before), f.cfg.Vector.Embeddings.Endpoint, "https://api.example.test/v1", 1))
				require.NoError(os.WriteFile(f.cfg.ConfigFilePath(), before, 0o600))
				f.cfg.Vector.Enabled = false
				f.cfg.Vector.Embeddings.Endpoint = "https://api.example.test/v1"
			}
			f.start(t)
			if tc.savedOnly {
				snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
				require.NoError(err)
				written, err := config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"vector", "embeddings"}, Values: map[string]any{"endpoint": "https://api.example.test/v1"}}})
				require.NoError(err)
				before = written.Content
			}
			status, err := f.controller.Inspect(f.ctx)
			require.NoError(err)
			assert.Equal(tc.wantProvider, status.Provider)
			require.ErrorContains(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}), "already uses a configured provider")
			after, err := os.ReadFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			assert.Equal(before, after)
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
			if tc.changedRecipe {
				snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
				require.NoError(t, err)
				_, err = config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"vector", "embeddings"}, Values: map[string]any{"document_prefix": "updated: "}}})
				require.NoError(t, err)
			}
			var id int64
			require.NoError(t, f.store.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
			oldVector, err := f.handle.vf.Backend.LoadVector(f.ctx, id)
			require.NoError(t, err)
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
				after, err := f.handle.vf.Backend.LoadVector(f.ctx, id)
				require.NoError(t, err)
				assert.Equal(t, oldVector, after)
				assert.Equal(t, int32(1), f.inputs.Load()-before)
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

func TestLocalEmbeddingsActivationPlannerUsesPublishedRecipe(t *testing.T) {
	require := require.New(t)
	f := newLocalActivationFixture(t, "")
	f.start(t)
	require.NoError(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
	vf := f.handle.vf
	generation, err := vf.Backend.CreateGeneration(f.ctx, vf.Cfg.Embeddings.Model, vf.Cfg.Embeddings.Dimension, vf.Cfg.GenerationFingerprint())
	require.NoError(err)
	result, err := vf.Runner.RunOnce(f.ctx, generation, testCLIEmbeddingPassScope())
	require.NoError(err)
	require.Equal(1, result.Succeeded)

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/cli/embeddings/plan", strings.NewReader(fmt.Sprintf(`{"operation":"activate","generation_id":%d}`, generation)))
	request.Header.Set("Authorization", "Bearer owner")
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	f.server.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var plan api.CLIEmbeddingsPlanResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &plan))
	require.True(plan.NeedsConfirmation)
	require.Contains(plan.Prompt, fmt.Sprintf("Activate generation %d", generation))
}

func TestLocalEmbeddingsCommandError(t *testing.T) {
	for _, tc := range []struct {
		name, stderr, want string
		err                error
	}{
		{name: "canceled without output", err: context.Canceled, want: "context canceled"},
		{name: "canceled after progress", err: errors.New(cliSubprocessExitSentinel), stderr: strings.Repeat("index progress\n", 1024) + "Error: embedding build interrupted: context canceled\n", want: "embedding build interrupted: context canceled"},
		{name: "plain failure", err: errors.New(cliSubprocessExitSentinel), stderr: "index progress\nError: model unavailable\n", want: "model unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			err := localEmbeddingsCommandError(t.Context(), tc.err, tc.stderr)
			require.EqualError(err, tc.want)
			if errors.Is(tc.err, context.Canceled) {
				require.ErrorIs(err, context.Canceled)
			}
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
	for _, failure := range []string{"download", "config conflict", "endpoint gone"} {
		t.Run(failure, func(t *testing.T) {
			require := require.New(t)
			f := newLocalActivationFixture(t, "")
			f.start(t)
			expected, err := os.ReadFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			install, endpoint := f.controller.install, localEmbeddingEndpoint
			f.controller.install = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
				switch failure {
				case "download":
					return embedlocal.Installation{}, errors.New("connection interrupted")
				case "endpoint gone":
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
				default:
					expected = append(expected, []byte("\n[web]\ntheme = \"dark\"\n")...)
					require.NoError(os.WriteFile(f.cfg.ConfigFilePath(), expected, 0o600))
					return install(ctx, opts)
				}
			}
			err = f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {})
			require.Nil(f.handle.vf)
			switch failure {
			case "download":
				require.ErrorContains(err, "connection interrupted")
			case "endpoint gone":
				require.ErrorContains(err, "local server unavailable")
			default:
				require.ErrorIs(err, config.ErrConfigConflict)
				require.EqualError(err, "Settings changed during setup. Turn on to retry.")
			}
			actual, err := os.ReadFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			require.Equal(expected, actual)
			f.controller.install, localEmbeddingEndpoint = install, endpoint
			require.NoError(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
			snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			loaded, err := f.cfg.ReloadConfigFile(snapshot)
			require.NoError(err)
			if failure == "config conflict" {
				require.Equal("dark", loaded.Web.Theme)
			}
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

func TestLocalEmbeddingsPublishesScheduledBuild(t *testing.T) {
	t.Setenv("VOYAGE_API_KEY", "synthetic-unused-key")
	f := newLocalActivationFixture(t, "")
	f.start(t)
	require.NoError(t, f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
	assert.True(t, f.handle.vf.Cfg.Embeddings.LocalOnly)
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

func TestLocalEmbeddingsOptimizeAfterActivation(t *testing.T) {
	f := newLocalActivationFixture(t, "")
	f.start(t)
	require.NoError(t, f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/cli/run", strings.NewReader(`{"args":["embeddings","optimize","--drop"]}`))
	request.Header.Set("Authorization", "Bearer owner")
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	f.server.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "exact vectors retained")
}

func TestLocalEmbeddingsBuildIndexResumesUnfinishedInitialBuild(t *testing.T) {
	for _, provider := range []string{"ollama", "builtin after restart"} {
		t.Run(provider, func(t *testing.T) {
			require := require.New(t)
			f := newLocalActivationFixture(t, "")
			f.ollamaTags = provider == "ollama"
			snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			_, err = config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"vector", "embed", "schedule"}, Values: map[string]any{"cron": "", "run_after_sync": false}}})
			require.NoError(err)
			f.start(t)
			f.onEmbedding = func([]string) error { return errors.New("provider temporarily unavailable") }
			require.Error(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
			building, err := f.handle.vf.Backend.BuildingGeneration(f.ctx)
			require.NoError(err)
			require.NotNil(building, "the first setup saved a generation that still needs its vectors")
			f.onEmbedding = nil
			snapshot, err = config.ReadConfigFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			saved, err := f.cfg.ReloadConfigFile(snapshot)
			require.NoError(err)
			require.True(saved.Vector.Enabled)
			require.Empty(saved.Vector.Embed.Schedule.Cron)
			require.False(saved.Vector.Embed.Schedule.RunAfterSync)
			if provider == "builtin after restart" {
				f.handle.CloseFeatures(f.state.logger)
				f.cfg, f.state.cfg = saved, saved
				f.handle = startVectorInit(f.ctx, f.store, f.cfg.DatabaseDSN(), nil, f.server, f.scheduler)
				require.True(f.handle.WaitTimeout(5 * time.Second))
				f.controller.handle = f.handle
			}
			// Full rebuild selects through a different branch than Turn on's default resume.
			f.runner.beforeBuild = nil // The hook checks Turn on's gate ownership; this call exercises the builder.
			require.NoError(f.runner.RunCLICommand(f.ctx, api.CLIRunRequest{Args: []string{"embeddings", "build", "--full-rebuild", "--yes"}}, nil))
			active, err := f.handle.vf.Backend.ActiveGeneration(f.ctx)
			require.NoError(err)
			require.Equal(building.ID, active.ID, "Build index must resume the matching generation")
		})
	}
}
