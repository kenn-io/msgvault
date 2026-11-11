package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXContentPendingFencesEveryAuthenticatedReader(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	at := time.Now().UTC()
	saved, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: source.ID, OwnerID: "1001", PostID: "100", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100"}, Text: "fixture constellation text", CreatedAt: at, FetchedAt: at, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", Username: "example_author", FetchedAt: at}})
	_testRequire.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.HomeDir = cfg.Data.DataDir
	cfg.Server.APIKey = "example-owner-key"
	cfg.Server.AgentAccess = true
	cfg.Server.RemoteClients = []config.RemoteClientConfig{{ClientID: "example-reader", APIKey: "example-reader-key"}}
	// The callback is the embedding host's public readiness contract. Its data
	// comes from the real Store; the test does not emulate cleanup or a handler.
	guard := func(ctx context.Context) (func(), error) {
		pending, err := st.XCleanupPendingContext(ctx)
		if err != nil {
			return nil, err
		}
		if pending {
			return nil, errors.New("cleanup pending")
		}
		return func() {}, nil
	}
	server := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger(), ContentReadGuard: guard})
	_, token, _, err := server.agentGrants.Issue("example-agent", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	_testRequire.NoError(err)
	credentials := []struct{ name, header, key string }{{"owner", "X-Api-Key", cfg.Server.APIKey}, {"agent", apiprotocol.AgentTokenHeader, token}, {"remote-reader", "X-Api-Key", "example-reader-key"}}
	read := func(header, key, path string) *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		request.Header.Set(header, key)
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, request)
		return response
	}
	path := fmt.Sprintf("/api/v1/cli/message?id=%d", saved.MessageID)
	for _, credential := range credentials {
		response := read(credential.header, credential.key, path)
		_testRequire.Equal(http.StatusOK, response.Code, response.Body.String())
		_testAssert.Contains(response.Body.String(), "fixture constellation")
	}
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_, err = st.RemoveXPostContentContext(t.Context(), source.ID, "100", "removed")
	_testRequire.NoError(err)
	_testRequire.NoError(execution.Release())
	for _, credential := range credentials {
		t.Run(credential.name, func(t *testing.T) {
			response := read(credential.header, credential.key, path)
			assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			assert.NotContains(t, response.Body.String(), "fixture constellation")
		})
	}
	for _, queryPath := range []string{"/api/v1/query", "/api/v1/query/archive"} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, queryPath, strings.NewReader(`{"sql":"SELECT snippet FROM messages"}`))
		request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, request)
		_testAssert.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
		_testAssert.Contains(response.Body.String(), "x_content_cleanup_pending")
	}
	unauthorized := read("X-Api-Key", "", path)
	_testAssert.Equal(http.StatusUnauthorized, unauthorized.Code, unauthorized.Body.String())
	health := read("X-Api-Key", cfg.Server.APIKey, "/health")
	_testAssert.Equal(http.StatusOK, health.Code, health.Body.String())
}
