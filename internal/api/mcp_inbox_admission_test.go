package api

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
)

func TestInboxLiveDescriptorUsesRegisteredRouteAndCurrentGrant(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	backend := &inboxHTTPStore{}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	_, secret, grant, err := srv.agentGrants.Issue("archive-only", []agentgrant.Permission{agentgrant.PermissionInboxRead, agentgrant.PermissionInboxArchive}, []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "owner@example.test"}}, time.Time{})
	requirements.NoError(err)
	for _, delegated := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
		if delegated {
			r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		} else {
			r.Header.Set("Authorization", "Bearer synthetic-owner-key")
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.Equal("no-store", w.Header().Get("Cache-Control"))
		var descriptor struct {
			Version   int  `json:"version"`
			Delegated bool `json:"delegated"`
			Routes    []struct {
				OperationID       string   `json:"operation_id"`
				Method            string   `json:"method"`
				Path              string   `json:"path"`
				RequestProperties []string `json:"request_properties"`
			} `json:"routes"`
			InboxOperations []string `json:"inbox_operations"`
		}
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &descriptor))
		assertions.Equal(1, descriptor.Version)
		assertions.Equal(delegated, descriptor.Delegated)
		var inboxRoute *struct {
			OperationID       string   `json:"operation_id"`
			Method            string   `json:"method"`
			Path              string   `json:"path"`
			RequestProperties []string `json:"request_properties"`
		}
		for i := range descriptor.Routes {
			if descriptor.Routes[i].OperationID == "controlInbox" {
				inboxRoute = &descriptor.Routes[i]
				break
			}
		}
		requirements.NotNil(inboxRoute, "the signed control route is registered for the current caller")
		assertions.Equal(http.MethodPost, inboxRoute.Method)
		assertions.Equal("/api/v1/inbox/control", inboxRoute.Path)
		for _, field := range []string{"operation", "target", "source", "expected", "preview_token", "idempotency_key", "receipt_id", "dry_run", "tags", "destination", "origin_folder"} {
			assertions.Contains(inboxRoute.RequestProperties, field)
		}
		assertions.Contains(descriptor.InboxOperations, "archive")
		if delegated {
			assertions.NotContains(descriptor.InboxOperations, "tags")
			assertions.NotContains(descriptor.InboxOperations, "set-read")
		}
	}
	assertions.Zero(backend.calls, "discovery never observes a provider or dispatches control")
	requirements.True(srv.agentGrants.Revoke(grant.ID))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
	r.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	assertions.Equal(http.StatusUnauthorized, w.Code)
}
