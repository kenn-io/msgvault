package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
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
	var failure struct {
		Error string `json:"error"`
		May   bool   `json:"operation_may_have_completed"`
	}
	require.NoError(t, json.Unmarshal(data, &failure))
	assert.Equal(t, code, failure.Error)
	assert.False(t, failure.May)
}

func TestMCPAttributeDefinitionCRUDAndNativeGuards(t *testing.T) {
	b, st, _ := recordAttributesFixture(t)
	list := operationOutput[generated.AttributeDefinitionsResponse](t, attributeOperation(t, b, "list_attribute_definitions", map[string]any{"object_type": "person", "include_hidden": true}))
	require.NotEmpty(t, list.Definitions)
	var seeded generated.AttributeDefinition
	for _, d := range list.Definitions {
		if !d.IsDeletable {
			seeded = d
			break
		}
	}
	require.Positive(t, seeded.ID)
	seed := operationOutput[mcpserver.AttributeDefinitionReceipt](t, attributeOperation(t, b, "get_attribute_definition", map[string]any{"definition_id": seeded.ID}))
	attributeError(t, attributeOperation(t, b, "delete_attribute_definition", map[string]any{"definition_id": seeded.ID, "etag": seed.ETag}), "attribute_definition_not_deletable")
	created := operationOutput[mcpserver.AttributeDefinitionReceipt](t, attributeOperation(t, b, "create_attribute_definition", map[string]any{"definition": map[string]any{"object_type": "person", "slug": "favorite_tea", "label": "Favorite tea", "description": "old", "value_type": "text", "field_type": "text", "cardinality": "single"}}))
	assert.NotEmpty(t, created.ETag)
	read := operationOutput[mcpserver.AttributeDefinitionReceipt](t, attributeOperation(t, b, "get_attribute_definition", map[string]any{"definition_id": created.Definition.ID}))
	assert.Equal(t, created.ETag, read.ETag)
	patched := operationOutput[mcpserver.AttributeDefinitionReceipt](t, attributeOperation(t, b, "patch_attribute_definition", map[string]any{"definition_id": created.Definition.ID, "etag": read.ETag, "changes": map[string]any{"label": "Tea", "description": nil, "is_sensitive": false}}))
	assert.Equal(t, "Tea", patched.Definition.Label)
	assert.Nil(t, patched.Definition.Description)
	assert.NotEqual(t, read.ETag, patched.ETag)
	attributeError(t, attributeOperation(t, b, "patch_attribute_definition", map[string]any{"definition_id": created.Definition.ID, "etag": read.ETag, "changes": map[string]any{"label": "Stale"}}), "attribute_definition_revision_conflict")
	deleted := operationOutput[mcpserver.AttributeDefinitionDeleted](t, attributeOperation(t, b, "delete_attribute_definition", map[string]any{"definition_id": created.Definition.ID, "etag": patched.ETag}))
	assert.True(t, deleted.Deleted)
	_, err := st.GetAttributeDefinitionContext(t.Context(), created.Definition.ID)
	assert.ErrorIs(t, err, store.ErrAttributeDefinitionNotFound)
}

func TestMCPPersonAttributeDryRunOrdinalAndCAS(t *testing.T) {
	b, st, person := recordAttributesFixture(t)
	created := operationOutput[mcpserver.AttributeDefinitionReceipt](t, attributeOperation(t, b, "create_attribute_definition", map[string]any{"definition": map[string]any{"object_type": "person", "slug": "favorite_tea", "label": "Favorite tea", "value_type": "text", "field_type": "text", "cardinality": "multi"}}))
	args := map[string]any{"person_id": person, "slug": "favorite_tea", "ordinal": int64(2), "value": map[string]any{"type": "text", "text": "green"}, "dry_run": true}
	preview := operationOutput[generated.PersonAttributeWrite](t, attributeOperation(t, b, "set_person_attribute", args))
	assert.True(t, preview.DryRun)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person, store.PersonAttributeQuery{DefinitionSlug: "favorite_tea"})
	require.NoError(t, err)
	assert.Empty(t, values)
	args["dry_run"] = false
	written := operationOutput[generated.PersonAttributeWrite](t, attributeOperation(t, b, "set_person_attribute", args))
	require.NotNil(t, written.Value)
	assert.Equal(t, int64(2), written.Value.Ordinal)
	args["expected_value_id"] = written.Value.ID + 1
	attributeError(t, attributeOperation(t, b, "set_person_attribute", args), "attribute_value_conflict")
	attributeError(t, attributeOperation(t, b, "delete_attribute_definition", map[string]any{"definition_id": created.Definition.ID, "etag": created.ETag}), "attribute_definition_has_values")
	clear := map[string]any{"person_id": person, "slug": "favorite_tea", "ordinal": int64(2), "expected_value_id": written.Value.ID, "dry_run": true}
	dry := operationOutput[generated.PersonAttributeWrite](t, attributeOperation(t, b, "clear_person_attribute", clear))
	assert.True(t, dry.DryRun)
	listed := operationOutput[generated.PersonAttributesResponse](t, attributeOperation(t, b, "list_person_attributes", map[string]any{"person_id": person, "slug": "favorite_tea", "history": true}))
	require.Len(t, listed.Attributes, 1)
	require.Len(t, listed.Attributes[0].Current, 1)
	clear["dry_run"] = false
	result := operationOutput[generated.PersonAttributeWrite](t, attributeOperation(t, b, "clear_person_attribute", clear))
	require.NotNil(t, result.Superseded)
	listed = operationOutput[generated.PersonAttributesResponse](t, attributeOperation(t, b, "list_person_attributes", map[string]any{"person_id": person, "slug": "favorite_tea", "history": true}))
	require.Len(t, listed.Attributes, 1)
	assert.Empty(t, listed.Attributes[0].Current)
	assert.NotEmpty(t, listed.Attributes[0].History)
	attributeError(t, attributeOperation(t, b, "clear_person_attribute", clear), "attribute_value_not_found")
}

func TestMCPAttributeCapabilitiesRejectMissingNativeContracts(t *testing.T) {
	b, _, _ := recordAttributesFixture(t)
	for _, name := range []string{"list_person_attributes", "set_person_attribute", "clear_person_attribute", "list_attribute_definitions", "get_attribute_definition", "create_attribute_definition", "patch_attribute_definition", "delete_attribute_definition"} {
		assert.Contains(t, b.capabilities(), name)
	}
	caps := &apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{{OperationID: "clearPersonAttribute", Method: http.MethodDelete, Path: "/api/v1/people/{id}/attributes/{slug}", QueryParameters: []string{"ordinal", "dry_run"}}}}
	partial := newDaemonMCPOperations(b.client, caps)
	assert.NotContains(t, partial.capabilities(), "clear_person_attribute")
	attributeError(t, attributeOperation(t, partial, "clear_person_attribute", map[string]any{}), "operation_not_supported")
}

func TestMCPAttributeSDKConfirmationAndNativeEffects(t *testing.T) {
	b, st, person := recordAttributesFixture(t)
	approvals := 0
	session := operationMCPSession(t, b, []mcpserver.OperationFamily{mcpserver.OperationFamilyAttributes}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: map[string]any{"person_id": person, "slug": store.AttributeSlugPrimaryChannel, "value": map[string]any{"type": "text", "text": "email"}}})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError)
	assert.Equal(t, 1, approvals)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel})
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.NotNil(t, values[0].Value.Text)
	assert.Equal(t, "email", *values[0].Value.Text)
	bad, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: map[string]any{"person_id": person, "slug": store.AttributeSlugPrimaryChannel, "value": map[string]any{"type": "text", "text": "chat"}, "unknown": "bad"}})
	assert.True(t, err != nil || bad.IsError)
	assert.Equal(t, 1, approvals)
}

// An HTTP fixture supplies failures the real store cannot reliably cause after dispatch.
func TestMCPAttributeResponseBoundsAndFailureCertainty(t *testing.T) {
	var mu sync.Mutex
	status := http.StatusInternalServerError
	body := `{"error":"attribute_failed","message":"private diagnostic"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	b := &daemonMCPOperations{client: client, supported: []string{"set_person_attribute", "list_attribute_definitions"}}
	args := map[string]any{"person_id": int64(1), "slug": "favorite_tea", "value": map[string]any{"type": "text", "text": "green"}}
	result := attributeOperation(t, b, "set_person_attribute", args)
	data, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.JSONEq(t, `{"error":"operation_refused","operation_may_have_completed":true}`, string(data))
	assert.NotContains(t, string(data), "private diagnostic")
	mu.Lock()
	status = http.StatusConflict
	body = `{"error":"attribute_value_conflict","message":"private diagnostic"}`
	mu.Unlock()
	attributeError(t, attributeOperation(t, b, "set_person_attribute", args), "attribute_value_conflict")
	mu.Lock()
	status = http.StatusOK
	body = strings.Repeat("x", (1<<20)+1)
	mu.Unlock()
	result = attributeOperation(t, b, "list_attribute_definitions", map[string]any{})
	attributeError(t, result, "operation_response_limit_exceeded")
}

func TestMCPAttributeConfirmationRejectsBeforeMutation(t *testing.T) {
	b, st, person := recordAttributesFixture(t)
	calls := 0
	session := operationMCPSession(t, b, []mcpserver.OperationFamily{mcpserver.OperationFamilyAttributes}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		calls++
		return &sdkmcp.ElicitResult{Action: "decline"}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: map[string]any{"person_id": person, "slug": store.AttributeSlugPrimaryChannel, "value": map[string]any{"type": "text", "text": "email"}}})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Equal(t, 1, calls)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel})
	require.NoError(t, err)
	assert.Empty(t, values)
}

func TestMCPAttributeInvalidArgumentsDoNotReachNativeRoute(t *testing.T) {
	b, st, person := recordAttributesFixture(t)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"set_person_attribute", map[string]any{"person_id": person, "slug": "../bad", "value": map[string]any{"type": "text", "text": "x"}}},
		{"clear_person_attribute", map[string]any{"person_id": person, "slug": store.AttributeSlugPrimaryChannel, "ordinal": -1}},
		{"list_person_attributes", map[string]any{"person_id": person, "slug": "primary_channel", "universal_id": "example"}},
		{"patch_attribute_definition", map[string]any{"definition_id": int64(1), "etag": "x", "changes": map[string]any{"slug": "other"}}},
	} {
		t.Run(tc.name, func(t *testing.T) { attributeError(t, attributeOperation(t, b, tc.name, tc.args), "invalid_arguments") })
	}
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel})
	require.NoError(t, err)
	assert.Empty(t, values)
}

func TestMCPAttributeDaemonFlagIsIndependent(t *testing.T) {
	command := &cobra.Command{Use: "fixture"}
	addMCPWriteFlags(command, "mcp-")
	var opts mcpserver.ServeOptions
	applyMCPWriteFlags(command, "mcp-", &opts)
	assert.NotContains(t, opts.OperationWriteFamilies, mcpserver.OperationFamilyAttributes)
	require.NoError(t, command.Flags().Set("mcp-allow-attribute-writes", "true"))
	applyMCPWriteFlags(command, "mcp-", &opts)
	assert.Contains(t, opts.OperationWriteFamilies, mcpserver.OperationFamilyAttributes)
	assert.NotContains(t, opts.OperationWriteFamilies, mcpserver.OperationFamilyRecords)
}

func TestMCPAttributeSDKJSONValueRoundTrip(t *testing.T) {
	b, _, person := recordAttributesFixture(t)
	session := operationMCPSession(t, b, []mcpserver.OperationFamily{mcpserver.OperationFamilyAttributes}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	call := func(name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err)
		require.NotNil(t, result)
		require.False(t, result.IsError)
		return result
	}
	call("create_attribute_definition", map[string]any{"definition": map[string]any{"object_type": "person", "slug": "metadata", "label": "Metadata", "value_type": "json", "field_type": "json"}})
	call("set_person_attribute", map[string]any{"person_id": person, "slug": "metadata", "value": map[string]any{"type": "json", "json": map[string]any{"level": "green"}}})
	listed := call("list_person_attributes", map[string]any{"person_id": person, "slug": "metadata"})
	data, err := json.Marshal(listed.StructuredContent)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"level":"green"`)
}
