package cmd

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestSyncXRemoveNoBudgetUsesSelectedDaemon(t *testing.T) {
	f := newXCleanupFixture(t)
	f.cfg.Server.APIKey = "example-owner-key"
	f.cfg.X.Accounts = []config.XAccountConfig{{UserID: "1001", MonthlyBudgetMicros: 0}}
	generation := int64(2)
	credentialError := "invalid_grant"
	modelError := store.XModelErrorResourceShape
	_, err := f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation, CredentialError: &credentialError, ModelError: &modelError})
	require.NoError(t, err)
	before, err := f.st.LoadXControlContext(t.Context(), f.source)
	require.NoError(t, err)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer provider.Close()
	deps := xCommandDeps{apiBaseURL: provider.URL + "/2", httpClient: provider.Client()}
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	adapter := &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler), xDeps: &deps, xCleanup: cleaner}
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.Handle("/", server.Router())
	host := httptest.NewServer(mux)
	defer host.Close()
	bridge := &xCollectionFixture{cfg: f.cfg, server: host}
	out, _, err := bridge.run(t, bridge.clientConfig(t), "--source-id", fmt.Sprint(f.source), "--remove-post", "100", "--json")
	require.NoError(t, err, out)
	var work store.XRemovalWork
	require.NoError(t, json.Unmarshal([]byte(out), &work))
	assert.False(t, work.Pending)
	assert.Equal(t, "100", work.CanonicalID)
	body, err := f.st.GetMessageBodyText(f.message)
	require.NoError(t, err)
	assert.Empty(t, body)
	target, err := f.st.ResolveXPostContext(t.Context(), f.source, "102")
	require.NoError(t, err)
	assert.Equal(t, "removed", target.Availability)
	after, err := f.st.LoadXControlContext(t.Context(), f.source)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Zero(t, calls.Load())
	_, err = os.Stat(f.cfg.TokensDir())
	assert.True(t, os.IsNotExist(err))
	// The real owner HTTP boundary rejects workstation state before dispatch.
	for _, req := range []api.CLIRunRequest{{Args: []string{"sync-x", "--source-id", fmt.Sprint(f.source), "--remove-post", "100"}, Cwd: "/tmp"}, {Args: []string{"sync-x", "--source-id", fmt.Sprint(f.source), "--remove-post", "100"}, Env: map[string]string{"MSGVAULT_X_TOKEN": "example-secret"}}} {
		data, err := json.Marshal(req)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, host.URL+"/api/v1/cli/run", bytes.NewReader(data))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
		response, err := host.Client().Do(request)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, http.StatusBadRequest, response.StatusCode)
	}
	// Delegated grants cannot authorize local removal through the real route.
	grantRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, host.URL+"/api/v1/agent-tokens", strings.NewReader(fmt.Sprintf(`{"label":"example-agent","permissions":["draft.create"],"source_ids":[%d]}`, f.source)))
	require.NoError(t, err)
	grantRequest.Header.Set("Content-Type", "application/json")
	grantRequest.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
	response, err := host.Client().Do(grantRequest)
	require.NoError(t, err)
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusCreated, response.StatusCode, string(data))
	var grant struct {
		Secret string `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(data, &grant))
	encoded, err := json.Marshal(api.CLIRunRequest{Args: []string{"sync-x", "--source-id", fmt.Sprint(f.source), "--remove-post", "900"}})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, host.URL+"/api/v1/cli/run", bytes.NewReader(encoded))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(apiprotocol.AgentTokenHeader, grant.Secret)
	response, err = host.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusBadRequest, response.StatusCode)
	_, err = f.st.ResolveXPostContext(t.Context(), f.source, "900")
	assert.ErrorIs(t, err, store.ErrXPostNotFound)
	assert.Zero(t, calls.Load())
}
