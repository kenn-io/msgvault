package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func TestOrganizationCapabilities(t *testing.T) {
	all := []string{"get_organization", "get_organization_history", "list_organization_attributes", "get_organization_media", "create_organization", "update_organization", "delete_organization", "merge_organization", "replace_organization_profile", "set_organization_attribute", "clear_organization_attribute"}
	properties := []string{"name", "kind", "primary_domain", "description", "retired", "losing_organization_id", "losing_revision", "names", "identifiers", "addresses", "contact_points", "media", "categories", "definition_slug", "value", "source", "ordinal", "expected_value_id", "dry_run"}
	for _, tc := range []struct {
		name                                string
		missingReads, missingUpdateProperty bool
		want                                []string
	}{
		{"all", false, false, all},
		{"no reads", true, false, []string{"get_organization_media", "create_organization"}},
		{"missing update property", false, true, []string{"get_organization", "get_organization_history", "list_organization_attributes", "get_organization_media", "create_organization", "delete_organization", "merge_organization", "replace_organization_profile", "set_organization_attribute", "clear_organization_attribute"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := &apiprotocol.MCPCapabilities{}
			for _, id := range []string{"createOrganization", "patchOrganization", "mergeOrganization", "putOrganizationProfile", "setOrganizationAttribute"} {
				props := properties
				if tc.missingUpdateProperty && id == "patchOrganization" {
					props = []string{"name", "kind", "primary_domain", "description"}
				}
				caps.Routes = append(caps.Routes, apiprotocol.MCPRouteDescriptor{OperationID: id, RequestProperties: props})
			}
			hasRoute := func(id, _, _ string, _ ...string) bool {
				return !tc.missingReads || (id != "getOrganization" && id != "getOrganizationHistory" && id != "listOrganizationAttributes")
			}
			assert.Equal(t, tc.want, organizationMCPCapabilities(hasRoute, caps))
		})
	}
}

func TestOrganizationReadsUseNativeStore(t *testing.T) {
	st := testutil.NewTestStore(t)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
	require.NoError(t, err)
	profile := operationOutput[mcpserver.OrganizationRecord](t, result)
	assert.Equal(t, org.ID, profile.Profile.Organization.ID)
	assert.NotEmpty(t, profile.ETag)
	result, err = backend.ExecuteOperation(t.Context(), "list_organization_attributes", map[string]any{"organization_id": org.ID})
	require.NoError(t, err)
	assert.Empty(t, operationOutput[generated.OrganizationAttributesResponse](t, result).Values)
	result, err = backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": -1})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.Contains(t, string(data), "invalid_arguments")
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
	staleData, err := json.Marshal(stale.Output)
	require.NoError(t, err)
	assert.Contains(t, string(staleData), "organization_revision_conflict")
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
	deletable, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Deletable", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	readable, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": deletable.ID})
	require.NoError(t, err)
	deleteETag := operationOutput[mcpserver.OrganizationRecord](t, readable).ETag
	deleted, err := backend.ExecuteOperation(t.Context(), "delete_organization", map[string]any{"organization_id": deletable.ID, "etag": deleteETag})
	require.NoError(t, err)
	assert.True(t, operationOutput[mcpserver.OrganizationDeleted](t, deleted).Deleted)
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
	t.Run("media", func(t *testing.T) {
		require.Len(t, profile.Profile.Media, 2)
		disclosureArgs := map[string]any{"organization_id": org.ID, "etag": profile.ETag, "body": map[string]any{"media": []any{map[string]any{"media_kind": "logo", "source": "user", "data": "YWJjZGVm"}}}}
		disclosure, err := backend.OperationDisclosure(t.Context(), "replace_organization_profile", disclosureArgs)
		require.NoError(t, err)
		digest := sha256.Sum256([]byte("abcdef"))
		assert.Contains(t, disclosure, `"bytes":6`)
		assert.Contains(t, disclosure, fmt.Sprintf(`"sha256":"%x"`, digest))
		assert.NotContains(t, disclosure, "YWJjZGVm")
		badArgs := map[string]any{"organization_id": org.ID, "etag": profile.ETag, "body": map[string]any{"media": []any{map[string]any{"media_kind": "logo", "source": "user", "data": "!!!"}}}}
		_, err = backend.OperationDisclosure(t.Context(), "replace_organization_profile", badArgs)
		var refusal *mcpserver.OperationRefusalError
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, "invalid_arguments", refusal.Code)
		oversized := base64.StdEncoding.EncodeToString(make([]byte, store.MaxPersonMediaBytes+1))
		tooLarge, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", map[string]any{"organization_id": org.ID, "etag": profile.ETag, "body": map[string]any{"media": []any{map[string]any{"media_kind": "logo", "source": "user", "data": oversized}}}})
		require.NoError(t, err)
		assertLoopRefusal(t, tooLarge, "invalid_organization", false)
		unchanged, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
		require.NoError(t, err)
		assert.Equal(t, profile, operationOutput[mcpserver.OrganizationRecord](t, unchanged))
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
		uriData, err := json.Marshal(uriResult.Output)
		require.NoError(t, err)
		assert.Contains(t, string(uriData), "profile_media_content_unavailable")
		firstResult, err := backend.ExecuteOperation(t.Context(), "get_organization_media", map[string]any{"organization_id": org.ID, "media_id": mediaID, "length": float64(3)})
		require.NoError(t, err)
		first := operationOutput[mcpserver.PersonMediaContent](t, firstResult)
		assert.Equal(t, "YWJj", first.DataBase64)
		assert.False(t, first.Complete)
		nextResult, err := backend.ExecuteOperation(t.Context(), "get_organization_media", map[string]any{"organization_id": org.ID, "media_id": mediaID, "offset": float64(3), "sha256": first.SHA256})
		require.NoError(t, err)
		next := operationOutput[mcpserver.PersonMediaContent](t, nextResult)
		assert.Equal(t, "ZGVm", next.DataBase64)
		assert.True(t, next.Complete)
		invalid, err := backend.ExecuteOperation(t.Context(), "get_organization_media", map[string]any{"organization_id": org.ID, "media_id": mediaID, "offset": float64(3)})
		require.NoError(t, err)
		assert.True(t, invalid.IsError)
		invalidData, err := json.Marshal(invalid.Output)
		require.NoError(t, err)
		assert.Contains(t, string(invalidData), "invalid_arguments")
	})
	stale, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", args)
	require.NoError(t, err)
	assert.True(t, stale.IsError)
	staleData, err := json.Marshal(stale.Output)
	require.NoError(t, err)
	assert.Contains(t, string(staleData), "organization_revision_conflict")
	clearProfile, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", map[string]any{"organization_id": org.ID, "etag": profile.ETag, "body": map[string]any{}})
	require.NoError(t, err)
	clearedProfile := operationOutput[mcpserver.OrganizationRecord](t, clearProfile)
	assert.Empty(t, clearedProfile.Profile.Names)
	history, err := backend.ExecuteOperation(t.Context(), "get_organization_history", map[string]any{"organization_id": org.ID})
	require.NoError(t, err)
	assert.NotEmpty(t, operationOutput[mcpserver.OrganizationRecord](t, history).Profile.Names)
	t.Run("attributes", func(t *testing.T) {
		invalid, err := backend.ExecuteOperation(t.Context(), "clear_organization_attribute", map[string]any{"organization_id": org.ID, "slug": "industry", "ordinal": -1})
		require.NoError(t, err)
		assertLoopRefusal(t, invalid, "invalid_ordinal", false)
		set, err := backend.ExecuteOperation(t.Context(), "set_organization_attribute", map[string]any{"organization_id": org.ID, "body": map[string]any{"definition_slug": "industry", "value": map[string]any{"type": "text", "text": "software"}, "source": "user"}})
		require.NoError(t, err)
		write := operationOutput[generated.OrganizationAttributeWrite](t, set)
		require.NotNil(t, write.Value)
		for _, tc := range []struct {
			name string
			args map[string]any
		}{
			{"set_organization_attribute", map[string]any{"organization_id": org.ID, "body": map[string]any{"definition_slug": "industry", "value": map[string]any{"type": "text", "text": "services"}, "source": "user"}}},
			{"clear_organization_attribute", map[string]any{"organization_id": org.ID, "slug": "industry"}},
		} {
			t.Run(tc.name+" disclosure", func(t *testing.T) {
				disclosure, err := backend.OperationDisclosure(t.Context(), tc.name, tc.args)
				require.NoError(t, err)
				var decoded struct {
					Current mcpserver.OrganizationRecord         `json:"current"`
					Preview generated.OrganizationAttributeWrite `json:"preview"`
				}
				require.NoError(t, json.Unmarshal([]byte(disclosure), &decoded))
				assert.Equal(t, org.ID, decoded.Current.Profile.Organization.ID)
				assert.True(t, decoded.Preview.DryRun)
				require.NotNil(t, decoded.Preview.Superseded)
				assert.Equal(t, write.Value.ID, decoded.Preview.Superseded.ID)
				missing := make(map[string]any, len(tc.args))
				for key, value := range tc.args {
					missing[key] = value
				}
				missing["organization_id"] = int64(999999)
				prompt, err := backend.OperationDisclosure(t.Context(), tc.name, missing)
				assert.Empty(t, prompt)
				var refusal *mcpserver.OperationRefusalError
				require.ErrorAs(t, err, &refusal)
				assert.Equal(t, "organization_not_found", refusal.Code)
			})
		}
		preview, err := backend.ExecuteOperation(t.Context(), "clear_organization_attribute", map[string]any{"organization_id": org.ID, "slug": "industry", "expected_value_id": write.Value.ID, "dry_run": true})
		require.NoError(t, err)
		assert.True(t, operationOutput[generated.OrganizationAttributeWrite](t, preview).DryRun)
		clear, err := backend.ExecuteOperation(t.Context(), "clear_organization_attribute", map[string]any{"organization_id": org.ID, "slug": "industry", "expected_value_id": write.Value.ID})
		require.NoError(t, err)
		require.NotNil(t, operationOutput[generated.OrganizationAttributeWrite](t, clear).Superseded)
		absentDisclosure, err := backend.OperationDisclosure(t.Context(), "set_organization_attribute", map[string]any{"organization_id": org.ID, "body": map[string]any{"definition_slug": "industry", "value": map[string]any{"type": "text", "text": "services"}, "source": "user"}})
		require.NoError(t, err)
		var absent struct {
			Preview generated.OrganizationAttributeWrite `json:"preview"`
		}
		require.NoError(t, json.Unmarshal([]byte(absentDisclosure), &absent))
		assert.True(t, absent.Preview.DryRun)
		assert.Nil(t, absent.Preview.Superseded)
		active, err := backend.ExecuteOperation(t.Context(), "list_organization_attributes", map[string]any{"organization_id": org.ID})
		require.NoError(t, err)
		assert.Empty(t, operationOutput[generated.OrganizationAttributesResponse](t, active).Values)
		historical, err := backend.ExecuteOperation(t.Context(), "list_organization_attributes", map[string]any{"organization_id": org.ID, "include_superseded": true})
		require.NoError(t, err)
		assert.Len(t, operationOutput[generated.OrganizationAttributesResponse](t, historical).Values, 1)
		filtered, err := backend.ExecuteOperation(t.Context(), "list_organization_attributes", map[string]any{"organization_id": org.ID, "include_superseded": true, "definition_slug": "nonexistent"})
		require.NoError(t, err)
		assert.Empty(t, operationOutput[generated.OrganizationAttributesResponse](t, filtered).Values)
	})
}

func TestOrganizationMergeThroughNativeRoutes(t *testing.T) {
	st := testutil.NewTestStore(t)
	first, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Survivor", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	second, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Losing", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	read, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": first.ID})
	require.NoError(t, err)
	etag := operationOutput[mcpserver.OrganizationRecord](t, read).ETag
	losingRead, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": second.ID})
	require.NoError(t, err)
	losingETag := operationOutput[mcpserver.OrganizationRecord](t, losingRead).ETag
	updated, err := backend.ExecuteOperation(t.Context(), "update_organization", map[string]any{"organization_id": second.ID, "etag": losingETag, "body": map[string]any{"name": "Updated Losing", "kind": "company"}})
	require.NoError(t, err)
	updatedLosing := operationOutput[mcpserver.OrganizationMutation](t, updated)
	stale, err := backend.ExecuteOperation(t.Context(), "merge_organization", map[string]any{"organization_id": first.ID, "etag": etag, "body": map[string]any{"losing_organization_id": second.ID, "losing_revision": second.Revision}})
	require.NoError(t, err)
	assertLoopRefusal(t, stale, "organization_revision_conflict", false)
	stillFirst, err := st.GetOrganizationContext(t.Context(), first.ID)
	require.NoError(t, err)
	assert.Nil(t, stillFirst.MergedIntoID)
	stillSecond, err := st.GetOrganizationContext(t.Context(), second.ID)
	require.NoError(t, err)
	assert.Nil(t, stillSecond.MergedIntoID)
	merged, err := backend.ExecuteOperation(t.Context(), "merge_organization", map[string]any{"organization_id": first.ID, "etag": etag, "body": map[string]any{"losing_organization_id": second.ID, "losing_revision": updatedLosing.Organization.Revision}})
	require.NoError(t, err)
	survivor := operationOutput[mcpserver.OrganizationMutation](t, merged)
	assert.Greater(t, survivor.Organization.Revision, first.Revision)
	losing, err := st.GetOrganizationContext(t.Context(), second.ID)
	require.NoError(t, err)
	require.NotNil(t, losing.MergedIntoID)
	assert.Equal(t, first.ID, *losing.MergedIntoID)
}

func TestOrganizationSDKRequiresConfirmationAndRejectsUnknownFields(t *testing.T) {
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyOrganizations}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		if approvals == 1 {
			return &sdkmcp.ElicitResult{Action: "decline"}, nil
		}
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
	missing, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_organization", Arguments: map[string]any{"organization_id": 999, "etag": `"organization-999-r1"`, "body": map[string]any{"name": "Missing Example"}}})
	require.NoError(t, err)
	require.True(t, missing.IsError)
	missingData, err := json.Marshal(missing.StructuredContent)
	require.NoError(t, err)
	var refusal map[string]any
	require.NoError(t, json.Unmarshal(missingData, &refusal))
	assert.Equal(t, "organization_not_found", refusal["error"])
	assert.Zero(t, approvals)
	declined, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_organization", Arguments: map[string]any{"body": map[string]any{"name": "Declined Example"}}})
	require.NoError(t, err)
	assert.True(t, declined.IsError)
	organizations, err := st.ListOrganizationsContext(t.Context(), store.OrganizationFilter{})
	require.NoError(t, err)
	assert.Empty(t, organizations)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_organization", Arguments: map[string]any{"body": map[string]any{"name": "Example SDK", "kind": "company"}}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Equal(t, 2, approvals)
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
	data, err = json.Marshal(unsupported.Output)
	require.NoError(t, err)
	assert.Contains(t, string(data), "operation_not_supported")
}

func TestOrganizationWriteFlagForEmbeddedServer(t *testing.T) {
	savedOrganization, savedCurated, savedProfile := mcpAllowOrganizationWrites, mcpAllowCuratedWrites, mcpAllowProfileWrites
	savedLoop, savedSource := mcpAllowLoopWrites, mcpAllowSourceWrites
	savedSensitive, savedDecisions, savedScoring := mcpConfirmSensitiveDrafts, mcpAllowIdentityDecisions, mcpAllowIdentityScoring
	savedMerges, savedKata, savedCardDAV, savedCalendar := mcpAllowPersonMerges, mcpAllowKataWrites, mcpAllowCardDAVWrites, mcpAllowCalendarWrites
	t.Cleanup(func() {
		mcpAllowOrganizationWrites, mcpAllowCuratedWrites, mcpAllowProfileWrites = savedOrganization, savedCurated, savedProfile
		mcpAllowLoopWrites, mcpAllowSourceWrites = savedLoop, savedSource
		mcpConfirmSensitiveDrafts, mcpAllowIdentityDecisions, mcpAllowIdentityScoring = savedSensitive, savedDecisions, savedScoring
		mcpAllowPersonMerges, mcpAllowKataWrites, mcpAllowCardDAVWrites, mcpAllowCalendarWrites = savedMerges, savedKata, savedCardDAV, savedCalendar
	})
	for _, prefix := range []string{"", "mcp-"} {
		cmd := &cobra.Command{}
		addMCPWriteFlags(cmd, prefix)
		var opts mcpserver.ServeOptions
		applyMCPWriteFlags(cmd, prefix, &opts)
		assert.NotContains(t, opts.OperationWriteFamilies, mcpserver.OperationFamilyOrganizations)
		require.NoError(t, cmd.Flags().Set(prefix+"allow-organization-writes", "true"))
		opts = mcpserver.ServeOptions{}
		applyMCPWriteFlags(cmd, prefix, &opts)
		assert.Contains(t, opts.OperationWriteFamilies, mcpserver.OperationFamilyOrganizations)
	}
}
