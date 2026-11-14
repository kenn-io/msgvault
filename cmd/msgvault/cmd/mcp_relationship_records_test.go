package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPRelationshipRecords(t *testing.T) {
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
	created := operationOutput[generated.PersonRelationship](t, result)
	args := map[string]any{"id": created.ID, "revision": created.Revision, "end_date": "2020", "notes": ""}
	_, err = backend.OperationDisclosure(t.Context(), "update_person_relationship", args)
	require.NoError(t, err)
	result, err = backend.ExecuteOperation(t.Context(), "update_person_relationship", args)
	require.NoError(t, err)
	updated := operationOutput[generated.PersonRelationship](t, result)
	assert.Nil(t, updated.Notes)
	require.NotNil(t, updated.EndDate)
	_, err = backend.OperationDisclosure(t.Context(), "delete_person_relationship", map[string]any{"id": created.ID, "revision": created.Revision})
	var refusal *mcpserver.OperationRefusalError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "relationship_revision_conflict", refusal.Code)
	deleteArgs := map[string]any{"id": updated.ID, "revision": updated.Revision}
	_, err = backend.OperationDisclosure(t.Context(), "delete_person_relationship", deleteArgs)
	require.NoError(t, err)
	result, err = backend.ExecuteOperation(t.Context(), "update_person_relationship", map[string]any{"id": updated.ID, "revision": updated.Revision, "notes": "changed"})
	require.NoError(t, err)
	current := operationOutput[generated.PersonRelationship](t, result)
	stale, err := backend.ExecuteOperation(t.Context(), "delete_person_relationship", deleteArgs)
	require.NoError(t, err)
	assertLoopRefusal(t, stale, "relationship_revision_conflict", false)
	disclosure, err := backend.OperationDisclosure(t.Context(), "delete_person_relationship", map[string]any{"id": current.ID, "revision": current.Revision})
	require.NoError(t, err)
	assert.Contains(t, disclosure, "changed")
	result, err = backend.ExecuteOperation(t.Context(), "delete_person_relationship", map[string]any{"id": current.ID, "revision": current.Revision})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	person, err := st.GetPerson(target.ID)
	require.NoError(t, err)
	ref, uid := "example-ref", "example-uid"
	imported, err := st.ResolveRelatedValueContext(t.Context(), store.RelatedImport{PersonID: source.ID, RawValue: person.VCardUID, RawType: "agent", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, SourceRef: &ref, SourceResourceUID: &uid, Actor: "system"})
	require.NoError(t, err)
	require.NotNil(t, imported.Relationship)
	session := operationMCPSession(t, backend, nil, nil)
	call := func(args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_relationships", Arguments: args})
		require.NoError(t, err)
		return result
	}
	inline := loopToolOutput[mcpserver.PersonRelationshipsPage](t, call(map[string]any{"person_id": source.ID}))
	require.Len(t, inline.Relationships, 1)
	assert.Equal(t, &ref, inline.Relationships[0].Relationship.SourceRef)
	assert.Equal(t, &uid, inline.Relationships[0].Relationship.SourceResourceUID)
	var assembled bytes.Buffer
	chunkArgs := map[string]any{"person_id": source.ID, "length": 40}
	for {
		chunk := loopToolOutput[mcpserver.PersonRelationshipsPage](t, call(chunkArgs))
		require.NotNil(t, chunk.DataBase64)
		data, err := base64.StdEncoding.DecodeString(*chunk.DataBase64)
		require.NoError(t, err)
		_, err = assembled.Write(data)
		require.NoError(t, err)
		if *chunk.Complete {
			break
		}
		chunkArgs = map[string]any{"person_id": source.ID, "length": 40, "offset": *chunk.Offset + *chunk.Length, "sha256": *chunk.SHA256}
	}
	var page generated.PersonRelationshipsResponse
	require.NoError(t, json.Unmarshal(assembled.Bytes(), &page))
	assert.Equal(t, inline.Relationships, page.Relationships)
	network, err := backend.ExecuteOperation(t.Context(), "get_person_network", map[string]any{"person_id": int64(999999)})
	require.NoError(t, err)
	assertLoopRefusal(t, network, "person_profile_not_found", false)
	_, err = backend.OperationDisclosure(t.Context(), "patch_person_record", map[string]any{"person_id": int64(999999), "etag": "tag", "patch": map[string]any{"names": map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": "Example", "envelope": map[string]any{"source": "user"}}}}}})
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "person_profile_not_found", refusal.Code)
}

func TestMCPRelationshipCapabilities(t *testing.T) {
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	assert.Contains(t, backend.capabilities(), "create_person_relationship")
	caps, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	for i := range caps.Routes {
		if caps.Routes[i].OperationID == "patchPersonRelationship" {
			caps.Routes[i].RequestProperties = []string{"end_date"}
		}
	}
	limited := newDaemonMCPOperations(backend.client, caps)
	assert.NotContains(t, limited.capabilities(), "update_person_relationship")
	assert.Contains(t, limited.capabilities(), "delete_person_relationship")
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
