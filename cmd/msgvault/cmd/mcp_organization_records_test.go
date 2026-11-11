package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
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

func TestOrganizationReadsUseNativeStore(t *testing.T) {
	st := testutil.NewTestStore(t)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	for _, name := range []string{"get_organization", "get_organization_history"} {
		result, err := backend.ExecuteOperation(t.Context(), name, map[string]any{"organization_id": org.ID})
		require.NoError(t, err)
		profile := operationOutput[mcpserver.OrganizationRecord](t, result)
		assert.Equal(t, org.ID, profile.Profile.Organization.ID)
		assert.NotEmpty(t, profile.ETag)
	}
	result, err := backend.ExecuteOperation(t.Context(), "list_organization_attributes", map[string]any{"organization_id": org.ID})
	require.NoError(t, err)
	require.False(t, result.IsError)
	result, err = backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": -1})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestOrganizationWritesUseRevisionsAndEmploymentGuard(t *testing.T) {
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	created, err := backend.ExecuteOperation(t.Context(), "create_organization", map[string]any{"body": map[string]any{"name": "Example Company", "kind": "company"}})
	require.NoError(t, err)
	first := operationOutput[mcpserver.OrganizationMutation](t, created)
	require.NotEmpty(t, first.ETag)
	id := first.Organization.ID
	patched, err := backend.ExecuteOperation(t.Context(), "update_organization", map[string]any{"organization_id": id, "etag": first.ETag, "body": map[string]any{"name": "Example Group", "kind": "company"}})
	require.NoError(t, err)
	second := operationOutput[mcpserver.OrganizationMutation](t, patched)
	assert.Equal(t, "Example Group", second.Organization.Name)
	stale, err := backend.ExecuteOperation(t.Context(), "delete_organization", map[string]any{"organization_id": id, "etag": first.ETag})
	require.NoError(t, err)
	assert.True(t, stale.IsError)
	participant, err := st.EnsureParticipant("employee@example.com", "Example Employee", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	_, err = st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person.ID, OrganizationID: id, Source: store.ProvenanceUser})
	require.NoError(t, err)
	refused, err := backend.ExecuteOperation(t.Context(), "delete_organization", map[string]any{"organization_id": id, "etag": second.ETag})
	require.NoError(t, err)
	assert.True(t, refused.IsError)
	encoded, err := json.Marshal(refused.Output)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "organization_has_employments")
	_, err = st.GetOrganizationContext(t.Context(), id)
	assert.NoError(t, err)
}

func TestOrganizationProfileMediaAndAttributesThroughNativeRoutes(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: "test-org-industry", ObjectType: store.AttributeObjectOrganization, Slug: "industry", Label: "Industry", ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText, Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser, APIMutable: true, UICreatable: true, UIEditable: true, IsDeletable: true})
	require.NoError(t, err)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	read, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
	require.NoError(t, err)
	initial := operationOutput[mcpserver.OrganizationRecord](t, read)
	args := map[string]any{"organization_id": org.ID, "etag": initial.ETag, "body": map[string]any{"names": []any{map[string]any{"name": "Example Alias", "name_kind": "alias", "source": "user"}}, "media": []any{map[string]any{"media_kind": "logo", "source": "user", "data": "YWJjZGVm"}, map[string]any{"media_kind": "photo", "source": "user", "ordinal": 1, "uri": "https://example.com/photo.png"}}}}
	disclosure, err := backend.OperationDisclosure(t.Context(), "replace_organization_profile", args)
	require.NoError(t, err)
	assert.NotContains(t, disclosure, "YWJjZGVm")
	assert.Contains(t, disclosure, "replace all profile collections")
	result, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", args)
	require.NoError(t, err)
	profile := operationOutput[mcpserver.OrganizationRecord](t, result)
	require.Len(t, profile.Profile.Media, 2)
	var mediaID, uriID int64
	for _, media := range profile.Profile.Media {
		if media.HasData {
			mediaID = media.Envelope.ID
		} else {
			uriID = media.Envelope.ID
		}
	}
	require.NotZero(t, mediaID)
	require.NotZero(t, uriID)
	uriResult, err := backend.ExecuteOperation(t.Context(), "get_organization_media", map[string]any{"organization_id": org.ID, "media_id": uriID})
	require.NoError(t, err)
	assert.True(t, uriResult.IsError)
	firstResult, err := backend.ExecuteOperation(t.Context(), "get_organization_media", map[string]any{"organization_id": org.ID, "media_id": mediaID, "length": 3})
	require.NoError(t, err)
	first := operationOutput[mcpserver.OrganizationMediaChunk](t, firstResult)
	assert.Equal(t, "YWJj", first.DataBase64)
	assert.False(t, first.Complete)
	nextResult, err := backend.ExecuteOperation(t.Context(), "get_organization_media", map[string]any{"organization_id": org.ID, "media_id": mediaID, "offset": 3, "sha256": first.SHA256})
	require.NoError(t, err)
	next := operationOutput[mcpserver.OrganizationMediaChunk](t, nextResult)
	assert.Equal(t, "ZGVm", next.DataBase64)
	assert.True(t, next.Complete)
	invalid, err := backend.ExecuteOperation(t.Context(), "get_organization_media", map[string]any{"organization_id": org.ID, "media_id": mediaID, "offset": 3})
	require.NoError(t, err)
	assert.True(t, invalid.IsError)
	stale, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", args)
	require.NoError(t, err)
	assert.True(t, stale.IsError)
	clearProfile, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", map[string]any{"organization_id": org.ID, "etag": profile.ETag, "body": map[string]any{}})
	require.NoError(t, err)
	clearedProfile := operationOutput[mcpserver.OrganizationRecord](t, clearProfile)
	assert.Empty(t, clearedProfile.Profile.Names)
	history, err := backend.ExecuteOperation(t.Context(), "get_organization_history", map[string]any{"organization_id": org.ID})
	require.NoError(t, err)
	assert.NotEmpty(t, operationOutput[mcpserver.OrganizationRecord](t, history).Profile.Names)
	set, err := backend.ExecuteOperation(t.Context(), "set_organization_attribute", map[string]any{"organization_id": org.ID, "body": map[string]any{"definition_slug": "industry", "value": map[string]any{"type": "text", "text": "software"}, "source": "user"}})
	require.NoError(t, err)
	write := operationOutput[generated.OrganizationAttributeWrite](t, set)
	require.NotNil(t, write.Value)
	preview, err := backend.ExecuteOperation(t.Context(), "clear_organization_attribute", map[string]any{"organization_id": org.ID, "slug": "industry", "expected_value_id": write.Value.ID, "dry_run": true})
	require.NoError(t, err)
	assert.True(t, operationOutput[generated.OrganizationAttributeWrite](t, preview).DryRun)
	clear, err := backend.ExecuteOperation(t.Context(), "clear_organization_attribute", map[string]any{"organization_id": org.ID, "slug": "industry", "expected_value_id": write.Value.ID})
	require.NoError(t, err)
	require.NotNil(t, operationOutput[generated.OrganizationAttributeWrite](t, clear).Superseded)
}

func TestOrganizationMergeAndDeleteThroughNativeRoutes(t *testing.T) {
	st := testutil.NewTestStore(t)
	first, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Survivor", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	second, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Losing", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	read, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": first.ID})
	require.NoError(t, err)
	etag := operationOutput[mcpserver.OrganizationRecord](t, read).ETag
	merged, err := backend.ExecuteOperation(t.Context(), "merge_organization", map[string]any{"organization_id": first.ID, "etag": etag, "body": map[string]any{"losing_organization_id": second.ID, "losing_revision": second.Revision}})
	require.NoError(t, err)
	survivor := operationOutput[mcpserver.OrganizationMutation](t, merged)
	assert.Greater(t, survivor.Organization.Revision, first.Revision)
	losing, err := st.GetOrganizationContext(t.Context(), second.ID)
	require.NoError(t, err)
	require.NotNil(t, losing.MergedIntoID)
	assert.Equal(t, first.ID, *losing.MergedIntoID)
	deletable, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Deletable", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	readable, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": deletable.ID})
	require.NoError(t, err)
	deleteETag := operationOutput[mcpserver.OrganizationRecord](t, readable).ETag
	deleted, err := backend.ExecuteOperation(t.Context(), "delete_organization", map[string]any{"organization_id": deletable.ID, "etag": deleteETag})
	require.NoError(t, err)
	assert.True(t, operationOutput[mcpserver.OrganizationDeleted](t, deleted).Deleted)
}

func TestOrganizationSDKRequiresConfirmationAndRejectsUnknownFields(t *testing.T) {
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyOrganizations}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assert.Contains(t, request.Params.Message, "create an organization")
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	tools, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	var createFound bool
	for _, tool := range tools.Tools {
		if tool.Name == "create_organization" {
			createFound = true
		}
	}
	require.True(t, createFound)
	invalid, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_organization", Arguments: map[string]any{"body": map[string]any{"name": "Example", "unexpected": "value"}}})
	require.NoError(t, err)
	assert.True(t, invalid.IsError)
	assert.Zero(t, approvals)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_organization", Arguments: map[string]any{"body": map[string]any{"name": "Example SDK", "kind": "company"}}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Equal(t, 1, approvals)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var created mcpserver.OrganizationMutation
	require.NoError(t, json.Unmarshal(data, &created))
	stored, err := st.GetOrganizationContext(t.Context(), created.Organization.ID)
	require.NoError(t, err)
	assert.Equal(t, "Example SDK", stored.Name)
}

func TestOrganizationErrorsHideDetailsAndPreserveUncertainty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"organization_failed","message":"private backend details"}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	backend := &daemonMCPOperations{client: client, supported: []string{"create_organization"}}
	result, err := backend.ExecuteOperation(t.Context(), "create_organization", map[string]any{"body": map[string]any{"name": "Example"}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "private backend details")
	assert.Contains(t, string(data), `"operation_may_have_completed":true`)
	unsupported, err := backend.ExecuteOperation(t.Context(), "delete_organization", map[string]any{"organization_id": 1, "etag": "invalid"})
	require.NoError(t, err)
	assert.True(t, unsupported.IsError)
}

func TestOrganizationSDKDeclinedConfirmationDoesNotWrite(t *testing.T) {
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyOrganizations}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "decline"}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_organization", Arguments: map[string]any{"body": map[string]any{"name": "Declined Example"}}})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	organizations, err := st.ListOrganizationsContext(t.Context(), store.OrganizationFilter{})
	require.NoError(t, err)
	assert.Empty(t, organizations)
}

func TestOrganizationWriteFlagForEmbeddedServer(t *testing.T) {
	cmd := &cobra.Command{}
	addMCPWriteFlags(cmd, "mcp-")
	var opts mcpserver.ServeOptions
	applyMCPWriteFlags(cmd, "mcp-", &opts)
	assert.NotContains(t, opts.OperationWriteFamilies, mcpserver.OperationFamilyOrganizations)
	require.NoError(t, cmd.Flags().Set("mcp-allow-organization-writes", "true"))
	opts = mcpserver.ServeOptions{}
	applyMCPWriteFlags(cmd, "mcp-", &opts)
	assert.Contains(t, opts.OperationWriteFamilies, mcpserver.OperationFamilyOrganizations)
}
