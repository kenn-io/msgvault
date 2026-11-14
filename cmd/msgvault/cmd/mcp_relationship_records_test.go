package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func relationshipFixture(t *testing.T) (*store.Store, *daemonMCPOperations, int64, int64) {
	t.Helper()
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
	return st, backend, source.ID, target.ID
}

func assertRelationshipDisclosure(t *testing.T, disclosure string) {
	t.Helper()
	var value struct {
		Source struct {
			DisplayName string `json:"display_name"`
		} `json:"source"`
		Target struct {
			DisplayName string `json:"display_name"`
		} `json:"target"`
		Relationship string `json:"relationship"`
	}
	require.NoError(t, json.Unmarshal([]byte(disclosure), &value))
	assert.Equal(t, "First Example", value.Source.DisplayName)
	assert.Equal(t, "Second Example", value.Target.DisplayName)
	assert.Equal(t, "parent", value.Relationship)
}

func TestMCPRelationshipRecords(t *testing.T) {
	st, backend, source, target := relationshipFixture(t)
	_, err := backend.OperationDisclosure(t.Context(), "create_person_relationship", map[string]any{"source_person_id": source, "target_person_id": int64(999999), "relationship_type_slug": "parent"})
	var refusal *mcpserver.OperationRefusalError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "person_profile_not_found", refusal.Code)
	createArgs := map[string]any{"source_person_id": source, "target_person_id": target, "relationship_type_slug": "parent", "notes": "synthetic"}
	disclosure, err := backend.OperationDisclosure(t.Context(), "create_person_relationship", createArgs)
	require.NoError(t, err)
	assertRelationshipDisclosure(t, disclosure)
	assert.Contains(t, disclosure, "synthetic")
	result, err := backend.ExecuteOperation(t.Context(), "create_person_relationship", createArgs)
	require.NoError(t, err)
	created := operationOutput[generated.PersonRelationship](t, result)
	args := map[string]any{"id": created.ID, "revision": created.Revision, "end_date": "2020", "notes": ""}
	disclosure, err = backend.OperationDisclosure(t.Context(), "update_person_relationship", args)
	require.NoError(t, err)
	assertRelationshipDisclosure(t, disclosure)
	assert.Contains(t, disclosure, "2020")
	result, err = backend.ExecuteOperation(t.Context(), "update_person_relationship", args)
	require.NoError(t, err)
	updated := operationOutput[generated.PersonRelationship](t, result)
	assert.Nil(t, updated.Notes)
	require.NotNil(t, updated.EndDate)
	deleteArgs := map[string]any{"id": updated.ID, "revision": updated.Revision}
	disclosure, err = backend.OperationDisclosure(t.Context(), "delete_person_relationship", deleteArgs)
	require.NoError(t, err)
	assertRelationshipDisclosure(t, disclosure)
	result, err = backend.ExecuteOperation(t.Context(), "delete_person_relationship", deleteArgs)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	session := operationMCPSession(t, backend, nil, nil)
	emptyResult, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_relationships", Arguments: map[string]any{"person_id": source}})
	require.NoError(t, err)
	empty := loopToolOutput[mcpserver.PersonRelationshipsPage](t, emptyResult)
	require.NotNil(t, empty.Relationships)
	assert.Empty(t, *empty.Relationships)
	person, err := st.GetPerson(target)
	require.NoError(t, err)
	ref, uid := "example-ref", "example-uid"
	imported, err := st.ResolveRelatedValueContext(t.Context(), store.RelatedImport{PersonID: source, RawValue: person.VCardUID, RawType: "agent", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, SourceRef: &ref, SourceResourceUID: &uid, Actor: "system"})
	require.NoError(t, err)
	require.NotNil(t, imported.Relationship)
	call := func(args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_relationships", Arguments: args})
		require.NoError(t, err)
		return result
	}
	inline := loopToolOutput[mcpserver.PersonRelationshipsPage](t, call(map[string]any{"person_id": source}))
	require.NotNil(t, inline.Relationships)
	require.Len(t, *inline.Relationships, 1)
	assert.Equal(t, &ref, (*inline.Relationships)[0].Relationship.SourceRef)
	assert.Equal(t, &uid, (*inline.Relationships)[0].Relationship.SourceResourceUID)
}

func TestMCPRelationshipStaleDisclosure(t *testing.T) {
	_, backend, source, target := relationshipFixture(t)
	result, err := backend.ExecuteOperation(t.Context(), "create_person_relationship", map[string]any{"source_person_id": source, "target_person_id": target, "relationship_type_slug": "parent"})
	require.NoError(t, err)
	created := operationOutput[generated.PersonRelationship](t, result)
	result, err = backend.ExecuteOperation(t.Context(), "update_person_relationship", map[string]any{"id": created.ID, "revision": created.Revision, "notes": "changed"})
	require.NoError(t, err)
	require.False(t, result.IsError)
	_, err = backend.OperationDisclosure(t.Context(), "delete_person_relationship", map[string]any{"id": created.ID, "revision": created.Revision})
	var refusal *mcpserver.OperationRefusalError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "relationship_revision_conflict", refusal.Code)
}

func TestMCPRelationshipStaleWrite(t *testing.T) {
	_, backend, source, target := relationshipFixture(t)
	result, err := backend.ExecuteOperation(t.Context(), "create_person_relationship", map[string]any{"source_person_id": source, "target_person_id": target, "relationship_type_slug": "parent"})
	require.NoError(t, err)
	created := operationOutput[generated.PersonRelationship](t, result)
	args := map[string]any{"id": created.ID, "revision": created.Revision}
	_, err = backend.OperationDisclosure(t.Context(), "delete_person_relationship", args)
	require.NoError(t, err)
	result, err = backend.ExecuteOperation(t.Context(), "update_person_relationship", map[string]any{"id": created.ID, "revision": created.Revision, "notes": "changed"})
	require.NoError(t, err)
	updated := operationOutput[generated.PersonRelationship](t, result)
	stale, err := backend.ExecuteOperation(t.Context(), "delete_person_relationship", args)
	require.NoError(t, err)
	assertLoopRefusal(t, stale, "relationship_revision_conflict", false)
	disclosure, err := backend.OperationDisclosure(t.Context(), "delete_person_relationship", map[string]any{"id": updated.ID, "revision": updated.Revision})
	require.NoError(t, err)
	assert.Contains(t, disclosure, "changed")
}

func TestMCPRelationshipCapabilities(t *testing.T) {
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	assert.Contains(t, backend.capabilities(), "create_person_relationship")
	assert.Contains(t, backend.capabilities(), "update_person_relationship")
	caps, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	patchCaps := *caps
	patchCaps.Routes = slices.Clone(caps.Routes)
	for i := range patchCaps.Routes {
		if patchCaps.Routes[i].OperationID == "patchPersonRelationship" {
			patchCaps.Routes[i].RequestProperties = []string{"end_date"}
		}
	}
	limited := newDaemonMCPOperations(backend.client, &patchCaps)
	assert.NotContains(t, limited.capabilities(), "update_person_relationship")
	assert.Contains(t, limited.capabilities(), "delete_person_relationship")
	caps.Routes = slices.DeleteFunc(caps.Routes, func(route apiprotocol.MCPRouteDescriptor) bool { return route.OperationID == "getPersonProfile" })
	limited = newDaemonMCPOperations(backend.client, caps)
	assert.NotContains(t, limited.capabilities(), "create_person_relationship")
	assert.NotContains(t, limited.capabilities(), "update_person_relationship")
	assert.NotContains(t, limited.capabilities(), "delete_person_relationship")
}

func TestMCPRelationshipLargeList(t *testing.T) {
	st := testutil.NewTestStore(t)
	first, err := st.EnsureParticipant("root@example.com", "Root", "example.com")
	require.NoError(t, err)
	root, _, err := st.CreatePersonFromParticipant(first)
	require.NoError(t, err)
	notes := strings.Repeat("x", 4096)
	for i := range 260 {
		participant, err := st.EnsureParticipant(fmt.Sprintf("user-%d@example.com", i), "Example", "example.com")
		require.NoError(t, err)
		person, _, err := st.CreatePersonFromParticipant(participant)
		require.NoError(t, err)
		_, err = st.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{SourcePersonID: root.ID, TargetPersonID: person.ID, TypeSlug: "parent", Notes: &notes, Source: store.ProvenanceUser, Actor: "user"})
		require.NoError(t, err)
	}
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	raw, err := backend.ExecuteOperation(t.Context(), "list_person_relationships", map[string]any{"person_id": root.ID})
	require.NoError(t, err)
	require.False(t, raw.IsError)
	require.Greater(t, len(raw.Data), 1<<20)
	session := operationMCPSession(t, backend, nil, nil)
	args := map[string]any{"person_id": root.ID, "length": 32768}
	var assembled bytes.Buffer
	for {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_relationships", Arguments: args})
		require.NoError(t, err)
		chunk := loopToolOutput[mcpserver.PersonRelationshipsPage](t, result)
		require.NotNil(t, chunk.DataBase64)
		data, err := base64.StdEncoding.DecodeString(*chunk.DataBase64)
		require.NoError(t, err)
		_, err = assembled.Write(data)
		require.NoError(t, err)
		if *chunk.Complete {
			break
		}
		args = map[string]any{"person_id": root.ID, "length": 32768, "offset": *chunk.Offset + *chunk.Length, "sha256": *chunk.SHA256}
	}
	assert.JSONEq(t, string(raw.Data), assembled.String())
}
