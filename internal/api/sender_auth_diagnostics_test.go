package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSenderAuthDiagnosticsOwnerBoundary(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: f.Store, Engine: query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	router := srv.Router()
	request := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/sender-auth/diagnostics?source_id=%d&limit=1&suggest=true", f.Source.ID), nil)
		req.Header.Set("Authorization", "Bearer "+auth)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	owner := request("owner")
	must.Equal(http.StatusOK, owner.Code, owner.Body.String())
	checks.Contains(owner.Body.String(), `"scanned":0`)

	_, token, _, err := srv.agentGrants.Issue("diagnostics denied", []agentgrant.Permission{agentgrant.PermissionStatsRead, agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: f.Source.ID, Type: f.Source.SourceType, Identifier: f.Source.Identifier}}, time.Time{})
	must.NoError(err)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/sender-auth/diagnostics?source_id=%d&limit=1&suggest=true", f.Source.ID), nil)
	req.Header.Set("X-Msgvault-Agent-Token", token)
	denied := httptest.NewRecorder()
	router.ServeHTTP(denied, req)
	checks.Equal(http.StatusUnauthorized, denied.Code, denied.Body.String())
}
