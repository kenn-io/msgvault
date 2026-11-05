package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestStoreAPIAdapterVerificationReads(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "verification", "Account", "Your code is 1234AB56")
	requirements.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { assertions.NoError(srv.Shutdown(context.Background())) })
	daemon := httptest.NewServer(srv.Router())
	t.Cleanup(daemon.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: "owner", AllowInsecure: true, HTTPClient: daemon.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(owner.Close()) })
	for _, tc := range []struct {
		permission string
		paths      []string
	}{
		{"message.read", []string{fmt.Sprintf("/messages/%d", id), fmt.Sprintf("/cli/message?id=%d", id), fmt.Sprintf("/cli/message/thread?id=%d", id)}},
		{"search.read", []string{"/messages", "/cli/search?q=code&include_snippet=true"}},
	} {
		issued, err := owner.IssueAgentToken(t.Context(), tc.permission, []string{tc.permission}, []int64{source.ID}, nil, time.Time{})
		requirements.NoError(err)
		for _, path := range tc.paths {
			request := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
			request.Header.Set(apiprotocol.AgentTokenHeader, issued.Secret)
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			assertions.Equal(http.StatusOK, response.Code, response.Body.String())
			assertions.Contains(response.Body.String(), "[verification content masked]", path)
			assertions.NotContains(response.Body.String(), "1234AB56", path)
		}
	}
}
