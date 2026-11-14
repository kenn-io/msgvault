package cmd

import (
	"context"
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

func TestOrganizationCapabilities(t *testing.T) {
	all := []string{"list_organizations", "get_organization", "list_organization_attributes", "create_organization", "update_organization", "delete_organization", "replace_organization_profile", "set_organization_attribute"}
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
			backend, _, _, _ := employmentFixture(t)
			caps, err := backend.client.MCPCapabilities(t.Context())
			require.NoError(t, err)
			caps.Routes = slices.DeleteFunc(caps.Routes, func(route apiprotocol.MCPRouteDescriptor) bool { return route.OperationID == tc.missingRoute })
			for i := range caps.Routes {
				if caps.Routes[i].OperationID == tc.missingProperty {
					property := "expected_value_id"
					if tc.missingProperty == "patchOrganization" {
						property = "retired"
					}
					caps.Routes[i].RequestProperties = slices.DeleteFunc(caps.Routes[i].RequestProperties, func(p string) bool { return p == property })
				}
			}
			got := newDaemonMCPOperations(backend.client, caps).capabilities()
			for _, name := range all {
				assert.Equal(t, slices.Contains(tc.want, name), slices.Contains(got, name), name)
			}
		})
	}
}

func TestOrganizationReadsUseNativeStore(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	must.NoError(err)
	_, err = st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Unrelated Company", Kind: store.OrganizationKindCompany})
	must.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	found, err := backend.ExecuteOperation(t.Context(), "list_organizations", map[string]any{"q": "Example"})
	must.NoError(err)
	organizations := operationOutput[generated.OrganizationsResponse](t, found).Organizations
	must.Len(organizations, 1)
	checks.Equal(org.ID, organizations[0].ID)
}

func TestOrganizationWritesUseRevisions(t *testing.T) {
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
	assertDefiniteRefusal(t, missing, "if_match_required")
}

func TestOrganizationAttributeDisclosureAndInvalidMedia(t *testing.T) {
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
	body := map[string]any{"definition_slug": "industry", "source": "user", "value": map[string]any{"type": "text", "text": "software"}, "expected_value_id": 0, "ordinal": 0}
	args := map[string]any{"organization_id": org.ID, "body": body}
	disclosure, err := backend.OperationDisclosure(t.Context(), "set_organization_attribute", args)
	must.NoError(err)
	must.Contains(disclosure, `"superseded":null`)
	values, err := st.ListOrganizationAttributeValuesContext(t.Context(), org.ID, store.OrganizationAttributeQuery{})
	must.NoError(err)
	must.Empty(values)
	delete(body, "ordinal")
	_, err = backend.OperationDisclosure(t.Context(), "set_organization_attribute", args)
	must.ErrorAs(err, &refusal)
	must.Equal("invalid_arguments", refusal.Code)
}

func readOrganizationChunks(t *testing.T, session *sdkmcp.ClientSession, name string, id int64) []byte {
	t.Helper()
	must := require.New(t)
	args := map[string]any{"organization_id": id, "include_superseded": true}
	var data []byte
	for {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		must.NoError(err)
		must.False(result.IsError)
		chunk := loopToolOutput[mcpserver.PersonRecordChunk](t, result)
		must.NotNil(chunk.DataBase64)
		part, err := base64.StdEncoding.DecodeString(*chunk.DataBase64)
		must.NoError(err)
		data = append(data, part...)
		if *chunk.Complete {
			return data
		}
		args = map[string]any{"organization_id": id, "include_superseded": true, "offset": *chunk.Offset + *chunk.Length, "sha256": *chunk.SHA256}
	}
}

func TestOrganizationLargeProfileResponse(t *testing.T) {
	must := require.New(t)
	st := testutil.NewTestStore(t)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	must.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	read, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
	must.NoError(err)
	initial, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", map[string]any{"organization_id": org.ID, "etag": read.ETag, "body": map[string]any{"names": []any{map[string]any{"name": "Former alias", "name_kind": "alias", "source": "user"}}}})
	must.NoError(err)
	initialReceipt := operationOutput[mcpserver.OrganizationRecord](t, initial)
	names := make([]any, 0, store.MaxOrganizationProfileValues)
	for i := range store.MaxOrganizationProfileValues {
		names = append(names, map[string]any{"name": fmt.Sprintf("Name %03d %s", i, strings.Repeat("a", 6<<10)), "name_kind": "alias", "source": "user"})
	}
	replaced, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", map[string]any{"organization_id": org.ID, "etag": initialReceipt.ETag, "body": map[string]any{"names": names}})
	must.NoError(err)
	receipt := operationOutput[mcpserver.OrganizationRecord](t, replaced)
	must.NotEmpty(receipt.ETag)
	must.Nil(receipt.Profile)
	disclosure, err := backend.OperationDisclosure(t.Context(), "update_organization", map[string]any{"organization_id": org.ID, "etag": receipt.ETag, "body": map[string]any{"name": "Example Group", "kind": "company"}})
	must.NoError(err)
	must.Less(len(disclosure), 4096)
	must.Contains(disclosure, "Example Company")
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyOrganizations}, nil)
	data := readOrganizationChunks(t, session, "get_organization", org.ID)
	var profile generated.OrganizationProfile
	must.NoError(json.Unmarshal(data, &profile))
	must.Len(profile.Names, store.MaxOrganizationProfileValues+1)
	var former *generated.OrganizationName
	for i := range profile.Names {
		if profile.Names[i].Name == "Former alias" {
			former = &profile.Names[i]
		}
	}
	must.NotNil(former)
	must.NotNil(former.Envelope.SupersededAt)
	current, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
	must.NoError(err)
	must.False(current.IsError)
	var currentProfile generated.OrganizationProfile
	must.NoError(json.Unmarshal(current.Data, &currentProfile))
	must.Len(currentProfile.Names, store.MaxOrganizationProfileValues)
	must.NotContains(string(current.Data), "Former alias")
}

func TestOrganizationSDKRequiresConfirmation(t *testing.T) {
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
	missing, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_organization", Arguments: map[string]any{"organization_id": 999, "etag": `"organization-999-r1"`, "body": map[string]any{"name": "Missing"}}})
	must.NoError(err)
	must.Equal("organization_not_found", loopToolFailure(t, missing)["error"])
	must.Zero(approvals)
	set, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_organization_attribute", Arguments: map[string]any{"organization_id": org.ID, "body": map[string]any{"definition_slug": "metadata", "value": map[string]any{"type": "json", "json": map[string]any{"enabled": true}}, "source": "user", "expected_value_id": 0, "ordinal": 0}}})
	must.NoError(err)
	must.False(set.IsError)
	must.Equal(1, approvals)
	listed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_organization_attributes", Arguments: map[string]any{"organization_id": org.ID}})
	must.NoError(err)
	attributes := loopToolOutput[generated.OrganizationAttributesResponse](t, listed)
	must.Len(attributes.Values, 1)
	must.JSONEq(`{"enabled":true}`, string(attributes.Values[0].Value.JSON))
}

func TestApprovalMatrixOrganization(t *testing.T) {
	t.Run("set attribute", func(t *testing.T) {
		exerciseApprovalMatrix(t, func(t *testing.T) approvalCase {
			t.Helper()
			st := testutil.NewTestStore(t)
			_, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: "test-org-matrix", ObjectType: store.AttributeObjectOrganization, Slug: "industry", Label: "Industry", ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText, Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser, APIMutable: true, UICreatable: true, UIEditable: true, IsDeletable: true})
			require.NoError(t, err)
			org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example"})
			require.NoError(t, err)
			b := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
			args := map[string]any{"organization_id": org.ID, "body": map[string]any{"definition_slug": "industry", "source": "user", "value": map[string]any{"type": "text", "text": "Approved"}, "expected_value_id": 0, "ordinal": 0}}
			return approvalCase{b, mcpserver.OperationFamilyOrganizations, "set_organization_attribute", args, func() {
				value := "Concurrent"
				_, err := st.SetOrganizationAttributeValueContext(t.Context(), store.OrganizationAttributeValueInput{OrganizationID: org.ID, DefinitionSlug: "industry", Source: store.ProvenanceUser, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &value}})
				require.NoError(t, err)
			}}
		})
	})
	t.Run("delete organization", func(t *testing.T) {
		exerciseApprovalMatrix(t, func(t *testing.T) approvalCase {
			t.Helper()
			st := testutil.NewTestStore(t)
			org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Original", Kind: store.OrganizationKindCompany})
			require.NoError(t, err)
			b := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
			read, err := b.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
			require.NoError(t, err)
			return approvalCase{b, mcpserver.OperationFamilyOrganizations, "delete_organization", map[string]any{"organization_id": org.ID, "etag": read.ETag}, func() {
				result, err := b.ExecuteOperation(t.Context(), "update_organization", map[string]any{"organization_id": org.ID, "etag": read.ETag, "body": map[string]any{"name": "Concurrent", "kind": "company"}})
				require.NoError(t, err)
				require.False(t, result.IsError)
			}}
		})
	})
}
