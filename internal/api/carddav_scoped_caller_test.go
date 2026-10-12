package api

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestScopedCardDAVHandlerPreservesCallerAdmission(t *testing.T) {
	for _, mode := range []string{"missing_headers", "invalid_agent_header", "caller_denied"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := testutil.NewTestStore(t)
			srv := NewServerWithOptions(ServerOptions{
				Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}},
				Store:  st, Logger: testLogger(),
			})
			t.Cleanup(func() { assert.NoError(srv.Shutdown(context.Background())) })
			var admitted Operation
			handler := srv.Handler(func(w http.ResponseWriter, _ *http.Request, operation Operation) bool {
				admitted = operation
				if mode == "caller_denied" {
					w.WriteHeader(http.StatusForbidden)
					return false
				}
				return true
			})
			call := func(method, path string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(`{"approval_token":"synthetic-review","idempotency_key":"synthetic-request"}`))
				r.Header.Set("Content-Type", "application/json")
				if mode == "invalid_agent_header" {
					r.Header.Set(apiprotocol.AgentTokenHeader, "invalid-synthetic-daemon-token")
				}
				if mode == "caller_denied" {
					r.Header.Set("Authorization", "Bearer "+agentTokenTestAPIKey)
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			response := call(http.MethodGet, "/api/v1/mcp/capabilities")
			assert.Equal("getMCPCapabilities", admitted.ID)
			if mode == "caller_denied" {
				assert.Equal(http.StatusForbidden, response.Code)
				assert.Empty(response.Body.String())
				for _, endpoint := range []struct{ method, suffix, operation string }{
					{http.MethodGet, "preview", "previewScopedCardDAVPublication"},
					{http.MethodPost, "approve", "approveScopedCardDAVPublication"},
					{http.MethodPost, "reconcile", "reconcileScopedCardDAVPublication"},
				} {
					denied := call(endpoint.method, "/api/v1/carddav/scoped/publications/1/"+endpoint.suffix)
					assert.Equal(endpoint.operation, admitted.ID)
					assert.Equal(http.StatusForbidden, denied.Code)
					assert.Empty(denied.Body.String(), "caller rejection must stop before operation admission")
				}
				return
			}
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var descriptor apiprotocol.MCPCapabilities
			require.NoError(json.Unmarshal(response.Body.Bytes(), &descriptor))
			assert.False(descriptor.Delegated)
			var operations []string
			for _, route := range descriptor.Routes {
				operations = append(operations, route.OperationID)
			}
			assert.ElementsMatch([]string{"previewIdentityOperation", "applyIdentityOperation", "getIdentityOperationReceipt", "reconcileScopedCardDAVPublication"}, operations, "one endpoint preserves identity contracts and CardDAV recovery")
			assert.True(descriptor.HasIdentityPreviewContract())
			assert.True(descriptor.HasIdentityApplyContract())
			assert.True(descriptor.HasIdentityReceiptContract())
		})
	}
}
