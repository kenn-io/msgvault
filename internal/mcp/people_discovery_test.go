package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query/querytest"
)

type discoveryPeopleBackend struct {
	request peoplebrowser.SemanticPeopleRequest
	err     error
}

func (b *discoveryPeopleBackend) SearchSemanticPeople(_ context.Context, request peoplebrowser.SemanticPeopleRequest) (*peoplebrowser.SemanticPeoplePage, error) {
	b.request = request
	return &peoplebrowser.SemanticPeoplePage{Results: []peoplebrowser.SemanticPersonMatch{{PersonID: 7, DisplayName: new("Alex Example"), VCardUID: "person-7", Score: .9}}}, b.err
}

func (b *discoveryPeopleBackend) ListOrganizations(context.Context, string, int, int) (*peoplebrowser.OrganizationPage, error) {
	return &peoplebrowser.OrganizationPage{Organizations: []peoplebrowser.OrganizationSummary{{ID: 13, Name: "Example Cooperative"}}, Total: 2, Limit: 1, Offset: 0, HasMore: true}, b.err
}

func discoveryToolOptions(b *discoveryPeopleBackend) ServeOptions {
	return ServeOptions{Engine: &querytest.MockEngine{}, SemanticPeopleBackend: b, OrganizationBackend: b}
}

func TestPersonDiscoveryMCPCatalogAndDelegatedBoundary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &discoveryPeopleBackend{}
	opts := discoveryToolOptions(backend)
	listed := toolsByName(t, rawListTools(t, opts, false))
	require.Contains(listed, "search_semantic_people")
	require.Contains(listed, "list_organizations")
	annotations, ok := listed["search_semantic_people"]["annotations"].(map[string]any)
	require.True(ok)
	assert.Equal(true, annotations["readOnlyHint"])
	assert.Equal(true, annotations["openWorldHint"])
	assert.Contains(listed["search_semantic_people"]["description"], "embedding provider")
	without := toolsByName(t, rawListTools(t, ServeOptions{Engine: &querytest.MockEngine{}}, false))
	assert.NotContains(without, "search_semantic_people")
	assert.NotContains(without, "list_organizations")
	opts.DelegatedOnly = true
	opts.GrantPermissions = []string{"search.read", "message.read", "stats.read"}
	delegated := toolsByName(t, rawListTools(t, opts, false))
	assert.NotContains(delegated, "search_semantic_people")
	assert.NotContains(delegated, "list_organizations")
}

func TestPersonDiscoveryMCPReportsCorpusAndExplicitAvailabilityErrors(t *testing.T) {
	assert := assert.New(t)
	backend := &discoveryPeopleBackend{}
	opts := discoveryToolOptions(backend)
	for _, code := range []string{"person_embedding_consent_required", "index_building", "index_stale", "person_embeddings_disabled"} {
		backend.err = &daemonclient.APIError{Status: 503, Code: code, Message: "specific safe recovery instructions"}
		failed := rawCallTool(t, opts, "search_semantic_people", map[string]any{"query": "equipment"})
		assert.Equal(true, failed["isError"])
		message := toolErrorTextFromResult(t, failed)
		assert.Contains(message, code)
		assert.Contains(message, "specific safe recovery instructions")
	}
}

func TestPersonDiscoveryMCPRejectsInvalidSearch(t *testing.T) {
	assert := assert.New(t)
	for _, args := range []map[string]any{{"query": "equipment", "organization_id": -1}, {"query": "equipment", "limit": 101}, {"query": "equipment", "organization_id": 1.5}, {"query": " "}} {
		failed := rawCallTool(t, discoveryToolOptions(&discoveryPeopleBackend{}), "search_semantic_people", args)
		assert.Equal(true, failed["isError"])
	}
}

func TestPersonDiscoveryMCPDelegatedDirectInvocationCannotReachBackend(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &discoveryPeopleBackend{}
	opts := discoveryToolOptions(backend)
	opts.DelegatedOnly = true
	opts.GrantPermissions = []string{"search.read", "message.read", "stats.read"}
	for _, name := range []string{"search_semantic_people", "list_organizations"} {
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"_meta": modernRequestMeta(), "name": name, "arguments": map[string]any{"query": "equipment"}}})
		require.NoError(err)
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Mcp-Protocol-Version", modernProtocolVersion)
		request.Header.Set("Mcp-Method", "tools/call")
		request.Header.Set("Mcp-Name", name)
		response := httptest.NewRecorder()
		newMCPHTTPServer(opts, HTTPOptions{}).Handler.ServeHTTP(response, request)
		assert.Equal(http.StatusBadRequest, response.Code)
		assert.Contains(response.Body.String(), "unknown tool")
	}
	assert.Empty(backend.request.Query)
}
