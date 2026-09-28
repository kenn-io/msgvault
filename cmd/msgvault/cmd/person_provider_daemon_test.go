package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personenrollment"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type inProcessPersonProviderDaemonStore struct {
	*storeAPIAdapter

	config      peoplesweep.Config
	httpClient  *http.Client
	credentials peoplesweep.CredentialStore
	requests    chan<- api.CLIRunRequest
}

func (s *inProcessPersonProviderDaemonStore) RunCLICommand(
	ctx context.Context,
	req api.CLIRunRequest,
	emit func(api.CLIRunEvent) error,
) error {
	if s.requests != nil {
		s.requests <- req
	}
	deps := localPersonProviderDeps(s.config, s.store, nil)
	deps.newChecker = func(
		config peoplesweep.Config,
		consent personProviderStore,
		_ personProviderSetupDeps,
	) (personProviderChecker, error) {
		registry, err := peoplesweep.NewDriverRegistry(s.httpClient, nil, nil)
		if err != nil {
			return nil, err
		}
		return peoplesweep.NewRunner(
			config,
			consent,
			registry,
			peoplesweep.NewCredentialResolver(s.credentials, func(name string) (string, bool) {
				if value, ok := req.Env[name]; ok {
					return value, true
				}
				return os.LookupEnv(name)
			}),
		)
	}

	root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonProviderCommand(deps))
	root.AddCommand(person)
	root.SetArgs(req.Args)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	err := root.ExecuteContext(ctx)
	if output.Len() > 0 && emit != nil {
		if emitErr := emit(api.CLIRunEvent{Type: cliStreamStdout, Data: output.String()}); emitErr != nil {
			return emitErr
		}
	}
	if err != nil {
		return fmt.Errorf("execute in-process person provider command: %w", err)
	}
	return nil
}

func TestSavedPersonProviderCheckForwardsExactCredentialThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const keyName = "SETUP_ONLY_PROVIDER_KEY"
	const secret = "synthetic-onboarding-key"
	t.Setenv(keyName, "") // The daemon process does not have the caller's key.
	var received atomic.Int64
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			http.Error(w, "missing credential", http.StatusUnauthorized)
			return
		}
		received.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"test-model","choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(provider.Close)
	peopleConfig := personProviderTestConfig()
	onboarded := configuredPersonProvider(peopleConfig)
	onboarded.Endpoint, onboarded.CredentialEnv = provider.URL+"/v1", keyName
	peopleConfig.Providers["onboarded"] = onboarded
	daemonConfig := config.NewDefaultConfig()
	daemonConfig.HomeDir = t.TempDir()
	daemonConfig.People.Sweep = personProviderTestConfig()
	// Onboarding publishes a new profile after the daemon has started.
	saved := *daemonConfig
	saved.People.Sweep = peopleConfig
	require.NoError(saved.Save())
	st := &inProcessPersonProviderDaemonStore{
		storeAPIAdapter: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t)},
		config:          peopleConfig, httpClient: provider.Client(),
	}
	daemon := api.NewServerWithOptions(api.ServerOptions{
		Config: daemonConfig, Store: st, Logger: slog.New(slog.DiscardHandler),
		OperationGate: api.NewSerialOperationGate(),
	})
	server := httptest.NewServer(daemon.Router())
	t.Cleanup(server.Close)
	frontend := *daemonConfig
	frontend.People.Sweep = peopleConfig
	frontend.Remote = config.RemoteConfig{URL: server.URL, AllowInsecure: true}
	testCtx := withStoreResolverConfig(t, &frontend)
	deps := defaultPersonProviderCommandDeps(testCtx)
	callerHasKey := false
	deps.setup.lookupEnv = func(name string) (string, bool) {
		assert.Equal(keyName, name)
		return secret, callerHasKey
	}
	var output bytes.Buffer
	command := &cobra.Command{Use: "setup"}
	command.SetContext(testCtx)
	command.SetContext(testCtx)
	command.SetOut(&output)
	command.SetErr(&output)
	require.Error(executeSavedPersonProviderCheck(command, deps, "onboarded", "", &output))
	assert.Zero(received.Load())
	output.Reset()
	callerHasKey = true
	require.NoError(executeSavedPersonProviderCheck(command, deps, "onboarded", "", &output), output.String())
	assert.Equal(int64(1), received.Load())
	assert.NotContains(output.String(), secret)

	profileConfig := peopleConfig
	profileConfig.Enabled = true
	profileConfig.Provider = peoplesweep.ProviderSelection{Name: "onboarded"}
	profile, err := profileConfig.Profile()
	require.NoError(err)
	for _, test := range []struct{ name, fingerprint, key string }{
		{name: "other provider key", fingerprint: profile.Fingerprint, key: "TEST_PROVIDER_KEY"},
		{name: "changed profile", fingerprint: strings.Repeat("a", 64), key: keyName},
		{name: "ordinary check", key: keyName},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"person", "provider", "check", "onboarded"}
			if test.fingerprint != "" {
				args = append(args, "--if-fingerprint", test.fingerprint)
			}
			body := mustJSON(t, api.CLIRunRequest{Args: args, Env: map[string]string{test.key: secret}})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			daemon.Router().ServeHTTP(response, request)
			assert.Equal(http.StatusBadRequest, response.Code, response.Body.String())
			assert.Equal(int64(1), received.Load())
		})
	}
}

func TestPersonProviderRealDaemonSyntheticCheckAndRevoke(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	type capturedProviderRequest struct {
		Authorization string
		Path          string
		Body          map[string]any
	}
	requests := make(chan capturedProviderRequest, 1)
	var requestCount atomic.Int64
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests <- capturedProviderRequest{
			Authorization: r.Header.Get("Authorization"),
			Path:          r.URL.Path,
			Body:          body,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "req-daemon")
		_, _ = io.WriteString(w, `{
			"model":"test-model",
			"choices":[{"message":{"content":"{\"ok\":true}"}}],
			"usage":{"prompt_tokens":9,"completion_tokens":2}
		}`)
	}))
	t.Cleanup(provider.Close)

	peopleConfig := personProviderTestConfig()
	mutateConfiguredPersonProvider(&peopleConfig, func(config *peoplesweep.ProviderConfig) {
		config.Endpoint = provider.URL + "/v1"
	})
	st := testutil.NewSQLiteTestStore(t)
	requestsToDaemon := make(chan api.CLIRunRequest, 4)
	daemonConfig := &config.Config{People: config.PeopleConfig{Sweep: peopleConfig}}
	daemonStore := &inProcessPersonProviderDaemonStore{
		storeAPIAdapter: &storeAPIAdapter{store: st},
		config:          peopleConfig,
		httpClient:      provider.Client(),
		requests:        requestsToDaemon,
	}
	var daemonLogs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&daemonLogs, nil))
	daemon := api.NewServerWithOptions(api.ServerOptions{
		Config: daemonConfig, Store: daemonStore, Logger: logger,
		OperationGate: api.NewSerialOperationGate(),
	})
	rawDaemonBodies := make(chan []byte, 4)
	daemonRouter := daemon.Router()
	daemonHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/cli/run" {
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				http.Error(w, "read request body", http.StatusBadRequest)
				return
			}
			rawDaemonBodies <- body
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		daemonRouter.ServeHTTP(w, r)
	}))
	t.Cleanup(daemonHTTP.Close)

	frontendConfig := *daemonConfig
	frontendConfig.Remote = config.RemoteConfig{URL: daemonHTTP.URL, AllowInsecure: true}
	testCtx := withStoreResolverConfig(t, &frontendConfig)
	const environmentSecretCanary = "caller-key-never-in-daemon-request"
	t.Setenv("TEST_PROVIDER_KEY", environmentSecretCanary)
	deps := defaultPersonProviderCommandDeps()

	reverifyOutput, err := executePersonProviderCommandContext(testCtx, t, deps, "reverify", "--yes")
	require.NoError(err)
	assert.Contains(reverifyOutput, "People inference provider disclosure")
	assert.Contains(reverifyOutput, provider.URL+"/v1")
	captured := <-requests
	consentOutput, err := executePersonProviderCommandContext(testCtx, t, deps, "consent", "--yes", "--json")
	require.NoError(err)
	assert.Contains(consentOutput, `"active":true`)
	output, err := executePersonProviderCommandContext(testCtx, t, deps, "check", "--json")
	require.NoError(err)
	assert.JSONEq(`{
		"ok":true,
		"provider_request_id":"req-daemon",
		"model":"test-model",
		"usage":{"input_tokens":9,"output_tokens":2}
	}`, output)

	assert.Equal("Bearer "+environmentSecretCanary, captured.Authorization)
	assert.Equal("/v1/chat/completions", captured.Path)
	assert.Equal("test-model", captured.Body["model"])
	messages, ok := captured.Body["messages"].([]any)
	require.True(ok)
	require.Len(messages, 2)
	message, ok := messages[1].(map[string]any)
	require.True(ok)
	assert.Equal("Return an object with ok set to true.", message["content"])
	assert.NotContains(string(mustJSON(t, captured.Body)), "archive")
	for range 3 {
		req := <-requestsToDaemon
		wire := mustJSON(t, req)
		assert.Empty(req.Env)
		assert.NotContains(string(wire), environmentSecretCanary)
		assert.NotContains(string(<-rawDaemonBodies), environmentSecretCanary)
	}
	assert.NotContains(output, environmentSecretCanary)
	assert.NotContains(daemonLogs.String(), environmentSecretCanary)
	<-requests

	_, err = executePersonProviderCommandContext(testCtx, t, deps, "revoke", "--json")
	require.NoError(err)
	output, err = executePersonProviderCommandContext(testCtx, t, deps, "check", "--json")
	require.NoError(err)
	assert.JSONEq(`{
		"ok":true,
		"provider_request_id":"req-daemon",
		"model":"test-model",
		"usage":{"input_tokens":9,"output_tokens":2}
	}`, output)
	assert.Equal(int64(3), requestCount.Load(), "synthetic checks bypass archive consent")
}

func TestPersonProviderStoredCheckKeepsSecretOutOfDaemonMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	const secretCanary = "stored-daemon-secret-canary"
	requests := make(chan api.CLIRunRequest, 1)
	providerRequests := make(chan string, 1)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerRequests <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"model":"test-model",
			"choices":[{"message":{"content":"{\"ok\":true}"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1}
		}`)
	}))
	t.Cleanup(provider.Close)

	peopleConfig := personProviderTestConfig()
	stored := configuredPersonProvider(peopleConfig)
	stored.Endpoint = provider.URL + "/v1"
	stored.Credential = peoplesweep.CredentialStored
	stored.CredentialEnv = ""
	peopleConfig.Provider = peoplesweep.ProviderSelection{Name: "stored"}
	peopleConfig.Providers = map[string]peoplesweep.ProviderConfig{"stored": stored}
	credentialStore := peoplesweep.NewFileCredentialStore(t.TempDir())
	require.NoError(credentialStore.Save("stored", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, secretCanary)))
	st := testutil.NewSQLiteTestStore(t)
	daemonConfig := &config.Config{People: config.PeopleConfig{Sweep: peopleConfig}}
	daemonStore := &inProcessPersonProviderDaemonStore{
		storeAPIAdapter: &storeAPIAdapter{store: st},
		config:          peopleConfig,
		httpClient:      provider.Client(),
		credentials:     credentialStore,
		requests:        requests,
	}
	daemon := api.NewServerWithOptions(api.ServerOptions{
		Config: daemonConfig, Store: daemonStore, Logger: slog.New(slog.DiscardHandler),
		OperationGate: api.NewSerialOperationGate(),
	})
	daemonHTTP := httptest.NewServer(daemon.Router())
	t.Cleanup(daemonHTTP.Close)

	frontendConfig := *daemonConfig
	frontendConfig.Remote = config.RemoteConfig{URL: daemonHTTP.URL, AllowInsecure: true}
	testCtx := withStoreResolverConfig(t, &frontendConfig)
	deps := defaultPersonProviderCommandDeps()
	output, err := executePersonProviderCommandContext(testCtx, t, deps, "check", "stored", "--json")
	require.NoError(err)
	assert.NotContains(output, secretCanary)

	req := <-requests
	wire, err := json.Marshal(req)
	require.NoError(err)
	assert.Equal([]string{"person", "provider", "check", "--json", "stored"}, req.Args)
	assert.Empty(req.Env)
	assert.NotContains(string(wire), secretCanary)
	assert.Equal("Bearer "+secretCanary, <-providerRequests)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}

var _ api.CLIRunner = (*inProcessPersonProviderDaemonStore)(nil)
var _ api.MessageStore = (*inProcessPersonProviderDaemonStore)(nil)
var _ personProviderStore = (*store.Store)(nil)

// Exercise the adapter installed by serve, not just the underlying Store.
func TestPeopleInferenceSettingsWithDaemonStore(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/v1/chat/completions", r.URL.Path)
		_, _ = io.WriteString(w, `{"model":"test-model","choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}]}`)
	}))
	defer providerServer.Close()
	configured := config.NewDefaultConfig()
	configured.HomeDir = t.TempDir()
	configured.Data.DataDir = configured.HomeDir
	require.NoError(configured.Save())
	st := testutil.NewSQLiteTestStore(t)
	before, err := config.ReadConfigFile(configured.ConfigFilePath())
	require.NoError(err)
	provider := configuredPersonProvider(personProviderTestConfig())
	provider.Endpoint = providerServer.URL + "/v1"
	provider.Auth, provider.Credential, provider.CredentialEnv = peoplesweep.AuthNone, peoplesweep.CredentialNone, ""
	created, err := personenrollment.NewService(configured.ConfigFilePath(), st).CreateProfile(before.ETag, "local", provider)
	require.NoError(err)
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: configured, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler),
		OperationGate: api.NewSerialOperationGate(),
	})
	const profilePath = "/api/v1/settings/people-inference/providers/local"
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("If-Match", created.ETag)
		r.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, r)
		return response
	}
	checked := request(http.MethodPost, profilePath+"/check", "")
	require.Equal(http.StatusOK, checked.Code, checked.Body.String())
	consented := request(http.MethodPost, profilePath+"/consent", fmt.Sprintf(`{"confirmed":true,"fingerprint":%q}`, created.Fingerprint))
	require.Equal(http.StatusOK, consented.Code, consented.Body.String())
	var status api.PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(consented.Body.Bytes(), &status))
	var local *api.PeopleInferenceProfileSetting
	for i := range status.Profiles {
		if status.Profiles[i].Name == "local" {
			local = &status.Profiles[i]
		}
	}
	require.NotNil(local)
	assert.True(local.Checked)
	assert.True(local.ConsentActive)
	removed := request(http.MethodDelete, profilePath, "")
	require.Equal(http.StatusOK, removed.Code, removed.Body.String())
	active, err := st.HasActivePersonInferenceConsent(t.Context(), created.Fingerprint)
	require.NoError(err)
	assert.False(active)
	verified, err := st.HasSuccessfulPersonInferenceCheck(t.Context(), created.Fingerprint)
	require.NoError(err)
	assert.False(verified)
}

func TestPersonProviderRemoveRevokesRunningPolicyThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/v1/chat/completions", r.URL.Path)
		calls.Add(1)
		_, _ = io.WriteString(w, `{"model":"running-model","choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}]}`)
	}))
	defer provider.Close()
	startup := config.NewDefaultConfig()
	startup.HomeDir = t.TempDir()
	startup.Data.DataDir = startup.HomeDir
	startup.People.Sweep = personProviderTestConfig()
	beta := configuredPersonProvider(startup.People.Sweep)
	beta.Model, beta.Endpoint = "running-model", provider.URL+"/v1"
	beta.Auth, beta.Credential, beta.CredentialEnv = peoplesweep.AuthNone, peoplesweep.CredentialNone, ""
	startup.People.Sweep.Providers["beta"] = beta
	startup.People.Sweep.Provider.Name = "beta"
	runningProfile, err := startup.People.Sweep.Profile()
	require.NoError(err)
	saved := *startup
	saved.People.Sweep.Providers = maps.Clone(startup.People.Sweep.Providers)
	beta.Model = "saved-model"
	saved.People.Sweep.Providers["beta"] = beta
	savedProfile, err := saved.People.Sweep.Profile()
	require.NoError(err)
	require.NotEqual(runningProfile.Fingerprint, savedProfile.Fingerprint)
	saved.People.Sweep.Provider.Name = "default"
	require.NoError(saved.Save())
	st := testutil.NewSQLiteTestStore(t)
	for _, profile := range []peoplesweep.ProviderProfile{runningProfile, savedProfile} {
		_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
		require.NoError(err)
		require.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
			ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(),
			DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode, ModelVersion: profile.Model,
		}))
		_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "test")
		require.NoError(err)
	}
	adapter := &storeAPIAdapter{store: st}
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: startup, Store: adapter, Logger: slog.New(slog.DiscardHandler),
		DaemonVersion: Version, OperationGate: api.NewSerialOperationGate(),
	})
	daemon := httptest.NewServer(srv.Router())
	defer daemon.Close()
	writeStatsHTTPDaemonRuntime(t, startup.Data.DataDir, daemon)
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	cfg = &saved
	deps := defaultPersonProviderCommandDeps()
	// Execute the old subprocess route in-process if removal still uses it.
	// Both paths use the real command/store; the test never launches a host daemon.
	deps.proxy = func(command *cobra.Command, args []string, _ map[string]string) error {
		argv, err := daemonCLIArgsFromCobra(command, args)
		if err != nil {
			return err
		}
		_, err = executePersonProviderCommand(t, localPersonProviderDeps(saved.People.Sweep, st, nil), argv[2:]...)
		return err
	}
	runner, err := newProductionStructuredRunner(startup, st)
	require.NoError(err)
	request := peoplesweep.StructuredRequest{
		ProgramID: "removal-test", ProgramVersion: "1", InputText: "synthetic input", SchemaName: "removal_test",
		Sources:         []peoplesweep.SourceDescriptor{{Class: peoplesweep.SourceConversationText, ObservedOn: "2025-06-01"}},
		JSONSchema:      []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
		MaxOutputTokens: 16,
	}
	_, err = runner.RunStructured(t.Context(), request)
	require.NoError(err)
	require.Equal(int64(1), calls.Load())
	// Reject a stale config revision without revoking either policy.
	before, err := config.ReadConfigFile(saved.ConfigFilePath())
	require.NoError(err)
	require.NoError(os.WriteFile(saved.ConfigFilePath(), append(before.Content, []byte("\n# concurrent edit\n")...), 0o600))
	readCurrent := deps.readConfigFile
	deps.readConfigFile = func() (config.ConfigFile, error) { return before, nil }
	_, err = executePersonProviderCommand(t, deps, "remove", "beta")
	require.ErrorContains(err, "config file changed")
	for _, profile := range []peoplesweep.ProviderProfile{runningProfile, savedProfile} {
		active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
		require.NoError(err)
		assert.True(active, profile.Model)
	}
	deps.readConfigFile = readCurrent
	output, err := executePersonProviderCommand(t, deps, "remove", "beta", "--json")
	require.NoError(err)
	var removed personProviderRemoveOutput
	require.NoError(json.Unmarshal([]byte(output), &removed))
	assert.Equal(personProviderRemoveOutput{Name: "beta", Removed: true, DaemonRestartRequired: true}, removed)
	snapshot, err := config.ReadConfigFile(saved.ConfigFilePath())
	require.NoError(err)
	current, err := config.LoadConfigFile(snapshot, saved.HomeDir)
	require.NoError(err)
	assert.NotContains(current.People.Sweep.Providers, "beta")
	for _, profile := range []peoplesweep.ProviderProfile{runningProfile, savedProfile} {
		active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
		require.NoError(err)
		assert.False(active, profile.Model)
		checked, err := st.HasSuccessfulPersonInferenceCheck(t.Context(), profile.Fingerprint)
		require.NoError(err)
		assert.False(checked, profile.Model)
	}
	_, err = runner.RunStructured(t.Context(), request)
	require.Error(err)
	assert.Equal(int64(1), calls.Load(), "removal must prevent further provider requests")
}
