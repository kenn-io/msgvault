package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
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

func recordAttributesFixture(t *testing.T) (*daemonMCPOperations, *store.Store, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("attribute@example.test", "Synthetic Person", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	return backend, st, person.ID
}

func attributeOperation(t *testing.T, b *daemonMCPOperations, name string, args map[string]any) *mcpserver.OperationResult {
	t.Helper()
	result, err := b.ExecuteOperation(t.Context(), name, args)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

func attributeError(t *testing.T, result *mcpserver.OperationResult, code string) {
	t.Helper()
	require.True(t, result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(t, err)
	var failure mcpOperationFailure
	require.NoError(t, json.Unmarshal(data, &failure))
	assert.Equal(t, code, failure.Error)
	assert.False(t, failure.OperationMayHaveCompleted)
}

func attributeRefused(t *testing.T, err error, code string) {
	t.Helper()
	var refusal *mcpserver.OperationRefusalError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, code, refusal.Code)
}

func attributeValues(t *testing.T, st *store.Store, person int64, slug string) []store.PersonAttributeValue {
	t.Helper()
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person, store.PersonAttributeQuery{DefinitionSlug: slug})
	require.NoError(t, err)
	return values
}

func attributeAccept() *sdkmcp.ElicitResult {
	return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}
}

// Protocol 2025-06-18 confirms through Elicitation; an empty version uses InputResponses.
func attributeSession(t *testing.T, b *daemonMCPOperations, ops mcpserver.OperationBackend, options *sdkmcp.ClientOptions, protocol string) *sdkmcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	opts := daemonMCPServeOptions(ctx, b.client, nil)
	if ops != nil {
		opts.Operations = ops
	}
	opts.OperationWriteFamilies = []mcpserver.OperationFamily{mcpserver.OperationFamilyAttributes}
	done := make(chan error, 1)
	go func() { done <- mcpserver.ServeTransport(ctx, opts, serverTransport) }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "attribute-test", Version: "1"}, options)
	session, err := client.Connect(ctx, clientTransport, &sdkmcp.ClientSessionOptions{ProtocolVersion: protocol})
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close(); cancel(); <-done })
	return session
}

func TestMCPAttributeInvalidArgumentsDoNotReachNativeRoute(t *testing.T) {
	b, st, person := recordAttributesFixture(t)
	_, err := b.OperationDisclosure(t.Context(), "set_person_attribute", map[string]any{"person_id": person, "slug": "../bad", "value": map[string]any{"type": "text", "text": "x"}})
	attributeRefused(t, err, "invalid_attribute_slug")
	session := attributeSession(t, b, nil, nil, "2025-06-18")
	for _, args := range []map[string]any{
		{"person_id": person, "value": map[string]any{"type": "text", "text": "x"}},
		{"person_id": person, "slug": store.AttributeSlugPrimaryChannel, "value": map[string]any{"type": "text", "text": "x"}, "unexpected": true},
		{"person_id": "wrong", "slug": store.AttributeSlugPrimaryChannel, "value": map[string]any{"type": "text", "text": "x"}},
	} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: args})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, mcpDraftResultText(t, result), "invalid_arguments")
	}
	assert.Empty(t, attributeValues(t, st, person, store.AttributeSlugPrimaryChannel))
}

func TestMCPAttributeNotesFallbackAndPersonDefinitions(t *testing.T) {
	b, st, person := recordAttributesFixture(t)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM attribute_definitions WHERE universal_id = ?`), store.AttributeUniversalIDNotes)
	require.NoError(t, err)
	var legacy store.AttributeDefinitionInput
	for _, d := range store.SeededAttributeDefinitions() {
		if d.UniversalID == store.AttributeUniversalIDNotes {
			legacy = d
		}
	}
	legacy.UniversalID, legacy.Ownership, legacy.IsDeletable = "994e8d78-4711-42ec-9801-e3348e6fd133", store.AttributeOwnershipUser, true
	_, err = st.CreateAttributeDefinitionContext(t.Context(), legacy)
	require.NoError(t, err)
	require.NoError(t, st.InitSchema())
	organization := legacy
	organization.ObjectType, organization.Slug, organization.UniversalID = store.AttributeObjectOrganization, "organization_example", "1583e7c1-5e7c-4115-b636-c8b03dab5010"
	orgDefinition, err := st.CreateAttributeDefinitionContext(t.Context(), organization)
	require.NoError(t, err)
	definitions := operationOutput[generated.AttributeDefinitionsResponse](t, attributeOperation(t, b, "list_attribute_definitions", map[string]any{"include_hidden": true}))
	var notes string
	for _, d := range definitions.Definitions {
		if d.UniversalID == store.AttributeUniversalIDNotes {
			notes = d.Slug
		}
		assert.Equal(t, "person", d.ObjectType)
	}
	require.NotEmpty(t, notes)
	assert.NotEqual(t, store.AttributeSlugNotes, notes)

	var approvals []string
	session := attributeSession(t, b, nil, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals = append(approvals, request.Params.Message)
		return attributeAccept(), nil
	}}, "2025-06-18")
	call := func(name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err)
		return result
	}
	for _, existing := range []bool{false, true} {
		if existing {
			_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person, DefinitionSlug: notes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("original")}, Source: store.ProvenanceUser})
			require.NoError(t, err)
		}
		assert.Contains(t, mcpDraftResultText(t, call("set_person_attribute", map[string]any{"person_id": person, "slug": notes, "value": map[string]any{"type": "text", "text": "x"}})), "attribute_notes_use_update_person_notes")
		assert.Contains(t, mcpDraftResultText(t, call("clear_person_attribute", map[string]any{"person_id": person, "slug": notes})), "attribute_notes_use_update_person_notes")
	}
	assert.Empty(t, approvals)
	require.Len(t, attributeValues(t, st, person, notes), 1)
	assert.Equal(t, "original", *attributeValues(t, st, person, notes)[0].Value.Text)

	// A user definition at the Notes slug is ordinary; MCP fills blank provenance.
	for _, tc := range []struct{ source, actor, wantSource, wantActor string }{
		{"", "", "enrichment", "mcp"},
		{" ", "  \t ", "enrichment", "mcp"},
		{"user", "owner", "user", "owner"},
	} {
		args := map[string]any{"person_id": person, "slug": store.AttributeSlugNotes, "value": map[string]any{"type": "text", "text": "legacy"}}
		if tc.actor != "" {
			args["source"], args["actor"] = tc.source, tc.actor
		}
		require.False(t, call("set_person_attribute", args).IsError)
		assert.Contains(t, approvals[len(approvals)-1], fmt.Sprintf(`"actor":%q`, tc.wantActor))
		assert.Contains(t, approvals[len(approvals)-1], fmt.Sprintf(`"source":%q`, tc.wantSource))
		values := attributeValues(t, st, person, store.AttributeSlugNotes)
		require.Len(t, values, 1)
		assert.Equal(t, store.Provenance(tc.wantSource), values[0].Source)
		assert.Equal(t, tc.wantActor, *values[0].Actor)
	}

	created := operationOutput[mcpserver.AttributeDefinitionReceipt](t, attributeOperation(t, b, "create_attribute_definition", map[string]any{"definition": map[string]any{"object_type": "organization", "slug": "favorite_tea", "label": "Favorite tea", "description": "old", "value_type": "text", "field_type": "text"}}))
	assert.Equal(t, "person", created.Definition.ObjectType)
	patched := operationOutput[mcpserver.AttributeDefinitionReceipt](t, attributeOperation(t, b, "patch_attribute_definition", map[string]any{"definition_id": created.Definition.ID, "etag": created.ETag, "changes": map[string]any{"description": nil}}))
	assert.Nil(t, patched.Definition.Description)
	_, err = b.OperationDisclosure(t.Context(), "delete_attribute_definition", map[string]any{"definition_id": created.Definition.ID, "etag": created.ETag})
	attributeRefused(t, err, "attribute_definition_revision_conflict")
	assert.True(t, operationOutput[mcpserver.AttributeDefinitionDeleted](t, attributeOperation(t, b, "delete_attribute_definition", map[string]any{"definition_id": created.Definition.ID, "etag": patched.ETag})).Deleted)
	attributeError(t, attributeOperation(t, b, "get_attribute_definition", map[string]any{"definition_id": orgDefinition.ID}), "attribute_definition_not_found")
	for _, name := range []string{"patch_attribute_definition", "delete_attribute_definition"} {
		_, err = b.OperationDisclosure(t.Context(), name, map[string]any{"definition_id": orgDefinition.ID, "etag": "example", "changes": map[string]any{"label": "changed"}})
		attributeRefused(t, err, "attribute_definition_not_found")
	}
}

func TestMCPAttributeClearPinsPreviewedValue(t *testing.T) {
	b, st, person := recordAttributesFixture(t)
	initial := map[string]any{"person_id": person, "slug": store.AttributeSlugPrimaryChannel, "value": map[string]any{"type": "text", "text": "email"}}
	attributeOperation(t, b, "set_person_attribute", initial)
	args := map[string]any{"person_id": person, "slug": store.AttributeSlugPrimaryChannel}
	_, err := b.OperationDisclosure(t.Context(), "clear_person_attribute", args)
	require.NoError(t, err)
	initial["value"] = map[string]any{"type": "text", "text": "chat"}
	attributeOperation(t, b, "set_person_attribute", initial)
	attributeError(t, attributeOperation(t, b, "clear_person_attribute", args), "attribute_value_conflict")
	values := attributeValues(t, st, person, store.AttributeSlugPrimaryChannel)
	require.Len(t, values, 1)
	assert.Equal(t, "chat", *values[0].Value.Text)
}

func TestMCPAttributePreviewCallerGuards(t *testing.T) {
	b, st, person := recordAttributesFixture(t)
	disclose := func(name string, args map[string]any) error {
		_, err := b.OperationDisclosure(t.Context(), name, args)
		return err
	}
	args := map[string]any{"person_id": person, "slug": store.AttributeSlugPrimaryChannel, "value": map[string]any{"type": "text", "text": "email"}}
	invalid := maps.Clone(args)
	invalid["expected_empty"], invalid["expected_value_id"] = true, int64(1)
	attributeRefused(t, disclose("set_person_attribute", invalid), "invalid_expected_empty")
	assert.Empty(t, attributeValues(t, st, person, store.AttributeSlugPrimaryChannel))
	pinned := maps.Clone(args)
	require.NoError(t, disclose("set_person_attribute", pinned))
	assert.Equal(t, true, pinned["expected_empty"])
	args["expected_empty"] = true
	require.NoError(t, disclose("set_person_attribute", args))
	assert.Nil(t, args["expected_value_id"])
	first := operationOutput[generated.PersonAttributeWrite](t, attributeOperation(t, b, "set_person_attribute", args))
	attributeRefused(t, disclose("set_person_attribute", args), "attribute_value_conflict")
	delete(args, "expected_empty")
	pinned = maps.Clone(args)
	require.NoError(t, disclose("set_person_attribute", pinned))
	assert.Equal(t, first.Value.ID, pinned["expected_value_id"])
	assert.Nil(t, pinned["expected_empty"])
	args["expected_value_id"] = first.Value.ID
	require.NoError(t, disclose("set_person_attribute", args))
	args["expected_value_id"] = first.Value.ID + 1
	attributeRefused(t, disclose("set_person_attribute", args), "attribute_value_conflict")
	attributeRefused(t, disclose("clear_person_attribute", map[string]any{"person_id": person, "slug": store.AttributeSlugPrimaryChannel, "expected_value_id": first.Value.ID + 1}), "attribute_value_conflict")
	args["expected_empty"] = true
	attributeRefused(t, disclose("set_person_attribute", args), "invalid_expected_empty")

	capabilities, err := b.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	for i, route := range capabilities.Routes {
		if route.OperationID == "setPersonAttribute" {
			capabilities.Routes[i].RequestProperties = slices.DeleteFunc(slices.Clone(route.RequestProperties), func(name string) bool { return name == "expected_empty" })
		}
	}
	assert.NotContains(t, newDaemonMCPOperations(b.client, capabilities).capabilities(), "set_person_attribute")
}

type attributeRaceBackend struct {
	*daemonMCPOperations

	count int
	race  func()
}

// Races after the retry's final dry-run disclosure, before confirmation consumes request state.
func (b *attributeRaceBackend) OperationDisclosure(ctx context.Context, name string, args map[string]any) (string, error) {
	disclosure, err := b.daemonMCPOperations.OperationDisclosure(ctx, name, args)
	if b.count++; b.count == 2 && err == nil {
		b.race()
	}
	return disclosure, err
}

func TestMCPAttributeEditDuringApproval(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, scenario := range []string{"empty", "existing", "multi"} {
			t.Run(fmt.Sprintf("%s/modern=%t", scenario, modern), func(t *testing.T) {
				b, st, person := recordAttributesFixture(t)
				slug := store.AttributeSlugPrimaryChannel
				if scenario == "multi" {
					slug = "favorite_tea"
					attributeOperation(t, b, "create_attribute_definition", map[string]any{"definition": map[string]any{"slug": slug, "label": "Favorite tea", "value_type": "text", "field_type": "text", "cardinality": "multi"}})
				}
				competing := map[string]any{"person_id": person, "slug": slug, "value": map[string]any{"type": "text", "text": "phone"}}
				if scenario == "existing" {
					require.False(t, attributeOperation(t, b, "set_person_attribute", map[string]any{"person_id": person, "slug": slug, "value": map[string]any{"type": "text", "text": "email"}}).IsError)
				}
				compete := func() { assert.False(t, attributeOperation(t, b, "set_person_attribute", competing).IsError) }
				params := &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: map[string]any{"person_id": person, "slug": slug, "value": map[string]any{"type": "text", "text": "chat"}}}
				var result *sdkmcp.CallToolResult
				var err error
				if modern {
					session := attributeSession(t, b, &attributeRaceBackend{daemonMCPOperations: b, race: compete}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}}, "")
					result, err = session.CallTool(t.Context(), params)
					require.NoError(t, err)
					require.NotEmpty(t, result.RequestState)
					params.RequestState, params.InputResponses = result.RequestState, sdkmcp.InputResponseMap{"confirm": attributeAccept()}
					result, err = session.CallTool(t.Context(), params)
				} else {
					session := attributeSession(t, b, nil, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
						if scenario != "existing" {
							assert.Contains(t, request.Params.Message, `"expected_empty":true`)
						}
						compete()
						return attributeAccept(), nil
					}}, "2025-06-18")
					result, err = session.CallTool(t.Context(), params)
				}
				require.NoError(t, err)
				require.True(t, result.IsError)
				assert.Contains(t, mcpDraftResultText(t, result), "attribute_value_conflict")
				values := attributeValues(t, st, person, slug)
				require.Len(t, values, 1)
				assert.Equal(t, "phone", *values[0].Value.Text)
			})
		}
	}
}

func TestMCPAttributeExactTokens(t *testing.T) {
	b, st, person := recordAttributesFixture(t)
	attributeOperation(t, b, "create_attribute_definition", map[string]any{"definition": map[string]any{"slug": "metadata", "label": "Metadata", "value_type": "json", "field_type": "json"}})
	session := operationMCPSession(t, b, []mcpserver.OperationFamily{mcpserver.OperationFamilyAttributes}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	confirm := func(approved, retried string) (string, *sdkmcp.CallToolResult) {
		value := map[string]any{"type": "json", "json": jsontext.Value(approved)}
		params := &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: map[string]any{"person_id": person, "slug": "metadata", "value": value}}
		pending, err := session.CallTool(t.Context(), params)
		require.NoError(t, err)
		require.NotEmpty(t, pending.RequestState)
		prompt, ok := pending.InputRequests["confirm"].(*sdkmcp.ElicitParams)
		require.True(t, ok)
		value["json"] = jsontext.Value(retried)
		params.RequestState, params.InputResponses = pending.RequestState, sdkmcp.InputResponseMap{"confirm": attributeAccept()}
		result, err := session.CallTool(t.Context(), params)
		require.NoError(t, err)
		return prompt.Message, result
	}
	list := &sdkmcp.CallToolParams{Name: "list_person_attributes", Arguments: map[string]any{"person_id": person, "slug": "metadata"}}
	for _, token := range []string{"1e2", "1E+2", "0.1234567890123456789", "9007199254740993", `{"n":1e2}`} {
		approval, result := confirm(token, token)
		require.False(t, result.IsError, "%v", result.Content)
		assert.Contains(t, approval, token)
		values := attributeValues(t, st, person, "metadata")
		require.Len(t, values, 1)
		assert.Equal(t, token, string(values[0].Value.JSON))
		listed, err := session.CallTool(t.Context(), list)
		require.NoError(t, err)
		assert.Contains(t, mcpDraftResultText(t, listed), token)
	}
	_, changed := confirm("0.1234567890123456789", "0.1234567890123456788")
	assert.True(t, changed.IsError)
	unchanged := attributeValues(t, st, person, "metadata")
	require.Len(t, unchanged, 1)
	assert.Equal(t, `{"n":1e2}`, string(unchanged[0].Value.JSON))

	// The SDK client decodes structured results through float64, so read the wire.
	_, err := st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person, DefinitionSlug: "metadata", Value: store.AttributeValue{Type: store.AttributeValueJSON, JSON: jsontext.Value("1e400")}, Source: store.ProvenanceUser})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- mcpserver.ServeTransport(ctx, daemonMCPServeOptions(ctx, b.client, nil), serverTransport)
	}()
	conn, err := clientTransport.Connect(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close(); cancel(); <-done }()
	var response string
	for _, raw := range []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw-test","version":"1"}}}`, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_person_attributes","arguments":{"person_id":%d,"slug":"metadata"}}}`, person)} {
		msg, err := jsonrpc.DecodeMessage([]byte(raw))
		require.NoError(t, err)
		require.NoError(t, conn.Write(ctx, msg))
		if strings.Contains(raw, `"id":`) {
			reply, err := conn.Read(ctx)
			require.NoError(t, err)
			encoded, err := jsonrpc.EncodeMessage(reply)
			require.NoError(t, err)
			response = string(encoded)
		}
	}
	assert.Contains(t, response, `"json":1e400`)
}
