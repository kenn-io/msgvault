package cmd

import (
	"context"
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
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestOrganizationCapabilities(t *testing.T) {
	all := []string{"list_organizations", "get_organization", "list_organization_attributes", "create_organization", "update_organization", "delete_organization", "replace_organization_profile", "set_organization_attribute"}
	properties := []string{"name", "kind", "primary_domain", "description", "retired", "names", "identifiers", "addresses", "contact_points", "media", "categories", "definition_slug", "value", "source", "ordinal", "expected_value_id", "dry_run"}
	for _, tc := range []struct {
		name, missingRoute, missingProperty string
		want                                []string
	}{
		{"all", "", "", all},
		{"no history", "getOrganizationHistory", "", []string{"list_organizations", "list_organization_attributes", "create_organization"}},
		{"no organization", "getOrganization", "", []string{"list_organizations", "list_organization_attributes", "create_organization"}},
		{"no attributes", "listOrganizationAttributes", "", []string{"list_organizations", "get_organization", "create_organization", "update_organization", "delete_organization", "replace_organization_profile"}},
		{"missing update property", "", "patchOrganization", []string{"list_organizations", "get_organization", "list_organization_attributes", "create_organization", "delete_organization", "replace_organization_profile", "set_organization_attribute"}},
		{"missing attribute expectation", "", "setOrganizationAttribute", []string{"list_organizations", "get_organization", "list_organization_attributes", "create_organization", "update_organization", "delete_organization", "replace_organization_profile"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := &apiprotocol.MCPCapabilities{}
			for _, id := range []string{"createOrganization", "patchOrganization", "putOrganizationProfile", "setOrganizationAttribute"} {
				props := properties
				if tc.missingProperty == id {
					if id == "patchOrganization" {
						props = []string{"name", "kind", "primary_domain", "description"}
					} else {
						props = []string{"definition_slug", "value", "source", "ordinal", "dry_run"}
					}
				}
				caps.Routes = append(caps.Routes, apiprotocol.MCPRouteDescriptor{OperationID: id, RequestProperties: props})
			}
			hasRoute := func(id, _, _ string, _ ...string) bool { return id != tc.missingRoute }
			assert.Equal(t, tc.want, organizationMCPCapabilities(hasRoute, caps))
		})
	}
}

func TestOrganizationReadsUseNativeStore(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	must.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
	must.NoError(err)
	var profile generated.OrganizationProfile
	must.NoError(json.Unmarshal(result.Data, &profile))
	checks.Equal(org.ID, profile.Organization.ID)
	checks.NotEmpty(result.ETag)
	found, err := backend.ExecuteOperation(t.Context(), "list_organizations", map[string]any{"q": "Example"})
	must.NoError(err)
	checks.Len(operationOutput[generated.OrganizationsResponse](t, found).Organizations, 1)
	result, err = backend.ExecuteOperation(t.Context(), "list_organization_attributes", map[string]any{"organization_id": org.ID})
	must.NoError(err)
	checks.Empty(operationOutput[generated.OrganizationAttributesResponse](t, result).Values)

}

func TestOrganizationWritesUseRevisionsAndEmploymentGuard(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	created, err := backend.ExecuteOperation(t.Context(), "create_organization", map[string]any{"body": map[string]any{"name": "Example Company", "kind": "company"}})
	must.NoError(err)
	first := operationOutput[mcpserver.OrganizationMutation](t, created)
	must.NotEmpty(first.ETag)
	id := first.Organization.ID
	patched, err := backend.ExecuteOperation(t.Context(), "update_organization", map[string]any{"organization_id": id, "etag": first.ETag, "body": map[string]any{"name": "Example Group", "kind": "company"}})
	must.NoError(err)
	second := operationOutput[mcpserver.OrganizationMutation](t, patched)
	checks.Equal("Example Group", second.Organization.Name)
	blank := map[string]any{"organization_id": id, "etag": "", "body": map[string]any{"name": "Example Blank", "kind": "company"}}
	_, err = backend.OperationDisclosure(t.Context(), "update_organization", blank)
	must.NoError(err)
	missing, err := backend.ExecuteOperation(t.Context(), "update_organization", blank)
	must.NoError(err)
	assertLoopRefusal(t, missing, "if_match_required", false)
	deletable, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Deletable", Kind: store.OrganizationKindCompany})
	must.NoError(err)
	readable, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": deletable.ID})
	must.NoError(err)
	deleteETag := readable.ETag
	deleted, err := backend.ExecuteOperation(t.Context(), "delete_organization", map[string]any{"organization_id": deletable.ID, "etag": deleteETag})
	must.NoError(err)
	checks.True(operationOutput[mcpserver.OrganizationDeleted](t, deleted).Deleted)
}

func TestOrganizationAttributeApprovalAndMedia(t *testing.T) {
	must := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: "test-org-industry", ObjectType: store.AttributeObjectOrganization, Slug: "industry", Label: "Industry", ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText, Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser, APIMutable: true, UICreatable: true, UIEditable: true, IsDeletable: true})
	must.NoError(err)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	must.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	read, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
	must.NoError(err)
	bad := map[string]any{"organization_id": org.ID, "etag": read.ETag, "body": map[string]any{"media": []any{map[string]any{"media_kind": "logo", "source": "user", "data": "!!!"}}}}
	_, err = backend.OperationDisclosure(t.Context(), "replace_organization_profile", bad)
	var refusal *mcpserver.OperationRefusalError
	must.ErrorAs(err, &refusal)
	must.Equal("invalid_arguments", refusal.Code)
	args := map[string]any{"organization_id": org.ID, "body": map[string]any{"definition_slug": "industry", "source": "user", "value": map[string]any{"type": "text", "text": "software"}, "expected_value_id": 0}}
	disclosure, err := backend.OperationDisclosure(t.Context(), "set_organization_attribute", args)
	must.NoError(err)
	must.Contains(disclosure, `"superseded":null`)
	first, err := backend.ExecuteOperation(t.Context(), "set_organization_attribute", args)
	must.NoError(err)
	must.False(first.IsError)
	stale, err := backend.ExecuteOperation(t.Context(), "set_organization_attribute", args)
	must.NoError(err)
	assertLoopRefusal(t, stale, "attribute_value_conflict", false)

}

func TestOrganizationLargeProfileResponse(t *testing.T) {
	must := require.New(t)
	st := testutil.NewTestStore(t)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	must.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	read, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
	must.NoError(err)
	names := make([]any, 0, store.MaxOrganizationProfileValues)
	for i := range store.MaxOrganizationProfileValues {
		names = append(names, map[string]any{"name": fmt.Sprintf("Name %03d %s", i, strings.Repeat("a", 6<<10)), "name_kind": "alias", "source": "user"})
	}
	replaced, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", map[string]any{"organization_id": org.ID, "etag": read.ETag, "body": map[string]any{"names": names}})
	must.NoError(err)
	receipt := operationOutput[mcpserver.OrganizationRecord](t, replaced)
	must.NotEmpty(receipt.ETag)
	must.Nil(receipt.Profile)
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyOrganizations}, nil)
	args := map[string]any{"organization_id": org.ID, "include_superseded": true}
	var data []byte
	for {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_organization", Arguments: args})
		must.NoError(err)
		must.False(result.IsError)
		chunk := loopToolOutput[mcpserver.OrganizationRecord](t, result)
		must.NotNil(chunk.DataBase64)
		part, err := base64.StdEncoding.DecodeString(*chunk.DataBase64)
		must.NoError(err)
		data = append(data, part...)
		if *chunk.Complete {
			break
		}
		args = map[string]any{"organization_id": org.ID, "include_superseded": true, "offset": *chunk.Offset + *chunk.Length, "sha256": *chunk.SHA256}
	}
	var profile generated.OrganizationProfile
	must.NoError(json.Unmarshal(data, &profile))
	must.Len(profile.Names, store.MaxOrganizationProfileValues)
}

func TestOrganizationSDKRequiresConfirmationAndRejectsUnknownFields(t *testing.T) {
	must := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: "test-org-json", ObjectType: store.AttributeObjectOrganization, Slug: "metadata", Label: "Metadata", ValueType: store.AttributeValueJSON, FieldType: store.AttributeFieldJSON, Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser, APIMutable: true, UICreatable: true, UIEditable: true, IsDeletable: true})
	must.NoError(err)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example SDK"})
	must.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyOrganizations}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	invalid, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_organization", Arguments: map[string]any{"body": map[string]any{"name": "Example", "unexpected": "value"}}})
	must.NoError(err)
	must.True(invalid.IsError)
	must.Zero(approvals)
	missing, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_organization", Arguments: map[string]any{"organization_id": 999, "etag": `"organization-999-r1"`, "body": map[string]any{"name": "Missing"}}})
	must.NoError(err)
	must.True(missing.IsError)
	data, err := json.Marshal(missing.StructuredContent)
	must.NoError(err)
	var refusal map[string]any
	must.NoError(json.Unmarshal(data, &refusal))
	must.Equal("organization_not_found", refusal["error"])
	must.Zero(approvals)
	set, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_organization_attribute", Arguments: map[string]any{"organization_id": org.ID, "body": map[string]any{"definition_slug": "metadata", "value": map[string]any{"type": "json", "json": map[string]any{"enabled": true}}, "source": "user", "expected_value_id": 0}}})
	must.NoError(err)
	must.False(set.IsError)
	must.Equal(1, approvals)
	listed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_organization_attributes", Arguments: map[string]any{"organization_id": org.ID}})
	must.NoError(err)
	must.False(listed.IsError)
	data, err = json.Marshal(listed.StructuredContent)
	must.NoError(err)
	var attributes generated.OrganizationAttributesResponse
	must.NoError(json.Unmarshal(data, &attributes))
	must.Len(attributes.Values, 1)
	must.JSONEq(`{"enabled":true}`, string(attributes.Values[0].Value.JSON))
}

func TestApprovalMatrixOrganization(t *testing.T) {
	exerciseApprovalMatrix(t, func(t *testing.T) approvalCase {
		st := testutil.NewTestStore(t)
		_, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: "test-org-matrix", ObjectType: store.AttributeObjectOrganization, Slug: "industry", Label: "Industry", ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText, Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser, APIMutable: true, UICreatable: true, UIEditable: true, IsDeletable: true})
		require.NoError(t, err)
		org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example"})
		require.NoError(t, err)
		b := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
		args := map[string]any{"organization_id": org.ID, "body": map[string]any{"definition_slug": "industry", "source": "user", "value": map[string]any{"type": "text", "text": "Approved"}, "expected_value_id": 0}}
		return approvalCase{b, mcpserver.OperationFamilyOrganizations, "set_organization_attribute", args, func() {
			value := "Concurrent"
			_, err := st.SetOrganizationAttributeValueContext(t.Context(), store.OrganizationAttributeValueInput{OrganizationID: org.ID, DefinitionSlug: "industry", Source: store.ProvenanceUser, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &value}})
			require.NoError(t, err)
		}}
	})
}
