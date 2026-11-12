package cmd

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPRelationshipTypesReadRealStore(t *testing.T) {
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "list_relationship_types", nil)
	require.NoError(t, err)
	types := operationOutput[generated.RelationshipTypesResponse](t, result)
	require.NotEmpty(t, types.RelationshipTypes)
	result, err = backend.ExecuteOperation(t.Context(), "get_relationship_type", map[string]any{"id": types.RelationshipTypes[0].ID})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.NotEmpty(t, result.ETag)
}

func TestMCPRelationshipTypeMutationPreservesEmptyAndRevision(t *testing.T) {
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	created, err := backend.ExecuteOperation(t.Context(), "create_relationship_type", map[string]any{"slug": "mentor", "forward_label": "mentor", "reverse_label": "mentee", "description": "initial"})
	require.NoError(t, err)
	record := operationOutput[mcpserver.RelationshipTypeRecord](t, created)
	require.NotEmpty(t, record.ETag)
	updated, err := backend.ExecuteOperation(t.Context(), "update_relationship_type", map[string]any{"id": record.RelationshipType.ID, "etag": record.ETag, "description": ""})
	require.NoError(t, err)
	changed := operationOutput[mcpserver.RelationshipTypeRecord](t, updated)
	assert.Nil(t, changed.RelationshipType.Description)
	stale, err := backend.ExecuteOperation(t.Context(), "delete_relationship_type", map[string]any{"id": record.RelationshipType.ID, "etag": record.ETag})
	require.NoError(t, err)
	assert.True(t, stale.IsError)
	deleted, err := backend.ExecuteOperation(t.Context(), "delete_relationship_type", map[string]any{"id": record.RelationshipType.ID, "etag": changed.ETag})
	require.NoError(t, err)
	assert.False(t, deleted.IsError)
}

func TestMCPRelationshipRecordPresenceAndDirection(t *testing.T) {
	st := testutil.NewTestStore(t)
	first, err := st.EnsureParticipant("first@example.com", "First Example", "example.com")
	require.NoError(t, err)
	second, err := st.EnsureParticipant("second@example.com", "Second Example", "example.com")
	require.NoError(t, err)
	source, _, err := st.CreatePersonFromParticipant(first)
	require.NoError(t, err)
	target, _, err := st.CreatePersonFromParticipant(second)
	require.NoError(t, err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "create_person_relationship", map[string]any{"source_person_id": source.ID, "target_person_id": target.ID, "relationship_type_slug": "parent", "notes": "synthetic"})
	require.NoError(t, err)
	created := operationOutput[mcpserver.PersonRelationshipRecord](t, result)
	require.NotNil(t, created.Relationship.Notes)
	listed, err := backend.ExecuteOperation(t.Context(), "list_person_relationships", map[string]any{"person_id": target.ID})
	require.NoError(t, err)
	views := operationOutput[generated.PersonRelationshipsResponse](t, listed)
	require.Len(t, views.Relationships, 1)
	assert.Equal(t, "incoming", views.Relationships[0].Direction)
	invalid, err := backend.ExecuteOperation(t.Context(), "update_person_relationship", map[string]any{"id": created.Relationship.ID, "etag": created.ETag, "end_date": nil})
	require.NoError(t, err)
	assert.True(t, invalid.IsError)
	ended, err := backend.ExecuteOperation(t.Context(), "update_person_relationship", map[string]any{"id": created.Relationship.ID, "etag": created.ETag, "end_date": "2020"})
	require.NoError(t, err)
	changed := operationOutput[mcpserver.PersonRelationshipRecord](t, ended)
	require.NotNil(t, changed.Relationship.Notes)
	assert.Equal(t, "synthetic", *changed.Relationship.Notes)
	cleared, err := backend.ExecuteOperation(t.Context(), "update_person_relationship", map[string]any{"id": changed.Relationship.ID, "etag": changed.ETag, "notes": nil})
	require.NoError(t, err)
	current := operationOutput[mcpserver.PersonRelationshipRecord](t, cleared)
	assert.Nil(t, current.Relationship.Notes)
	stale, err := backend.ExecuteOperation(t.Context(), "delete_person_relationship", map[string]any{"id": current.Relationship.ID, "etag": created.ETag})
	require.NoError(t, err)
	assert.True(t, stale.IsError)
	deleted, err := backend.ExecuteOperation(t.Context(), "delete_person_relationship", map[string]any{"id": current.Relationship.ID, "etag": current.ETag})
	require.NoError(t, err)
	assert.False(t, deleted.IsError)
}

func TestMCPRelationshipCapabilitiesRequireRouteAndProperties(t *testing.T) {
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: testutil.NewTestStore(t)}, Logger: slog.New(slog.DiscardHandler)}))
	assert.Contains(t, backend.capabilities(), "create_relationship_type")
	assert.Contains(t, backend.capabilities(), "update_person_relationship")
	assert.Empty(t, newDaemonMCPOperations(backend.client, nil).capabilities())
	result, err := backend.ExecuteOperation(t.Context(), "get_person_network", map[string]any{"person_id": 1, "depth": 4})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestMCPRelationshipSDKConfirmationAndNativeGuard(t *testing.T) {
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyRelationshipRecords}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assert.Contains(t, request.Params.Message, "create_relationship_type")
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_relationship_type", Arguments: map[string]any{"slug": "colleague-custom", "forward_label": "colleague", "reverse_label": "colleague", "is_symmetric": true}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Equal(t, 1, approvals)
	types, err := st.ListRelationshipTypesContext(t.Context())
	require.NoError(t, err)
	found := false
	for _, typ := range types {
		if typ.Slug == "colleague-custom" {
			found = true
		}
	}
	assert.True(t, found)
}

func TestMCPRelationshipReadNetworkReviewsAndSeedGuard(t *testing.T) {
	st := testutil.NewTestStore(t)
	first, err := st.EnsureParticipant("network-a@example.com", "Example A", "example.com")
	require.NoError(t, err)
	second, err := st.EnsureParticipant("network-b@example.com", "Example B", "example.com")
	require.NoError(t, err)
	a, _, err := st.CreatePersonFromParticipant(first)
	require.NoError(t, err)
	b, _, err := st.CreatePersonFromParticipant(second)
	require.NoError(t, err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	edge, err := backend.ExecuteOperation(t.Context(), "create_person_relationship", map[string]any{"source_person_id": a.ID, "target_person_id": b.ID, "relationship_type_slug": "parent"})
	require.NoError(t, err)
	require.False(t, edge.IsError)
	graphResult, err := backend.ExecuteOperation(t.Context(), "get_person_network", map[string]any{"person_id": a.ID, "depth": 1})
	require.NoError(t, err)
	graph := operationOutput[generated.PersonNetwork](t, graphResult)
	assert.Equal(t, a.ID, graph.RootPersonID)
	assert.NotEmpty(t, graph.Edges)
	_, err = st.ResolveRelatedValueContext(t.Context(), store.RelatedImport{PersonID: a.ID, RawValue: "Unknown Example", RawType: "friend", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, VCardIdentity: store.VCardIdentity{Property: "RELATED"}, Actor: "system"})
	require.NoError(t, err)
	reviewsResult, err := backend.ExecuteOperation(t.Context(), "list_person_relationship_reviews", map[string]any{"person_id": a.ID, "status": "pending"})
	require.NoError(t, err)
	reviews := operationOutput[generated.RelationshipReviewsResponse](t, reviewsResult)
	require.Len(t, reviews.Reviews, 1)
	assert.Equal(t, "Unknown Example", reviews.Reviews[0].RawRelatedValue)
	typesResult, err := backend.ExecuteOperation(t.Context(), "list_relationship_types", nil)
	require.NoError(t, err)
	types := operationOutput[generated.RelationshipTypesResponse](t, typesResult)
	var seed int64
	for _, typ := range types.RelationshipTypes {
		if typ.Slug == "parent" {
			seed = typ.ID
		}
	}
	require.Positive(t, seed)
	typeResult, err := backend.ExecuteOperation(t.Context(), "get_relationship_type", map[string]any{"id": seed})
	require.NoError(t, err)
	seedType := operationOutput[mcpserver.RelationshipTypeRecord](t, typeResult)
	rejected, err := backend.ExecuteOperation(t.Context(), "delete_relationship_type", map[string]any{"id": seed, "etag": seedType.ETag})
	require.NoError(t, err)
	require.True(t, rejected.IsError)
	assert.Equal(t, "relationship_type_not_deletable", operationOutputError(t, rejected))
}

func operationOutputError(t *testing.T, result *mcpserver.OperationResult) string {
	t.Helper()
	data, err := json.Marshal(result.Output)
	require.NoError(t, err)
	var value struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(data, &value))
	return value.Error
}

func TestMCPRelationshipWriteRefusesInvalidPresence(t *testing.T) {
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"update_relationship_type", map[string]any{"id": 1, "etag": "tag", "description": nil}},
		{"update_person_relationship", map[string]any{"id": 1, "etag": "tag", "end_date": " "}},
		{"update_person_relationship", map[string]any{"id": 1, "etag": "tag", "end_date": nil}},
		{"update_person_relationship", map[string]any{"id": 1, "etag": "tag"}},
	} {
		result, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
		require.NoError(t, err)
		require.True(t, result.IsError)
		assert.Equal(t, "invalid_arguments", operationOutputError(t, result))
	}
}

func TestMCPRelationshipInUseTypeCannotBeDeleted(t *testing.T) {
	st := testutil.NewTestStore(t)
	first, err := st.EnsureParticipant("type-a@example.com", "Example A", "example.com")
	require.NoError(t, err)
	second, err := st.EnsureParticipant("type-b@example.com", "Example B", "example.com")
	require.NoError(t, err)
	a, _, err := st.CreatePersonFromParticipant(first)
	require.NoError(t, err)
	b, _, err := st.CreatePersonFromParticipant(second)
	require.NoError(t, err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	typeResult, err := backend.ExecuteOperation(t.Context(), "create_relationship_type", map[string]any{"slug": "advisor", "forward_label": "advisor", "reverse_label": "advisee"})
	require.NoError(t, err)
	typ := operationOutput[mcpserver.RelationshipTypeRecord](t, typeResult)
	edge, err := backend.ExecuteOperation(t.Context(), "create_person_relationship", map[string]any{"source_person_id": a.ID, "target_person_id": b.ID, "relationship_type_slug": "advisor"})
	require.NoError(t, err)
	require.False(t, edge.IsError)
	rejected, err := backend.ExecuteOperation(t.Context(), "delete_relationship_type", map[string]any{"id": typ.RelationshipType.ID, "etag": typ.ETag})
	require.NoError(t, err)
	require.True(t, rejected.IsError)
	assert.Equal(t, "relationship_type_in_use", operationOutputError(t, rejected))
}

func TestMCPRelationshipCapabilityRequiresEveryPatchProperty(t *testing.T) {
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	caps, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	for i := range caps.Routes {
		if caps.Routes[i].OperationID == "patchPersonRelationship" {
			caps.Routes[i].RequestProperties = []string{"end_date"}
		}
	}
	limited := newDaemonMCPOperations(backend.client, caps)
	assert.NotContains(t, limited.capabilities(), "update_person_relationship")
	assert.Contains(t, limited.capabilities(), "get_person_relationship_record")
	assert.Contains(t, limited.capabilities(), "delete_person_relationship")
}

func TestMCPRelationshipReadRefusesOversizedAndRawErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/relationship-types" {
			_, _ = io.WriteString(w, strings.Repeat("x", 1<<20+1))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"private-secret"}`)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	backend := newDaemonMCPOperations(client, nil)
	result, handled, err := backend.executeRelationshipRead(t.Context(), "list_relationship_types", nil)
	require.NoError(t, err)
	require.True(t, handled)
	require.True(t, result.IsError)
	assert.Equal(t, "operation_response_limit_exceeded", operationOutputError(t, result))
	result, handled, err = backend.executeRelationshipRead(t.Context(), "get_relationship_type", map[string]any{"id": 1})
	require.NoError(t, err)
	require.True(t, handled)
	require.True(t, result.IsError)
	assert.Equal(t, "operation_refused", operationOutputError(t, result))
}
