package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/introops"
	"go.kenn.io/msgvault/internal/store"
)

func TestIntroductionReadsWithoutCapability(t *testing.T) {
	srv, _ := newTestServerWithAgentGrants(t)
	for _, path := range []string{"/api/v1/intros", "/api/v1/intros/unknown"} {
		response := doRequest(t, srv.Router(), http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer owner-key"})
		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		assert.Contains(t, response.Body.String(), "feature_unavailable")
	}
}

func TestIntroductionOpenAPI(t *testing.T) {
	doc := OpenAPIDocument()
	for path, id := range map[string]string{"/api/v1/intros": "listIntroductions", "/api/v1/intros/{id}": "getIntroduction"} {
		require.NotNil(t, doc.Paths[path])
		op := doc.Paths[path].Get
		require.NotNil(t, op)
		assert.Equal(t, id, op.OperationID)
		require.NotNil(t, op.Responses["200"])
		assert.Contains(t, op.Responses["200"].Headers, "Cache-Control")
	}
	schemas := doc.Components.Schemas.Map()
	require.Contains(t, schemas, "IntroductionRecord")
	require.Contains(t, schemas, "IntroductionPage")
	for _, name := range []string{"intro_id", "revision", "status", "source_id", "person_a_id", "person_b_id", "title", "body", "created_by", "cancelled_at", "created_at", "updated_at", "members", "blockers"} {
		assert.Contains(t, schemas["IntroductionRecord"].Properties, name)
	}
	for _, name := range []string{"items", "has_more", "next_after_id"} {
		assert.Contains(t, schemas["IntroductionPage"].Properties, name)
	}
	assert.Equal(t, []any{"prepared", "cancelled"}, schemas["IntroductionRecord"].Properties["status"].Enum)
}

type introductionTestStore struct{ stubSourceStore }

func (*introductionTestStore) ListIntroductionsContext(context.Context, *agentgrant.Grant, store.PersonIntroductionQuery) (introops.Page, error) {
	return introops.Page{}, nil
}
func (*introductionTestStore) GetIntroductionContext(context.Context, *agentgrant.Grant, string) (introops.Record, error) {
	return introops.Record{}, nil
}

func TestIntroductionReadErrorsAndSnapshotCapacity(t *testing.T) {
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true, RemoteClients: []config.RemoteClientConfig{{ClientID: "reader", APIKey: "remote-key"}}}}, &introductionTestStore{}, nil, testLogger())
	t.Cleanup(srv.agentGrants.Close)
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "beeper", Identifier: "account"}}, time.Time{})
	require.NoError(t, err)
	for _, path := range []string{"/api/v1/intros", "/api/v1/intros/id"} {
		for _, tc := range []struct {
			key    string
			status int
		}{{"", http.StatusUnauthorized}, {"remote-key", http.StatusForbidden}} {
			response := doRequest(t, srv.Router(), http.MethodGet, path, nil, map[string]string{"X-Api-Key": tc.key})
			assert.Equal(t, tc.status, response.Code)
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		}
	}
	for _, query := range []string{"source_id=1%ZZ", "after_id=%ZZ", "limit=1;limit=2", "limit=1&limit=%ZZ"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/intros", nil)
		req.URL.RawQuery = query
		req.Header.Set("X-Api-Key", "owner-key")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		assert.Equal(t, http.StatusBadRequest, response.Code, query)
		assert.Contains(t, response.Body.String(), "invalid_args")
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	}
	for range readSnapshotConcurrency {
		srv.readSnapshotSlots <- struct{}{}
	}
	for _, path := range []string{"/api/v1/intros", "/api/v1/intros/id"} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		req.Header.Set("X-Msgvault-Agent-Token", token)
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
		assert.Contains(t, response.Body.String(), "agent_read_busy")
		assert.Len(t, srv.readSnapshotSlots, readSnapshotConcurrency)
	}
}
