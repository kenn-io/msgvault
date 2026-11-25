package api

import (
	"context"
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
	"go.kenn.io/msgvault/internal/hex"
)

type fakeHexImportStore struct {
	*mockStore

	calls int
	run   func(context.Context, hex.RemoteRequest) (hex.RemoteResult, error)
}

func (s *fakeHexImportStore) ImportHex(ctx context.Context, req hex.RemoteRequest) (hex.RemoteResult, error) {
	s.calls++
	if s.run != nil {
		return s.run(ctx, req)
	}
	return hex.RemoteResult{Status: "registered", SourceID: 1}, nil
}
func TestHexImportBoundary(t *testing.T) {
	for _, tt := range []struct {
		name, body, key, media string
		status                 int
		delegated              bool
	}{
		{"owner", `{"action":"register","source":{"identifier":"recorder","account_email":"user@example.com"}}`, meetingImportTestAPIKey, "application/json", 200, false},
		{"delegated", `{"action":"register","source":{"identifier":"recorder","account_email":"user@example.com"}}`, "", "application/json", 401, true},
		{"unauthorized", `{}`, "", "application/json", 401, false},
		{"blank entry", `{"action":"upsert","source":{"identifier":"recorder","account_email":"user@example.com"},"meeting":{"manifest":{"schema_version":1,"id":"call","status":"complete","created_at_ms":1},"entries":[{"source":"system","start_ms":0,"end_ms":1,"text":" "}],"transcript_state":"final"}}`, meetingImportTestAPIKey, "application/json", 422, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			st := &fakeHexImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
			server := NewServer(&config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey, AgentAccess: tt.delegated}}, st, nil, testLogger())

			var secret string
			if tt.delegated {
				registry := agentgrant.NewRegistry()
				server.agentGrants = registry
				_, token, _, err := registry.Issue("synthetic agent", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "user@example.com"}}, time.Time{})
				require.NoError(t, err)
				secret = token
				health := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil)
				health.Header.Set(apiprotocol.AgentTokenHeader, secret)
				healthOut := httptest.NewRecorder()
				server.Router().ServeHTTP(healthOut, health)
				require.Equal(t, http.StatusOK, healthOut.Code, "delegated token authenticates before endpoint denial")
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/hex", strings.NewReader(tt.body))
			req.Header.Set("X-Api-Key", tt.key)
			req.Header.Set("Content-Type", tt.media)
			if tt.delegated {
				req.Header.Set(apiprotocol.AgentTokenHeader, secret)
			}
			out := httptest.NewRecorder()
			server.Router().ServeHTTP(out, req)
			assert.Equal(tt.status, out.Code)
			if tt.status != 200 {
				assert.Zero(st.calls)
				assert.NotContains(out.Body.String(), "private")
			} else {
				assert.Equal(1, st.calls)
			}
		})
	}
}
