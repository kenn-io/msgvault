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
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.EnsureParticipant("first@example.com", "First Example", "example.com")
	requirements.NoError(err)
	second, err := st.EnsureParticipant("second@example.com", "Second Example", "example.com")
	requirements.NoError(err)
	source, _, err := st.CreatePersonFromParticipant(first)
	requirements.NoError(err)
	target, _, err := st.CreatePersonFromParticipant(second)
	requirements.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	return st, backend, source.ID, target.ID
}

func assertRelationshipDisclosure(t *testing.T, disclosure string) {
	t.Helper()
	assertions := assert.New(t)
	requirements := require.New(t)
	var value struct {
		Source       string `json:"source"`
		Target       string `json:"target"`
		Relationship string `json:"relationship"`
	}
	requirements.NoError(json.Unmarshal([]byte(disclosure), &value))
	assertions.Equal("First Example", value.Source)
	assertions.Equal("Second Example", value.Target)
	assertions.Equal("relative", value.Relationship)
}

func TestMCPRelationshipRecords(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, backend, source, target := relationshipFixture(t)
	createArgs := map[string]any{"source_person_id": source, "target_person_id": target, "relationship_type_slug": " kin ", "notes": "synthetic"}
	disclosure, err := backend.OperationDisclosure(t.Context(), "create_person_relationship", createArgs)
	requirements.NoError(err)
	assertRelationshipDisclosure(t, disclosure)
	assertions.Contains(disclosure, "synthetic")
	result, err := backend.ExecuteOperation(t.Context(), "create_person_relationship", createArgs)
	requirements.NoError(err)
	created := operationOutput[generated.PersonRelationship](t, result)
	args := map[string]any{"id": created.ID, "revision": created.Revision, "end_date": "2020", "notes": ""}
	disclosure, err = backend.OperationDisclosure(t.Context(), "update_person_relationship", args)
	requirements.NoError(err)
	assertRelationshipDisclosure(t, disclosure)
	assertions.Contains(disclosure, "2020")
	result, err = backend.ExecuteOperation(t.Context(), "update_person_relationship", args)
	requirements.NoError(err)
	updated := operationOutput[generated.PersonRelationship](t, result)
	assertions.Nil(updated.Notes)
	requirements.NotNil(updated.EndDate)
	requirements.NotNil(updated.EndDate.Year)
	assertions.Equal(int64(2020), *updated.EndDate.Year)
	deleteArgs := map[string]any{"id": updated.ID, "revision": updated.Revision}
	_, err = backend.OperationDisclosure(t.Context(), "delete_person_relationship", deleteArgs)
	requirements.NoError(err)
	result, err = backend.ExecuteOperation(t.Context(), "update_person_relationship", map[string]any{"id": updated.ID, "revision": updated.Revision, "notes": "changed"})
	requirements.NoError(err)
	current := operationOutput[generated.PersonRelationship](t, result)
	_, err = backend.OperationDisclosure(t.Context(), "delete_person_relationship", deleteArgs)
	var refusal *mcpserver.OperationRefusalError
	requirements.ErrorAs(err, &refusal)
	assertions.Equal("relationship_revision_conflict", refusal.Code)
	stale, err := backend.ExecuteOperation(t.Context(), "delete_person_relationship", deleteArgs)
	requirements.NoError(err)
	assertLoopRefusal(t, stale, "relationship_revision_conflict", false)
	deleteArgs["revision"] = current.Revision
	disclosure, err = backend.OperationDisclosure(t.Context(), "delete_person_relationship", deleteArgs)
	requirements.NoError(err)
	assertRelationshipDisclosure(t, disclosure)
	assertions.Contains(disclosure, "changed")
	result, err = backend.ExecuteOperation(t.Context(), "delete_person_relationship", deleteArgs)
	requirements.NoError(err)
	assertions.True(operationOutput[mcpserver.RelationshipDeletion](t, result).Deleted)
	_, err = st.GetPersonRelationshipContext(t.Context(), created.ID)
	requirements.ErrorIs(err, store.ErrPersonRelationshipNotFound)
}

func TestMCPRelationshipListAndNetworkReads(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, backend, source, target := relationshipFixture(t)
	session := operationMCPSession(t, backend, nil, nil)
	emptyResult, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_relationships", Arguments: map[string]any{"person_id": source}})
	requirements.NoError(err)
	empty := loopToolOutput[mcpserver.PersonRelationshipsPage](t, emptyResult)
	requirements.NotNil(empty.Relationships)
	assertions.Empty(*empty.Relationships)
	person, err := st.GetPerson(target)
	requirements.NoError(err)
	ref, uid := "example-ref", "example-uid"
	imported, err := st.ResolveRelatedValueContext(t.Context(), store.RelatedImport{PersonID: source, RawValue: person.VCardUID, RawType: "agent", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, SourceRef: &ref, SourceResourceUID: &uid, Actor: "system"})
	requirements.NoError(err)
	requirements.NotNil(imported.Relationship)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_relationships", Arguments: map[string]any{"person_id": source}})
	requirements.NoError(err)
	inline := loopToolOutput[mcpserver.PersonRelationshipsPage](t, result)
	requirements.NotNil(inline.Relationships)
	requirements.Len(*inline.Relationships, 1)
	assertions.Equal(&ref, (*inline.Relationships)[0].Relationship.SourceRef)
	assertions.Equal(&uid, (*inline.Relationships)[0].Relationship.SourceResourceUID)
	networkResult, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_network", Arguments: map[string]any{"person_id": source}})
	requirements.NoError(err)
	network := loopToolOutput[generated.PersonNetwork](t, networkResult)
	assertions.Equal(source, network.RootPersonID)
	requirements.Len(network.Edges, 1)
	missing, err := backend.ExecuteOperation(t.Context(), "get_person_network", map[string]any{"person_id": int64(999999)})
	requirements.NoError(err)
	assertLoopRefusal(t, missing, "person_profile_not_found", false)
}

func TestMCPRelationshipCapabilities(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)}))
	assertions.Contains(backend.capabilities(), "create_person_relationship")
	assertions.Contains(backend.capabilities(), "update_person_relationship")
	caps, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	patchCaps := *caps
	patchCaps.Routes = slices.Clone(caps.Routes)
	for i := range patchCaps.Routes {
		if patchCaps.Routes[i].OperationID == "patchPersonRelationship" {
			patchCaps.Routes[i].RequestProperties = []string{"end_date"}
		}
	}
	limited := newDaemonMCPOperations(backend.client, &patchCaps)
	assertions.NotContains(limited.capabilities(), "update_person_relationship")
	assertions.Contains(limited.capabilities(), "delete_person_relationship")
	caps.Routes = slices.DeleteFunc(caps.Routes, func(route apiprotocol.MCPRouteDescriptor) bool { return route.OperationID == "getPersonProfile" })
	limited = newDaemonMCPOperations(backend.client, caps)
	assertions.NotContains(limited.capabilities(), "create_person_relationship")
	assertions.NotContains(limited.capabilities(), "update_person_relationship")
	assertions.NotContains(limited.capabilities(), "delete_person_relationship")
}

func TestMCPRelationshipLargeList(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, backend, root, _ := relationshipFixture(t)
	notes := strings.Repeat("x", 4096)
	for i := range 260 {
		participant, err := st.EnsureParticipant(fmt.Sprintf("user-%d@example.com", i), "Example", "example.com")
		requirements.NoError(err)
		person, _, err := st.CreatePersonFromParticipant(participant)
		requirements.NoError(err)
		_, err = st.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{SourcePersonID: root, TargetPersonID: person.ID, TypeSlug: "parent", Notes: &notes, Source: store.ProvenanceUser, Actor: "user"})
		requirements.NoError(err)
	}
	raw, err := backend.ExecuteOperation(t.Context(), "list_person_relationships", map[string]any{"person_id": root})
	requirements.NoError(err)
	requirements.False(raw.IsError)
	requirements.Greater(len(raw.Data), 1<<20)
	session := operationMCPSession(t, backend, nil, nil)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_relationships", Arguments: map[string]any{"person_id": root}})
	requirements.NoError(err)
	chunk := loopToolOutput[mcpserver.PersonRelationshipsPage](t, result)
	requirements.NotNil(chunk.DataBase64)
	requirements.NotNil(chunk.Complete)
	assertions.False(*chunk.Complete)
	requirements.NotNil(chunk.Offset)
	requirements.NotNil(chunk.Length)
	requirements.NotNil(chunk.SHA256)
	first, err := base64.StdEncoding.DecodeString(*chunk.DataBase64)
	requirements.NoError(err)
	requirements.NotEmpty(first)
	assertions.True(bytes.HasPrefix(raw.Data, first))
}
