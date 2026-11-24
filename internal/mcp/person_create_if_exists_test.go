package mcp

import (
	"encoding/json/v2"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCreatePersonIfExistsSchemaAndGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &creatingPeopleBackend{store: testutil.NewTestStore(t)}
	opts := personCreateToolOptions(backend)
	old := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(old, "create_person")
	oldSchema, ok := old["create_person"]["inputSchema"].(map[string]any)
	require.True(ok)
	oldInput, ok := oldSchema["properties"].(map[string]any)
	require.True(ok)
	assert.NotContains(oldInput, "if_exists")
	opts.PersonCreateIfExists = true
	tools := toolsByName(t, rawListTools(t, opts, true))
	tool := tools["create_person"]
	schema, ok := tool["inputSchema"].(map[string]any)
	require.True(ok)
	input, ok := schema["properties"].(map[string]any)
	require.True(ok)
	require.Contains(input, "if_exists")
	require.Contains(input["if_exists"], "enum")
	ifExists, ok := input["if_exists"].(map[string]any)
	require.True(ok)
	assert.ElementsMatch([]any{"error", "return"}, ifExists["enum"])
	output, ok := tool["outputSchema"].(map[string]any)
	require.True(ok)
	branches, ok := output["anyOf"].([]any)
	require.True(ok)
	require.Len(branches, 2)
	output, ok = branches[0].(map[string]any)
	require.True(ok)
	assert.Contains(output["properties"], "ensure")
	assert.NotContains(output["required"], "ensure")
}

func TestCreatePersonIfExistsReturnAndConflicts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &creatingPeopleBackend{store: testutil.NewTestStore(t)}
	opts := personCreateToolOptions(backend)
	opts.PersonCreateIfExists = true
	args := map[string]any{"name": "Alex Example", "emails": []any{map[string]any{"value": "alex@example.test"}}, "if_exists": "return"}
	first := rawCallTool(t, opts, "create_person", args)
	require.NotEqual(true, first["isError"])
	firstData := toolStructuredContent(t, first)
	firstEnsure, ok := firstData["ensure"].(map[string]any)
	require.True(ok)
	assert.Equal("created", firstEnsure["status"])
	second := rawCallTool(t, opts, "create_person", args)
	require.NotEqual(true, second["isError"])
	secondEnsure, ok := toolStructuredContent(t, second)["ensure"].(map[string]any)
	require.True(ok)
	assert.Equal("matched_existing", secondEnsure["status"])
	rawID, ok := firstData["id"].(float64)
	require.True(ok)
	id := int64(rawID)
	points, err := backend.store.ListPersonContactPointsContext(t.Context(), id, true)
	require.NoError(err)
	require.Len(points, 1)
	assert.Equal(store.ProvenanceEnrichment, points[0].Envelope.Source)
	participant, err := backend.store.EnsureParticipant("observed@example.test", "Observed Example", "example.test")
	require.NoError(err)
	args["emails"] = []any{map[string]any{"value": "observed@example.test"}}
	conflict := rawCallTool(t, opts, "create_person", args)
	require.Equal(true, conflict["isError"])
	data := toolStructuredContent(t, conflict)
	assert.Equal("person_matches_participant", data["error"])
	details, ok := data["details"].(map[string]any)
	require.True(ok)
	assert.InDelta(float64(participant), details["participant_id"], 0)
	next, ok := details["next"].(map[string]any)
	require.True(ok)
	assert.Equal([]any{"msgvault", "person", "promote", strconv.FormatInt(participant, 10), "--json"}, next["cli"])
	args["emails"] = []any{map[string]any{"value": "alex@example.test"}, map[string]any{"value": "observed@example.test"}}
	ambiguous := rawCallTool(t, opts, "create_person", args)
	require.Equal(true, ambiguous["isError"])
	data = toolStructuredContent(t, ambiguous)
	assert.Equal("person_ambiguous", data["error"])
	details, ok = data["details"].(map[string]any)
	require.True(ok)
	assert.Len(details["candidates"], 2)
}

func TestCreatePersonIfExistsDaemonConflictDetails(t *testing.T) {
	for _, test := range []struct{ code, details string }{
		{"person_matches_participant", `{"participant_id":7,"match_basis":[{"kind":"email","value":"alex@example.test","matched":"participant","person_id":null,"participant_id":7}],"next":{"cli":["msgvault","person","promote","7","--json"],"mcp":{"tool":"promote_person","arguments":{"participant_id":7}}}}`},
		{"person_ambiguous", `{"candidates":[{"person_id":1,"participant_id":null,"name":"Alex Example"},{"person_id":null,"participant_id":7,"name":"Blair Example"}],"match_basis":[{"kind":"email","value":"alex@example.test","matched":"contact_point","person_id":1,"participant_id":null},{"kind":"phone","value":"+12025550123","matched":"participant","person_id":null,"participant_id":7}]}`},
	} {
		t.Run(test.code, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backend := &creatingPeopleBackend{err: &daemonclient.APIError{Status: 409, Code: test.code, Message: "Resolve these matches", Details: []byte(test.details)}}
			opts := personCreateToolOptions(backend)
			opts.PersonCreateIfExists = true
			result := rawCallTool(t, opts, "create_person", map[string]any{"name": "Alex Example", "if_exists": "return", "emails": []any{map[string]any{"value": "alex@example.test"}}, "phones": []any{map[string]any{"value": "+12025550123"}}})
			require.Equal(true, result["isError"])
			data := toolStructuredContent(t, result)
			assert.Equal(test.code, data["error"])
			details, err := json.Marshal(data["details"])
			require.NoError(err)
			assert.JSONEq(test.details, string(details))
		})
	}
}
