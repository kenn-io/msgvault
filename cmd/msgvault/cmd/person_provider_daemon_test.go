package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
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
	withStoreResolverConfig(t, &frontend)
	deps := defaultPersonProviderCommandDeps()
	callerHasKey := false
	deps.setup.lookupEnv = func(name string) (string, bool) {
		assert.Equal(keyName, name)
		return secret, callerHasKey
	}
	var output bytes.Buffer
	command := &cobra.Command{Use: "setup"}
	command.SetContext(t.Context())
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
	withStoreResolverConfig(t, &frontendConfig)
	const environmentSecretCanary = "caller-key-never-in-daemon-request"
	t.Setenv("TEST_PROVIDER_KEY", environmentSecretCanary)
	deps := defaultPersonProviderCommandDeps()

	_, err := executePersonProviderCommand(t, deps, "consent", "--yes", "--json")
	require.NoError(err)
	output, err := executePersonProviderCommand(t, deps, "check", "--json")
	require.NoError(err)
	assert.JSONEq(`{
		"ok":true,
		"provider_request_id":"req-daemon",
		"model":"test-model",
		"usage":{"input_tokens":9,"output_tokens":2}
	}`, output)

	captured := <-requests
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
	for range 2 {
		req := <-requestsToDaemon
		wire := mustJSON(t, req)
		assert.Empty(req.Env)
		assert.NotContains(string(wire), environmentSecretCanary)
		assert.NotContains(string(<-rawDaemonBodies), environmentSecretCanary)
	}
	assert.NotContains(output, environmentSecretCanary)
	assert.NotContains(daemonLogs.String(), environmentSecretCanary)

	_, err = executePersonProviderCommand(t, deps, "revoke", "--json")
	require.NoError(err)
	output, err = executePersonProviderCommand(t, deps, "check", "--json")
	require.NoError(err)
	assert.JSONEq(`{
		"ok":true,
		"provider_request_id":"req-daemon",
		"model":"test-model",
		"usage":{"input_tokens":9,"output_tokens":2}
	}`, output)
	assert.Equal(int64(2), requestCount.Load(), "synthetic checks bypass archive consent")
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
	withStoreResolverConfig(t, &frontendConfig)
	deps := defaultPersonProviderCommandDeps()
	output, err := executePersonProviderCommand(t, deps, "check", "stored", "--json")
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
