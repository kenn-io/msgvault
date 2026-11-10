package mcp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCreatePersonIfExistsSchemaAndGate(t *testing.T) {
	backend := &creatingPeopleBackend{store: testutil.NewTestStore(t)}
	opts := personCreateToolOptions(backend)
	old := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(t, old, "create_person")
	oldInput := old["create_person"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	assert.NotContains(t, oldInput, "if_exists")
	opts.PersonCreateIfExists = true
	tools := toolsByName(t, rawListTools(t, opts, true))
	assert.NotContains(t, tools, "ensure_person")
	tool := tools["create_person"]
	input := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	assert.NotContains(t, input, "source")
	require.Contains(t, input, "if_exists")
	require.Contains(t, input["if_exists"], "enum")
	assert.ElementsMatch(t, []any{"error", "return"}, input["if_exists"].(map[string]any)["enum"])
	output := tool["outputSchema"].(map[string]any)
	branches := output["anyOf"].([]any)
	require.Len(t, branches, 2)
	output = branches[0].(map[string]any)
	assert.Contains(t, output["properties"], "ensure")
	assert.NotContains(t, output["required"], "ensure")
}

func TestCreatePersonIfExistsReturnAndConflicts(t *testing.T) {
	backend := &creatingPeopleBackend{store: testutil.NewTestStore(t)}
	opts := personCreateToolOptions(backend)
	opts.PersonCreateIfExists = true
	args := map[string]any{"name": "Alex Example", "emails": []any{map[string]any{"value": "alex@example.test"}}, "if_exists": "return"}
	first := rawCallTool(t, opts, "create_person", args)
	require.NotEqual(t, true, first["isError"])
	assert.Equal(t, "created", toolStructuredContent(t, first)["ensure"].(map[string]any)["status"])
	second := rawCallTool(t, opts, "create_person", args)
	require.NotEqual(t, true, second["isError"])
	assert.Equal(t, "matched_existing", toolStructuredContent(t, second)["ensure"].(map[string]any)["status"])
	id := int64(toolStructuredContent(t, first)["id"].(float64))
	points, err := backend.store.ListPersonContactPointsContext(t.Context(), id, true)
	require.NoError(t, err)
	require.Len(t, points, 1)
	assert.Equal(t, store.ProvenanceEnrichment, points[0].Envelope.Source)
	participant, err := backend.store.EnsureParticipant("observed@example.test", "Observed Example", "example.test")
	require.NoError(t, err)
	args["emails"] = []any{map[string]any{"value": "observed@example.test"}}
	conflict := rawCallTool(t, opts, "create_person", args)
	require.Equal(t, true, conflict["isError"])
	data := toolStructuredContent(t, conflict)
	assert.Equal(t, "person_matches_participant", data["error"])
	details := data["details"].(map[string]any)
	assert.Equal(t, float64(participant), details["participant_id"])
	assert.Equal(t, []any{"msgvault", "person", "promote", fmt.Sprint(participant), "--json"}, details["next"].(map[string]any)["cli"])
	args["emails"] = []any{map[string]any{"value": "alex@example.test"}, map[string]any{"value": "observed@example.test"}}
	ambiguous := rawCallTool(t, opts, "create_person", args)
	require.Equal(t, true, ambiguous["isError"])
	data = toolStructuredContent(t, ambiguous)
	assert.Equal(t, "person_ambiguous", data["error"])
	assert.Len(t, data["details"].(map[string]any)["candidates"], 2)
}

func TestCreatePersonIfExistsDaemonConflictDetails(t *testing.T) {
	for _, code := range []string{"person_matches_participant", "person_ambiguous"} {
		t.Run(code, func(t *testing.T) {
			backend := &creatingPeopleBackend{err: &daemonclient.APIError{Status: 409, Code: code, Message: "Resolve these matches", Details: []byte(`{"participant_id":7,"next":{"cli":["msgvault","person","promote","7","--json"]}}`)}}
			opts := personCreateToolOptions(backend)
			opts.PersonCreateIfExists = true
			result := rawCallTool(t, opts, "create_person", map[string]any{"name": "Alex Example", "if_exists": "return"})
			require.Equal(t, true, result["isError"])
			data := toolStructuredContent(t, result)
			assert.Equal(t, code, data["error"])
			assert.Contains(t, data["details"], "next")
		})
	}
}
