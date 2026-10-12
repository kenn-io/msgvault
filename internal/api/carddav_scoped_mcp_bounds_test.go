package api

import (
	"context"
	"encoding/json/jsontext"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestScopedCardDAVMCPBoundsOriginalRequestBeforeConfirmation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { assertions.NoError(srv.Shutdown(context.Background())) })
	var requests atomic.Int32
	router := srv.Router()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, scopedCardDAVPublicationPath) {
			requests.Add(1)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: agentTokenTestAPIKey, AllowInsecure: true, HTTPClient: daemon.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(client.Close()) })
	session := scopedCardDAVMCPSession(t, client)
	for _, tool := range []string{"approve_scoped_carddav_publication", "reconcile_scoped_carddav_publication"} {
		for _, field := range []string{"approval_token", "idempotency_key"} {
			for _, tc := range []struct {
				name       string
				value      string
				wantPrompt bool
			}{
				{"ascii_limit", strings.Repeat("a", 256), true},
				{"utf8_limit", strings.Repeat("é", 128), true},
				{"ascii_oversize", strings.Repeat("a", 257), false},
				{"utf8_oversize", strings.Repeat("é", 129), false},
			} {
				t.Run(tool+"/"+field+"/"+tc.name, func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					args := map[string]any{"person_id": int64(1), "approval_token": "synthetic-original-token", "idempotency_key": "synthetic-original-key"}
					args[field] = tc.value
					result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: args})
					requirements.NoError(err)
					requirements.NotNil(result)
					assertions.Equal(tc.wantPrompt, result.NeedsInput(), "enforce the byte bound before requesting client confirmation")
					assertions.Equal(!tc.wantPrompt, result.IsError)
					assertions.Zero(requests.Load(), "neither invalid nor unconfirmed input may reach the daemon operation")
				})
			}
		}
	}
}

func TestScopedCardDAVMCPRejectsRoundedPersonID(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { assertions.NoError(srv.Shutdown(context.Background())) })
	var requests atomic.Int32
	router := srv.Router()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, scopedCardDAVPublicationPath) {
			requests.Add(1)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: agentTokenTestAPIKey, AllowInsecure: true, HTTPClient: daemon.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(client.Close()) })
	session := scopedCardDAVMCPSession(t, client)
	for _, tool := range []string{"preview_scoped_carddav_publication", "approve_scoped_carddav_publication", "reconcile_scoped_carddav_publication"} {
		t.Run(tool, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			args := `{"person_id":1.0000000000000001}`
			if tool != "preview_scoped_carddav_publication" {
				args = `{"person_id":1.0000000000000001,"approval_token":"synthetic-original-token","idempotency_key":"synthetic-original-key"}`
			}
			before := requests.Load()
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: jsontext.Value(args)})
			requirements.NoError(err)
			requirements.NotNil(result)
			assertions.True(result.IsError, "the original fractional ID must not become a different integer request")
			assertions.False(result.NeedsInput(), "reject the original ID before requesting confirmation")
			assertions.Equal(before, requests.Load(), "invalid raw ID must not reach the daemon operation")
		})
	}
}
