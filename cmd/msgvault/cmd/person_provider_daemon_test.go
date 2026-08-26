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

	config     peoplesweep.Config
	httpClient *http.Client
}

func (s *inProcessPersonProviderDaemonStore) RunCLICommand(
	ctx context.Context,
	req api.CLIRunRequest,
	emit func(api.CLIRunEvent) error,
) error {
	deps := localPersonProviderDeps(s.config, s.store, nil)
	deps.newChecker = func(
		config peoplesweep.Config,
		consent personProviderStore,
	) (personProviderChecker, error) {
		return peoplesweep.NewRunner(
			config,
			consent,
			peoplesweep.NewOpenAICompatibleTransport(s.httpClient),
			func(name string) (string, bool) {
				value, ok := req.Env[name]
				return value, ok
			},
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
		w.Header().Set("x-request-id", "req-daemon")
		_, _ = io.WriteString(w, `{
			"model":"test-model",
			"choices":[{"message":{"content":"{\"ok\":true}"}}],
			"usage":{"prompt_tokens":9,"completion_tokens":2}
		}`)
	}))
	t.Cleanup(provider.Close)

	peopleConfig := personProviderTestConfig()
	peopleConfig.Provider.Endpoint = provider.URL + "/v1"
	st := testutil.NewSQLiteTestStore(t)
	daemonConfig := &config.Config{People: peoplesweep.PeopleConfig{Sweep: peopleConfig}}
	daemonStore := &inProcessPersonProviderDaemonStore{
		storeAPIAdapter: &storeAPIAdapter{store: st},
		config:          peopleConfig,
		httpClient:      provider.Client(),
	}
	logger := slog.New(slog.DiscardHandler)
	daemon := api.NewServerWithOptions(api.ServerOptions{
		Config: daemonConfig, Store: daemonStore, Logger: logger,
		OperationGate: api.NewSerialOperationGate(),
	})
	daemonHTTP := httptest.NewServer(daemon.Router())
	t.Cleanup(daemonHTTP.Close)

	frontendConfig := *daemonConfig
	frontendConfig.Remote = config.RemoteConfig{URL: daemonHTTP.URL, AllowInsecure: true}
	withStoreResolverConfig(t, &frontendConfig)
	t.Setenv("TEST_PROVIDER_KEY", "caller-key")
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
	assert.Equal("Bearer caller-key", captured.Authorization)
	assert.Equal("/v1/chat/completions", captured.Path)
	assert.Equal("test-model", captured.Body["model"])
	messages, ok := captured.Body["messages"].([]any)
	require.True(ok)
	require.Len(messages, 2)
	message, ok := messages[1].(map[string]any)
	require.True(ok)
	assert.Equal("Return an object with ok set to true.", message["content"])
	assert.NotContains(string(mustJSON(t, captured.Body)), "archive")

	_, err = executePersonProviderCommand(t, deps, "revoke", "--json")
	require.NoError(err)
	_, err = executePersonProviderCommand(t, deps, "check", "--json")
	require.ErrorContains(err, "active exact consent")
	assert.Equal(int64(1), requestCount.Load(), "revoked check must not reach the provider")
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
