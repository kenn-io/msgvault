package api

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
)

func TestInboxLiveDescriptorUsesRegisteredRouteAndCurrentGrant(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	backend := &inboxHTTPStore{}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	_, secret, grant, err := srv.agentGrants.Issue("archive-only", []agentgrant.Permission{agentgrant.PermissionInboxRead, agentgrant.PermissionInboxArchive}, []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "owner@example.test"}}, time.Time{})
	require.NoError(err)
	for _, delegated := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
		if delegated {
			r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		} else {
			r.Header.Set("Authorization", "Bearer synthetic-owner-key")
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		require.Equal(http.StatusOK, w.Code, w.Body.String())
		assert.Equal("no-store", w.Header().Get("Cache-Control"))
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
		require.NoError(json.Unmarshal(w.Body.Bytes(), &descriptor))
		assert.Equal(1, descriptor.Version)
		assert.Equal(delegated, descriptor.Delegated)
		require.Len(descriptor.Routes, 1)
		assert.Equal("controlInbox", descriptor.Routes[0].OperationID)
		assert.Equal(http.MethodPost, descriptor.Routes[0].Method)
		assert.Equal("/api/v1/inbox/control", descriptor.Routes[0].Path)
		for _, field := range []string{"operation", "target", "source", "expected", "preview_token", "idempotency_key", "receipt_id", "dry_run", "tags", "destination", "origin_folder"} {
			assert.Contains(descriptor.Routes[0].RequestProperties, field)
		}
		assert.Contains(descriptor.InboxOperations, "archive")
		if delegated {
			assert.NotContains(descriptor.InboxOperations, "tags")
			assert.NotContains(descriptor.InboxOperations, "set-read")
		}
	}
	assert.Zero(backend.calls, "discovery never observes a provider or dispatches control")
	require.True(srv.agentGrants.Revoke(grant.ID))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
	r.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	assert.Equal(http.StatusUnauthorized, w.Code)
}
