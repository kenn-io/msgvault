package api

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
)

func TestInboxReadDiscoveryRequiresNativeReader(t *testing.T) {
	for _, tc := range []struct {
		name      string
		store     MessageStore
		available bool
	}{
		{"absent", &mockStore{}, false},
		{"registered", &inboxCandidateHTTPStore{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, tc.store, nil, testLogger())
			req := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
			req.Header.Set("Authorization", "Bearer synthetic-owner-key")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, req)
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			var descriptor apiprotocol.MCPCapabilities
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &descriptor))
			assertions.Equal(tc.available, descriptor.HasInboxCandidatesContract())
			assertions.Equal(tc.available, descriptor.HasInboxContextContract())
			assertions.Equal("no-store", response.Header().Get("Cache-Control"))
		})
	}
}

func TestInboxReadRoutesWithoutNativeReaderReturnUnavailable(t *testing.T) {
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, &mockStore{}, nil, testLogger())
	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"candidates", http.MethodGet, "/api/v1/inbox/candidates?source_id=1&source_type=gmail&source_identifier=reader%40example.test&account_id=reader%40example.test&scope=message", ""},
		{"context", http.MethodPost, "/api/v1/inbox/context", `{"target":{"source_id":1,"source_type":"gmail","source_identifier":"reader@example.test","account_id":"reader@example.test","scope":"message","item_id":1,"provider_id":"native-unavailable"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)

			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer synthetic-owner-key")
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, req)
			assertions.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
			var failure ErrorResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &failure))
			assertions.Equal("inbox_unavailable", failure.Error)
			assertions.Equal("no-store", response.Header().Get("Cache-Control"))
		})
	}
}
