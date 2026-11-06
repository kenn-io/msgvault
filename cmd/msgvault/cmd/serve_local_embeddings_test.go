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
	docbankdocument "go.kenn.io/docbank/document"
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
	failBuild   bool
	beforeBuild func()
}

func (r *localBuildTestRunner) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	r.calls = append(r.calls, slices.Clone(req.Args))
	r.contexts = append(r.contexts, ctx)
	index := slices.Index(req.Args, "--config")
	if !assert.Positive(r.t, index) || index+1 >= len(req.Args) {
		return errors.New("missing test config argument")
	}
	snapshot, err := config.ReadConfigFile(req.Args[index+1])
	if err != nil {
		return err
	}
	home := ""
	if homeIndex := slices.Index(req.Args, "--home"); homeIndex >= 0 {
		home = req.Args[homeIndex+1]
	}
	cfg, err := config.LoadConfigFile(snapshot, home)
	if err != nil {
		return err
	}
	if !assert.Equal(r.t, r.home, cfg.HomeDir) {
		return errors.New("unexpected test config home")
	}
	if r.beforeBuild != nil {
		r.beforeBuild()
	}
	if r.failBuild && req.Args[1] == "build" {
		r.failBuild = false
		if emit != nil {
			if err := emit(api.CLIRunEvent{Type: cliStreamStderr, Data: strings.Repeat("index progress\n", 1024)}); err != nil {
				return err
			}
			if err := emit(api.CLIRunEvent{Type: cliStreamStderr, Data: "Error: embedding build interrupted: "}); err != nil {
				return err
			}
			if err := emit(api.CLIRunEvent{Type: cliStreamStderr, Data: "context canceled\n"}); err != nil {
				return err
			}
		}
		return errors.New(cliSubprocessExitSentinel)
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
	command.SetArgs(req.Args[2:index])
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
	embeddingKey  atomic.Value
	onEmbedding   func([]string) error
	inputs        atomic.Int32
	installs      int
	refreshes     int
	runtimeCtx    context.Context
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

func newLocalActivationFixture(t *testing.T, model, extra string) *localActivationFixture {
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
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
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
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return endpoint.URL + "/v1", nil }
	localEmbeddingsDir = func() (string, error) { return filepath.Join(dir, "shared"), nil }
	t.Cleanup(func() { localEmbeddingEndpoint, localEmbeddingsDir = oldEndpoint, oldDir })
	content := "[data]\ndata_dir = " + strconv.Quote(dir) + "\n[vector]\nenabled = false\n"
	if model != "" {
		content = "[data]\ndata_dir = " + strconv.Quote(dir) + "\n[vector]\nenabled = true\n[vector.embeddings]\nmodel = " + strconv.Quote(model) + "\ndimension = 768\nendpoint = " + strconv.Quote(endpoint.URL+"/v1") + "\ndocument_prefix = \"document: \"\nquery_prefix = \"query: \"\nmax_input_chars = 1000\n"
	}
	content += "\n[chat]\nserver = " + strconv.Quote(endpoint.URL) + "\n" + extra
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
	_, _, err = testutil.CreateIndexedSourceMessage(f.store, "archive@example.test", "first", "Orchard", "The orchard has apples.")
	require.NoError(t, err)
	f.state = &invocation{cfg: f.cfg, logger: testDiscardLogger()}
	f.ctx = withInvocation(t.Context(), f.state)
	f.runner = &localBuildTestRunner{t: t, home: dir}
	f.scheduler = scheduler.New(nil)
	gate := api.NewSerialOperationGate()
	f.server = api.NewServerWithOptions(api.ServerOptions{OperationGate: gate, Config: f.cfg, Store: &storeAPIAdapter{store: f.store, config: f.cfg}, OperationHistoryReader: &storeAPIAdapter{store: f.store, config: f.cfg}, Logger: f.state.logger, VectorCfg: f.cfg.Vector})
	t.Cleanup(func() {
		<-f.scheduler.Stop().Done()
		_ = f.server.Shutdown(context.Background())
		if f.handle != nil {
			f.handle.CloseFeatures(f.state.logger)
		}
	})
	f.controller = &localEmbeddingsController{gate: gate, state: f.state, store: f.store, dbPath: f.cfg.DatabaseDSN(), server: f.server, scheduler: f.scheduler, runner: f.runner,
		refreshMCP: func(ctx context.Context, state *invocation) error {
			f.refreshes++
			f.runtimeCtx = ctx
			assert.NoError(t, ctx.Err())
			assert.True(t, state.cfg.Vector.Enabled)
			return nil
		},
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
				return embedlocal.Installation{}, err
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

func (f *localActivationFixture) localStatus(t *testing.T) api.LocalEmbeddingsStatus {
	t.Helper()
	require := require.New(t)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/local", nil)
	request.Header.Set("Authorization", "Bearer owner")
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	f.server.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var status api.LocalEmbeddingsStatus
	require.NoError(json.Unmarshal(response.Body.Bytes(), &status))
	return status
}

func (f *localActivationFixture) enableThroughAPI(t *testing.T) {
	t.Helper()
	f.server.SetLocalEmbeddings(f.controller)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings/local", strings.NewReader(`{"use_builtin":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer owner")
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	f.server.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusAccepted, response.Code)
}

func TestLocalEmbeddingsRestoresSavedBuiltinWithOllamaAvailable(t *testing.T) {
	for _, startupFailed := range []bool{false, true} {
		t.Run(fmt.Sprintf("startup_failed=%t", startupFailed), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newLocalActivationFixture(t, "nomic-embed-text", "")
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
			assert.True(status.OllamaAvailable)
			f.server.SetLocalEmbeddings(f.controller)
			assert.Equal(startupError, f.localStatus(t).Error)
			localEmbeddingEndpoint = fixtureEndpoint
			require.NoError(f.controller.Enable(f.ctx, false, func(api.LocalEmbeddingsStatus) {}))
			require.Equal(1, f.installs)
			assert.Empty(f.localStatus(t).Error)
			status, err = f.controller.Inspect(f.ctx)
			require.NoError(err)
			require.True(status.Enabled)
			require.Equal("builtin", status.Provider)
			messages, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 5})
			require.NoError(err)
			require.Len(messages, 1)
		})
	}
}

func TestLocalEmbeddingsDropsOldJobWhenScheduleReplacementFails(t *testing.T) {
	require := require.New(t)
	f := newLocalActivationFixture(t, "nomic-embed-text", "")
	f.start(t)
	snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
	require.NoError(err)
	_, err = config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"vector", "embed", "schedule"}, Values: map[string]any{"cron": ", * * * *"}}})
	require.NoError(err)
	f.enableThroughAPI(t)
	require.Eventually(func() bool { return !f.localStatus(t).Running }, 5*time.Second, 10*time.Millisecond)
	require.Contains(f.localStatus(t).Error, "invalid embed cron")
	require.ErrorContains(f.scheduler.TriggerJob(jobctx.ScheduledEmbeddingLabel), "not scheduled")
	_, _, err = testutil.CreateIndexedSourceMessage(f.store, "archive@example.test", "later", "Later orchard", "Another orchard message.")
	require.NoError(err)
	f.runner.beforeBuild = nil
	require.NoError(f.runner.RunCLICommand(f.ctx, api.CLIRunRequest{Args: []string{"embeddings", "build", "--yes", "--config", f.cfg.ConfigFilePath()}}, nil))
	require.Contains(f.localStatus(t).Error, "invalid embed cron", "a message build cannot repair rejected scheduling")
}

func TestLocalEmbeddingsChoosesAvailableProvider(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		kit, configured, builtin, available bool
		installs                            int
	}{
		{name: "built-in", builtin: true, installs: 1},
		{name: "available Ollama", kit: true, available: true},
		{name: "Ollama on chat server", configured: true, available: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLocalActivationFixture(t, "", "")
			f.ollamaTags = tc.configured || tc.kit
			if tc.kit {
				f.ollamaCatalog = `{"models":[]}`
				f.cfg.Chat.Server = "http://127.0.0.1:11434"
				require.NoError(t, f.cfg.Save())
			}
			f.controller.inspect = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
				return embedlocal.Status{OllamaAvailable: tc.kit}, nil
			}

			f.start(t)
			availability, err := f.controller.Inspect(f.ctx)
			require.NoError(t, err)
			assert.Equal(t, tc.available, availability.OllamaAvailable)
			assert.NotEmpty(t, availability.Host)
			require.NoError(t, f.controller.Enable(f.ctx, tc.builtin, func(api.LocalEmbeddingsStatus) {}))
			assert.Equal(t, tc.installs, f.installs)
			assert.Equal(t, 1, f.refreshes)
			if tc.builtin {
				require.NoError(t, f.runtimeCtx.Err())
				for _, ctx := range f.runner.contexts {
					_, bounded := ctx.Deadline()
					assert.True(t, bounded)
				}
			}

			generation, err := f.handle.vf.Backend.ActiveGeneration(f.ctx)
			require.NoError(t, err)
			assert.Equal(t, "nomic-embed-text", generation.Model)
		})
	}
}

func TestLocalEmbeddingsRejectsSharedEndpointAfterOllamaStops(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newLocalActivationFixture(t, "", "")
	f.start(t)
	before, err := os.ReadFile(f.cfg.ConfigFilePath())
	require.NoError(err)
	f.controller.inspect = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		return embedlocal.Status{OllamaAvailable: true}, nil
	}
	// Endpoint discovery falls back to the shared server if inspected Ollama stops.
	// The fixture endpoint serves embeddings but has no Ollama catalog.
	err = f.controller.Enable(f.ctx, false, func(api.LocalEmbeddingsStatus) {})
	assert.EqualError(err, "Ollama stopped during setup. Turn on to retry.")
	after, err := os.ReadFile(f.cfg.ConfigFilePath())
	require.NoError(err)
	assert.Equal(string(before), string(after), "retry must retain the saved provider choice")
}

func TestLocalEmbeddingsSwitchesConfiguredOllamaEmbeddingOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, catalog string
		enabled       bool
	}{
		{name: "paused"},
		{name: "active with empty catalog", enabled: true, catalog: `{"models":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			f := newLocalActivationFixture(t, "nomic-embed-text", "")
			f.cfg.Vector.Enabled = tc.enabled
			f.ollamaTags = true
			content, err := os.ReadFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			content = []byte(strings.Replace(string(content), "enabled = true", "enabled = "+strconv.FormatBool(tc.enabled), 1))
			content = []byte(strings.Replace(string(content), "server = "+strconv.Quote(f.cfg.Chat.Server), "server = \"https://chat.example.test\"", 1))
			require.NoError(os.WriteFile(f.cfg.ConfigFilePath(), content, 0o600))
			f.cfg.Chat.Server = "https://chat.example.test"
			f.start(t)
			f.ollamaCatalog = tc.catalog
			status, err := f.controller.Inspect(f.ctx)
			require.NoError(err)
			require.Equal("ollama", status.Provider)
			require.True(status.OllamaAvailable, "Turn on resumes this provider without a download")
			require.NoError(f.controller.Enable(f.ctx, tc.enabled, func(api.LocalEmbeddingsStatus) {}))
			status, err = f.controller.Inspect(f.ctx)
			require.NoError(err)
			if tc.enabled {
				require.Equal("builtin", status.Provider)
			} else {
				require.Equal("ollama", status.Provider)
				require.True(status.Enabled)
				require.Zero(f.installs)
			}
		})
	}
}

func TestLocalEmbeddingsExplainsFailedStartupAndBuildsDespiteDocumentJobFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newLocalActivationFixture(t, "nomic-embed-text", "\n[attachments.documents]\nenabled = true\n[attachments.documents.index]\nenabled = true\n[attachments.documents.index.embeddings]\nenabled = true\nprofile = \"vector.embeddings\"\n")
	// A credential for a previous destination makes real startup fail before jobs register.
	credentials, err := providercredentials.Read(f.cfg.TokensDir())
	require.NoError(err)
	_, err = providercredentials.Put(f.cfg.TokensDir(), credentials.ETag, providercredentials.VectorEmbeddingsID, "https://previous.example.test/v1", "synthetic-previous-key")
	require.NoError(err)
	require.NoError(precheckVectorFeatures(f.cfg.DatabaseDSN(), f.cfg))
	f.handle = startVectorInit(f.ctx, f.store, f.cfg.DatabaseDSN(), nil, f.server, f.scheduler)
	require.True(f.handle.WaitTimeout(5 * time.Second))
	f.controller.handle = f.handle
	status, detail := f.server.VectorStatus()
	require.Equal(api.VectorStatusError, status)
	assert.Contains(detail, providercredentials.ErrOriginMismatch.Error())
	localStatus, err := f.controller.Inspect(f.ctx)
	require.NoError(err)
	assert.Contains(localStatus.Error, providercredentials.ErrOriginMismatch.Error())
	var id int64
	require.NoError(f.store.DB().QueryRow("SELECT id FROM messages").Scan(&id))
	seedLocalSearchDocument(t, f.store, id)
	// This archive already authorized the built-in destination on an earlier run.
	desired := *f.cfg
	desired.Vector.Embeddings.LocalOnly = true
	desired.Vector.Embeddings.Endpoint = ""
	state := *f.state
	state.cfg = &desired
	consentCtx := withInvocation(f.ctx, &state)
	spec, err := configuredDocumentVectorSpec(consentCtx, f.store)
	require.NoError(err)
	consent, err := configuredDocumentVectorConsentSpec(spec, &state, "document_embedding")
	require.NoError(err)
	_, _, err = f.store.RecordDocumentVectorConsent(f.ctx, consent, time.Now())
	require.NoError(err)
	// Publication registers jobs again; reject that replacement to exercise admission failure.
	snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
	require.NoError(err)
	_, err = config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"vector", "embed", "schedule"}, Values: map[string]any{"cron": ", * * * *"}}})
	require.NoError(err)
	var phase string
	beforeBuild := f.runner.beforeBuild
	f.runner.beforeBuild = func() {
		assert.Equal("indexing", phase)
		beforeBuild()
	}
	err = f.controller.Enable(f.ctx, true, func(status api.LocalEmbeddingsStatus) { phase = status.Phase })
	require.ErrorContains(err, "not scheduled")
	assert.Equal("idle", phase)
	messages, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 5})
	require.NoError(err)
	require.Len(messages, 1)
}

func TestLocalEmbeddingsPreservesConfiguredLocalProvider(t *testing.T) {
	for _, tc := range []struct {
		name, catalog string
		pausedRemote  bool
	}{
		{name: "local without Ollama API"},
		{name: "local without catalog", catalog: `{}`},
		{name: "local with invalid catalog", catalog: `invalid json`},
		{name: "paused remote", pausedRemote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newLocalActivationFixture(t, "custom-model", "")
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
			status, err := f.controller.Inspect(f.ctx)
			require.NoError(err)
			assert.Equal("configured", status.Provider)
			assert.False(status.OllamaAvailable)
			for _, builtin := range []bool{false, true} {
				require.ErrorContains(f.controller.Enable(f.ctx, builtin, func(api.LocalEmbeddingsStatus) {}), "already uses a configured provider")
			}
			after, err := os.ReadFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			assert.Equal(before, after)
		})
	}
}

func TestLocalEmbeddingsReusesOrReplacesMessageGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, model, building string
		reuse                 bool
	}{
		{name: "matching active", model: "nomic-embed-text", reuse: true},
		{name: "different active", model: "different-model"},
		{name: "matching build", model: "nomic-embed-text", building: "matching"},
		{name: "different build", model: "different-model", building: "obsolete-build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLocalActivationFixture(t, tc.model, "")
			f.start(t)
			old, err := f.handle.vf.Backend.ActiveGeneration(f.ctx)
			require.NoError(t, err)
			var id int64
			require.NoError(t, f.store.DB().QueryRow("SELECT id FROM messages").Scan(&id))
			oldVector, err := f.handle.vf.Backend.LoadVector(f.ctx, id)
			require.NoError(t, err)
			_, _, err = testutil.CreateIndexedSourceMessage(f.store, "archive@example.test", "second", "Pear", "A pear tree is growing.")
			require.NoError(t, err)
			var building vector.GenerationID
			if tc.building != "" {
				fingerprint := tc.building
				if tc.building == "matching" {
					fingerprint = f.handle.vf.Cfg.GenerationFingerprint()
				}
				building, err = f.handle.vf.Backend.CreateGeneration(f.ctx, tc.model, 768, fingerprint)
				require.NoError(t, err)
			}
			before := f.inputs.Load()
			require.NoError(t, f.controller.Enable(f.ctx, true, func(api.LocalEmbeddingsStatus) {}))
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

func TestLocalEmbeddingsRequiresPeopleConsentForSelectedProvider(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		consented   bool
	}{
		{name: "compatible destination consent granted", model: "nomic-embed-text", consented: true},
		{name: "replacement destination consent granted", model: "different-model", consented: true},
		{name: "destination consent missing", model: "different-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			f := newLocalActivationFixture(t, tc.model, "\n[vector.people]\nenabled = true\nretention_posture = \"zero_data_retention\"\ntraining_posture = \"no_training\"\n[vector.embed.schedule]\ncron = \"\"\nrun_after_sync = false\n")
			participant, err := f.store.EnsureParticipantByIdentifier("email", "curated@example.test", "Curated Orchard Person")
			require.NoError(err)
			_, _, err = f.store.CreatePersonFromParticipantContext(f.ctx, participant)
			require.NoError(err)
			configs := []vector.Config{f.cfg.Vector}
			if tc.consented {
				desired := f.cfg.Vector
				desired.Embeddings.LocalOnly = true
				desired.Embeddings.Endpoint = ""
				desired.Embeddings.APIKeyEnv = ""
				desired.Embeddings.Model = "nomic-embed-text"
				configs = append(configs, desired)
			}
			for _, cfg := range configs {
				profile, err := cfg.SemanticPersonEmbeddingProfile()
				require.NoError(err)
				_, err = f.store.EnsurePersonSemanticEmbeddingProfile(f.ctx, profile)
				require.NoError(err)
				_, _, err = f.store.GrantPersonSemanticEmbeddingConsent(f.ctx, profile.Fingerprint, "test")
				require.NoError(err)
			}
			f.start(t)
			before, err := f.handle.vf.PersonSearchEngine.Search(f.ctx, "orchard", 5)
			require.NoError(err)
			require.Len(before, 1)
			require.NoError(f.controller.Enable(f.ctx, true, func(api.LocalEmbeddingsStatus) {}))
			after, err := f.handle.vf.PersonSearchEngine.Search(f.ctx, "orchard", 5)
			if tc.consented {
				require.NoError(err, "authorized people search must survive activation without automatic indexing")
				require.Len(after, 1)
			} else {
				require.ErrorIs(err, vector.ErrSemanticPersonEmbeddingConsentRequired)
			}
		})
	}
}

func TestLocalEmbeddingsRequiresDocumentConsentForSelectedProvider(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newLocalActivationFixture(t, "different-model", "\n[attachments.documents]\nenabled = true\n[attachments.documents.index]\nenabled = true\n[attachments.documents.index.embeddings]\nenabled = true\nprofile = \"vector.embeddings\"\n")
	var id int64
	require.NoError(f.store.DB().QueryRow("SELECT id FROM messages").Scan(&id))
	seedLocalSearchDocument(t, f.store, id)
	spec, err := configuredDocumentVectorSpec(f.ctx, f.store)
	require.NoError(err)
	for _, purpose := range []string{"document_embedding", "query_embedding"} {
		consent, err := configuredDocumentVectorConsentSpec(spec, f.state, purpose)
		require.NoError(err)
		_, _, err = f.store.RecordDocumentVectorConsent(f.ctx, consent, time.Now())
		require.NoError(err)
	}
	f.start(t)
	require.NoError(runScheduledDocumentVectorGeneration(jobctx.WithProgress(f.ctx), f.store, f.handle.vf, 100))
	snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
	require.NoError(err)
	_, err = config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"vector", "embed", "schedule"}, Values: map[string]any{"cron": ", * * * *"}}})
	require.NoError(err)
	err = f.controller.Enable(f.ctx, true, func(api.LocalEmbeddingsStatus) {})
	require.ErrorContains(err, "invalid embed cron")
	assert.NotContains(err.Error(), "not scheduled", "the unconsented document job must not be admitted")
}

func TestLocalEmbeddingsBuildFailureKeepsSavedProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newLocalActivationFixture(t, "", "")
	f.start(t)
	f.runner.failBuild = true
	err := f.controller.Enable(f.ctx, true, func(api.LocalEmbeddingsStatus) {})
	require.EqualError(err, "Message index build interrupted. Use Build index to finish.")
	assert.Equal(1, f.refreshes, "published MCP capabilities refresh even when the initial build fails")
	snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
	require.NoError(err)
	saved, err := config.LoadConfigFile(snapshot, f.cfg.HomeDir)
	require.NoError(err)
	assert.True(saved.Vector.Enabled)
}

func TestLocalEmbeddingsCanceledBuildWithoutChildOutput(t *testing.T) {
	require := require.New(t)
	err := localEmbeddingsCommandError(t.Context(), true, context.Canceled, "")
	require.EqualError(err, "Message index build interrupted. Use Build index to finish.")
	require.ErrorIs(err, context.Canceled)
}

func TestLocalEmbeddingsRecoveryWithoutNewBatches(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newLocalActivationFixture(t, "", "")
			f.start(t)
			require.NoError(f.controller.Enable(f.ctx, true, func(api.LocalEmbeddingsStatus) {}))
			f.runner.failBuild = true
			f.enableThroughAPI(t)
			require.Eventually(func() bool { return !f.localStatus(t).Running }, 5*time.Second, 10*time.Millisecond)
			require.NotEmpty(f.localStatus(t).Error)
			args := []string{"embeddings", "build", "--yes", "--config", f.cfg.ConfigFilePath()}
			if empty {
				var sourceID int64
				require.NoError(f.store.DB().QueryRow("SELECT source_id FROM messages").Scan(&sourceID))
				require.NoError(f.store.MarkMessageDeleted(sourceID, "first"))
				args = slices.Insert(args, 3, "--full-rebuild")
			}
			f.runner.beforeBuild = nil
			require.NoError(f.runner.RunCLICommand(f.ctx, api.CLIRunRequest{Args: args}, nil))
			assert.Empty(f.localStatus(t).Error)
		})
	}
}

func TestLocalEmbeddingsSetupDeadlineCancelsWaitingInitialization(t *testing.T) {
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

func TestLocalEmbeddingsSetupFailureKeepsSavedSettings(t *testing.T) {
	for _, failure := range []string{"download", "config conflict", "provider conflict"} {
		t.Run(failure, func(t *testing.T) {
			require := require.New(t)
			f := newLocalActivationFixture(t, "", "")
			f.start(t)
			expected, err := os.ReadFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			install := f.controller.install
			f.controller.install = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
				if failure == "download" {
					return embedlocal.Installation{}, errors.New("connection interrupted")
				}
				if failure == "provider conflict" {
					expected = append(expected, []byte("\n[vector.embeddings]\nendpoint = \"https://embeddings.example.test/v1\"\nmodel = \"selected-model\"\ndimension = 4\n")...)
				} else {
					expected = append(expected, []byte("\n[web]\ntheme = \"dark\"\n")...)
				}
				require.NoError(os.WriteFile(f.cfg.ConfigFilePath(), expected, 0o600))
				return install(ctx, opts)
			}
			err = f.controller.Enable(f.ctx, true, func(api.LocalEmbeddingsStatus) {})
			if failure == "download" {
				require.ErrorContains(err, "connection interrupted")
			} else {
				require.ErrorIs(err, config.ErrConfigConflict)
				require.EqualError(err, "Settings changed during setup. Turn on to retry.")
			}
			actual, err := os.ReadFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			require.Equal(expected, actual)
			if failure == "download" {
				return
			}
			f.controller.install = install
			err = f.controller.Enable(f.ctx, true, func(api.LocalEmbeddingsStatus) {})
			if failure == "provider conflict" {
				require.ErrorContains(err, "already uses a configured provider")
				actual, err := os.ReadFile(f.cfg.ConfigFilePath())
				require.NoError(err)
				require.Equal(expected, actual)
				return
			}
			require.NoError(err)
			snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
			require.NoError(err)
			loaded, err := f.cfg.ReloadConfigFile(snapshot)
			require.NoError(err)
			require.Equal("dark", loaded.Web.Theme)
			require.True(loaded.Vector.Embeddings.LocalOnly)
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
