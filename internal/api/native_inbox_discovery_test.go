package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/store"
)

func TestMCPEventResourcesOwnerDiscoveryAndDefaultOff(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	srv, f := newMCPEventsAPIFixture(t)
	resource, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: f.Source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier, Known: true, MessageRead: true}))
	require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: f.Source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier, Kind: "gmail_label", Complete: true, Scopes: []store.NativeMessageScope{{ProviderID: "label-a", DisplayName: "Alpha"}, {ProviderID: "label-b", DisplayName: "Beta"}}}))
	owner := http.Header{"Authorization": {"Bearer " + testSessionAPIKey}}
	query := `{"kind":"inbox","source_id":"` + strconv.FormatInt(f.Source.ID, 10) + `","limit":1}`
	response := mcpEventsAPIRequest(t, srv, "resources", query, owner)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal("no-store", response.Header().Get("Cache-Control"))
	var first mcpevents.ResourcePage
	require.NoError(json.Unmarshal(response.Body.Bytes(), &first))
	require.Len(first.Resources, 1)
	require.NotEmpty(first.NextCursor)
	assert.Equal("gmail_label", first.Resources[0].ScopeKind)
	assert.Equal([]string{"msgvault.inbox_message_archived"}, first.Resources[0].Capabilities.Events)
	query = `{"kind":"inbox","source_id":"` + strconv.FormatInt(f.Source.ID, 10) + `","limit":1,"cursor":"` + first.NextCursor + `"}`
	response = mcpEventsAPIRequest(t, srv, "resources", query, owner)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var second mcpevents.ResourcePage
	require.NoError(json.Unmarshal(response.Body.Bytes(), &second))
	require.Len(second.Resources, 1)
	assert.NotEqual(first.Resources[0].ID, second.Resources[0].ID)
	assert.Empty(second.NextCursor)
	for _, body := range []string{`{"kind":"unknown"}`, `{"kind":"inbox","limit":101}`, `{"kind":"inbox","source_id":"01"}`, `{"kind":"inbox","principal":"foreign-owner"}`, `{"kind":"source","limit":1,"cursor":"` + first.NextCursor + `"}`} {
		response = mcpEventsAPIRequest(t, srv, "resources", body, owner)
		assert.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	}
	response = mcpEventsAPIRequest(t, srv, "resources", `{"kind":"source"}`, nil)
	assert.Equal(http.StatusUnauthorized, response.Code)
	session := loginSavedViewSession(t, srv)
	response = mcpEventsAPIRequest(t, srv, "resources", `{"kind":"source"}`, session.mutationHeaders())
	assert.Equal(http.StatusForbidden, response.Code)
	registry := agentgrant.NewRegistry()
	t.Cleanup(registry.Close)
	srv.agentGrants = registry
	_, token, _, err := registry.Issue("synthetic-discovery-agent", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}, []agentgrant.SourceRef{{ID: 1, Type: "gcal", Identifier: "calendar@example.net"}}, time.Time{})
	require.NoError(err)
	response = mcpEventsAPIRequest(t, srv, "resources", `{"kind":"source"}`, http.Header{apiprotocol.AgentTokenHeader: {token}})
	assert.Equal(http.StatusForbidden, response.Code)
	key := filepath.Join(t.TempDir(), "disabled-events.key")
	disabled, err := mcpevents.New(context.Background(), f.Store, mcpevents.Options{Enabled: false, OwnerKey: testSessionAPIKey, KeyPath: key})
	require.NoError(err)
	srv.SetMCPEvents(disabled)
	response = mcpEventsAPIRequest(t, srv, "resources", `{"kind":"inbox"}`, owner)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var page mcpevents.ResourcePage
	require.NoError(json.Unmarshal(response.Body.Bytes(), &page))
	require.Len(page.Resources, 2)
	for _, r := range page.Resources {
		assert.Empty(r.Capabilities.Events)
		assert.Equal("events_disabled", r.Capabilities.UnavailableReason)
	}
	assert.NoFileExists(key, "discovery must not enable capture or create signing credentials")
}
