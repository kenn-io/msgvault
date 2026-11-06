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
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
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
	if r.beforeBuild != nil {
		r.beforeBuild()
	}
	if r.failBuild && req.Args[1] == "build" {
		r.failBuild = false
		if emit != nil {
			require.NoError(r.t, emit(api.CLIRunEvent{Type: cliStreamStderr, Data: strings.Repeat("index progress\n", 1024)}))
			require.NoError(r.t, emit(api.CLIRunEvent{Type: cliStreamStderr, Data: "embedding build interrupted: "}))
			require.NoError(r.t, emit(api.CLIRunEvent{Type: cliStreamStderr, Data: "context canceled\n"}))
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

type localActivationFixture struct {
	cfg        *config.Config
	store      *store.Store
	state      *invocation
	ctx        context.Context
	handle     *vectorInitHandle
	server     *api.Server
	scheduler  *scheduler.Scheduler
	controller *localEmbeddingsController
	runner     *localBuildTestRunner
	inputs     atomic.Int32
	installs   int
	refreshes  int
	runtimeCtx context.Context
	ollamaTags bool
}

func newLocalActivationFixture(t *testing.T, model, extra string) *localActivationFixture {
	t.Helper()
	f := &localActivationFixture{}
	dir := t.TempDir()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			models := []map[string]string{}
			if f.ollamaTags {
				models = append(models, map[string]string{"name": "nomic-embed-text:latest"})
			}
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
		f.inputs.Add(int32(len(body.Input)))
		data := make([]map[string]any, len(body.Input))
		for i := range data {
			vec := make([]float32, 768)
			vec[0] = 1
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
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
	f.server = api.NewServerWithOptions(api.ServerOptions{OperationGate: gate, Config: f.cfg, Store: &storeAPIAdapter{store: f.store, config: f.cfg}, Logger: f.state.logger, VectorCfg: f.cfg.Vector})
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
			require.NoError(t, ctx.Err())
			assert.True(t, state.cfg.Vector.Enabled)
			return nil
		},
		inspect: func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
			return embedlocal.Status{DownloadBytes: 290000000}, nil
		},
		install: func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
			f.installs++
			_, _, held := gate.Holder()
			require.False(t, held, "installation leaves the archive available to sync")
			release, ok := gate.BeginLabeledWorkContext(ctx, "scheduled sync")
			require.True(t, ok)
			release()
			yes, err := opts.Confirm(ctx, 290000000)
			require.NoError(t, err)
			require.True(t, yes)
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

func TestLocalEmbeddingsChoosesAvailableProvider(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		kit, configured, builtin bool
		installs                 int
	}{
		{name: "built-in", builtin: true, installs: 1},
		{name: "available Ollama", kit: true},
		{name: "configured Ollama", configured: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLocalActivationFixture(t, "", "")
			f.ollamaTags = tc.configured
			f.controller.inspect = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
				return embedlocal.Status{OllamaAvailable: tc.kit}, nil
			}
			mcp := &cobra.Command{Use: "serve"}
			if tc.builtin {
				addServeConfigFlags(mcp)
				require.NoError(t, installServeMCP(f.ctx, mcp, f.server, f.state))
				assert.NotContains(t, f.mcpCall("tools/list", `{}`).Body.String(), `"find_similar_messages"`)
				refresh := f.controller.refreshMCP
				f.controller.refreshMCP = func(ctx context.Context, state *invocation) error {
					require.NoError(t, refresh(ctx, state))
					return installServeMCP(ctx, mcp, f.server, state)
				}
			}
			f.start(t)
			availability, err := f.controller.Inspect(f.ctx)
			require.NoError(t, err)
			assert.Equal(t, tc.kit || tc.configured, availability.OllamaAvailable)
			assert.NotEmpty(t, availability.Host)
			require.NoError(t, f.controller.Enable(f.ctx, tc.builtin, func(api.LocalEmbeddingsStatus) {}))
			assert.Equal(t, tc.installs, f.installs)
			assert.Equal(t, 1, f.refreshes)
			generation, err := f.handle.vf.Backend.ActiveGeneration(f.ctx)
			require.NoError(t, err)
			assert.Equal(t, "nomic-embed-text", generation.Model)
			require.NoError(t, f.runtimeCtx.Err())
			for _, ctx := range f.runner.contexts {
				_, bounded := ctx.Deadline()
				assert.True(t, bounded)
				require.ErrorIs(t, ctx.Err(), context.Canceled)
			}
			if tc.builtin {
				assert.Contains(t, f.mcpCall("tools/list", `{}`).Body.String(), `"find_similar_messages"`)
				semantic := f.mcpCall("tools/call", `{"name":"semantic_search_messages","arguments":{"query":"orchard","mode":"vector"}}`)
				require.Equal(t, http.StatusOK, semantic.Code, semantic.Body.String())
				assert.Contains(t, semantic.Body.String(), "Orchard")
				assert.NotContains(t, semantic.Body.String(), `"isError":true`)
			}
		})
	}
}

func (f *localActivationFixture) mcpCall(method, params string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+params+`}`))
	request.Header.Set("Authorization", "Bearer owner")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	response := httptest.NewRecorder()
	f.server.Router().ServeHTTP(response, request)
	return response
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

func TestLocalEmbeddingsRetainsPeopleConsent(t *testing.T) {
	for _, consented := range []bool{true, false} {
		t.Run(strconv.FormatBool(consented), func(t *testing.T) {
			f := newLocalActivationFixture(t, "different-model", "\n[vector.people]\nenabled = true\nretention_posture = \"zero_data_retention\"\ntraining_posture = \"no_training\"\n")
			participant, err := f.store.EnsureParticipantByIdentifier("email", "curated@example.test", "Curated Orchard Person")
			require.NoError(t, err)
			_, _, err = f.store.CreatePersonFromParticipantContext(f.ctx, participant)
			require.NoError(t, err)
			if consented {
				profile, err := f.cfg.Vector.SemanticPersonEmbeddingProfile()
				require.NoError(t, err)
				_, err = f.store.EnsurePersonSemanticEmbeddingProfile(f.ctx, profile)
				require.NoError(t, err)
				_, _, err = f.store.GrantPersonSemanticEmbeddingConsent(f.ctx, profile.Fingerprint, "test")
				require.NoError(t, err)
			}
			f.start(t)
			require.NoError(t, f.controller.Enable(f.ctx, true, func(api.LocalEmbeddingsStatus) {}))
			results, err := f.handle.vf.PersonSearchEngine.Search(f.ctx, "orchard", 5)
			if consented {
				require.NoError(t, err)
				require.Len(t, results, 1)
			} else {
				require.ErrorIs(t, err, vector.ErrSemanticPersonEmbeddingConsentRequired)
			}
		})
	}
}

func TestLocalEmbeddingsRetainsDocumentConsentAfterConfigWrite(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(strconv.FormatBool(conflict), func(t *testing.T) {
			f := newLocalActivationFixture(t, "different-model", "\n[attachments.documents]\nenabled = true\n[attachments.documents.index]\nenabled = true\n[attachments.documents.index.embeddings]\nenabled = true\nprofile = \"vector.embeddings\"\n")
			var id int64
			require.NoError(t, f.store.DB().QueryRow("SELECT id FROM messages").Scan(&id))
			seedLocalSearchDocument(t, f.store, id)
			spec, err := configuredDocumentVectorSpec(f.ctx, f.store)
			require.NoError(t, err)
			for _, purpose := range []string{"document_embedding", "query_embedding"} {
				consent, err := configuredDocumentVectorConsentSpec(spec, f.state, purpose)
				require.NoError(t, err)
				_, _, err = f.store.RecordDocumentVectorConsent(f.ctx, consent, time.Now())
				require.NoError(t, err)
			}
			f.start(t)
			require.NoError(t, runScheduledDocumentVectorGeneration(jobctx.WithProgress(f.ctx), f.store, f.handle.vf, 100))
			old, err := f.store.GetActiveDocumentVectorGeneration(f.ctx)
			require.NoError(t, err)
			require.NotNil(t, old)
			if conflict {
				install := f.controller.install
				f.controller.install = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
					file, err := os.OpenFile(f.cfg.ConfigFilePath(), os.O_APPEND|os.O_WRONLY, 0o600)
					require.NoError(t, err)
					_, err = file.WriteString("\n[web]\ntheme = \"dark\"\n")
					require.NoError(t, err)
					require.NoError(t, file.Close())
					return install(ctx, opts)
				}
			}
			err = f.controller.Enable(f.ctx, true, func(api.LocalEmbeddingsStatus) {})
			if conflict {
				require.Error(t, err)
				var consents int
				require.NoError(t, f.store.DB().QueryRow("SELECT COUNT(*) FROM document_vector_consents").Scan(&consents))
				assert.Equal(t, 2, consents)
				snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
				require.NoError(t, err)
				saved, err := config.LoadConfigFile(snapshot, f.cfg.HomeDir)
				require.NoError(t, err)
				assert.Equal(t, "different-model", saved.Vector.Embeddings.Model)
				return
			}
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				generation, err := f.store.GetActiveDocumentVectorGeneration(f.ctx)
				return err == nil && generation != nil && generation.ID != old.ID
			}, 10*time.Second, 50*time.Millisecond)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/documents/search?q=orchard&mode=semantic", nil)
			request.Header.Set("Authorization", "Bearer owner")
			f.server.Router().ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "orchard.pdf")
			assert.Contains(t, response.Body.String(), "orchard document evidence")
		})
	}
}

func TestLocalEmbeddingsBuildFailureKeepsSavedProvider(t *testing.T) {
	f := newLocalActivationFixture(t, "", "")
	f.start(t)
	f.runner.failBuild = true
	f.runner.beforeBuild = func() {
		_, err := f.handle.vf.Backend.CreateGeneration(f.ctx, f.handle.vf.Cfg.Embeddings.Model, f.handle.vf.Cfg.Embeddings.Dimension, f.handle.vf.Cfg.GenerationFingerprint())
		require.NoError(t, err)
	}
	err := f.controller.Enable(f.ctx, true, func(api.LocalEmbeddingsStatus) {})
	require.ErrorContains(t, err, "embedding build interrupted: context canceled")
	assert.LessOrEqual(t, len(err.Error()), len(cliSubprocessExitSentinel)+2+8192)
	snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
	require.NoError(t, err)
	saved, err := config.LoadConfigFile(snapshot, f.cfg.HomeDir)
	require.NoError(t, err)
	assert.True(t, saved.Vector.Enabled)
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

func TestLocalEmbeddingsDownloadFailureKeepsSavedSettings(t *testing.T) {
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	cfg := fixture.load(t)
	before, err := os.ReadFile(fixture.path)
	require.NoError(t, err)
	handle := &vectorInitHandle{done: make(chan struct{})}
	close(handle.done)
	controller := &localEmbeddingsController{state: &invocation{cfg: cfg, logger: testDiscardLogger()}, handle: handle,
		inspect: func(context.Context, embedlocal.Options) (embedlocal.Status, error) { return embedlocal.Status{}, nil },
		install: func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
			return embedlocal.Installation{}, errors.New("connection interrupted")
		},
	}
	err = controller.Enable(t.Context(), true, func(api.LocalEmbeddingsStatus) {})
	require.ErrorContains(t, err, "connection interrupted")
	after := fixture.load(t)
	assert.False(t, after.Vector.Enabled)
	actual, err := os.ReadFile(fixture.path)
	require.NoError(t, err)
	assert.Equal(t, before, actual)
}

func TestLocalEmbeddingsPreservesPausedConfiguredProviders(t *testing.T) {
	for _, chat := range []string{"", "[chat]\nserver = \"https://api.example.test\"\n"} {
		t.Run(chat, func(t *testing.T) {
			fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig+`
[vector]
enabled = false
[vector.embeddings]
endpoint = "https://api.example.test/v1"
model = "configured-model"
api_key_env = "EXAMPLE_EMBEDDING_KEY"
`+chat)
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
			controller.inspect = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
				return embedlocal.Status{}, errors.New("local activation reached for a remote provider")
			}
			for _, builtin := range []bool{false, true} {
				assert.ErrorContains(t, controller.Enable(t.Context(), builtin, func(api.LocalEmbeddingsStatus) {}), "already uses a configured provider")
			}
			after, err := os.ReadFile(fixture.path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
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
