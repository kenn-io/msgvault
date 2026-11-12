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
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

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
			assertpkg.Equal(t, tc.want, organizationMCPCapabilities(hasRoute, caps))
		})
	}
}

func TestOrganizationReadsUseNativeStore(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	require.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
	require.NoError(err)
	profile := operationOutput[mcpserver.OrganizationRecord](t, result)
	assert.Equal(org.ID, profile.Profile.Organization.ID)
	assert.NotEmpty(profile.ETag)
	result, err = backend.ExecuteOperation(t.Context(), "list_organization_attributes", map[string]any{"organization_id": org.ID})
	require.NoError(err)
	assert.Empty(operationOutput[generated.OrganizationAttributesResponse](t, result).Values)
	result, err = backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": -1})
	require.NoError(err)
	assert.True(result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(err)
	assert.Contains(string(data), "invalid_arguments")
}

func TestOrganizationWritesUseRevisionsAndEmploymentGuard(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	created, err := backend.ExecuteOperation(t.Context(), "create_organization", map[string]any{"body": map[string]any{"name": "Example Company", "kind": "company"}})
	require.NoError(err)
	first := operationOutput[mcpserver.OrganizationMutation](t, created)
	require.NotEmpty(first.ETag)
	id := first.Organization.ID
	patched, err := backend.ExecuteOperation(t.Context(), "update_organization", map[string]any{"organization_id": id, "etag": first.ETag, "body": map[string]any{"name": "Example Group", "kind": "company"}})
	require.NoError(err)
	second := operationOutput[mcpserver.OrganizationMutation](t, patched)
	assert.Equal("Example Group", second.Organization.Name)
	stale, err := backend.ExecuteOperation(t.Context(), "delete_organization", map[string]any{"organization_id": id, "etag": first.ETag})
	require.NoError(err)
	assert.True(stale.IsError)
	staleData, err := json.Marshal(stale.Output)
	require.NoError(err)
	assert.Contains(string(staleData), "organization_revision_conflict")
	participant, err := st.EnsureParticipant("employee@example.com", "Example Employee", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)
	_, err = st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person.ID, OrganizationID: id, Source: store.ProvenanceUser})
	require.NoError(err)
	refused, err := backend.ExecuteOperation(t.Context(), "delete_organization", map[string]any{"organization_id": id, "etag": second.ETag})
	require.NoError(err)
	assert.True(refused.IsError)
	encoded, err := json.Marshal(refused.Output)
	require.NoError(err)
	assert.Contains(string(encoded), "organization_has_employments")
	_, err = st.GetOrganizationContext(t.Context(), id)
	assert.NoError(err)
	deletable, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Deletable", Kind: store.OrganizationKindCompany})
	require.NoError(err)
	readable, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": deletable.ID})
	require.NoError(err)
	deleteETag := operationOutput[mcpserver.OrganizationRecord](t, readable).ETag
	deleted, err := backend.ExecuteOperation(t.Context(), "delete_organization", map[string]any{"organization_id": deletable.ID, "etag": deleteETag})
	require.NoError(err)
	assert.True(operationOutput[mcpserver.OrganizationDeleted](t, deleted).Deleted)
}

func TestOrganizationProfileMediaAndAttributesThroughNativeRoutes(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: "test-org-industry", ObjectType: store.AttributeObjectOrganization, Slug: "industry", Label: "Industry", ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText, Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser, APIMutable: true, UICreatable: true, UIEditable: true, IsDeletable: true})
	require.NoError(err)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	require.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	read, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
	require.NoError(err)
	initial := operationOutput[mcpserver.OrganizationRecord](t, read)
	args := map[string]any{"organization_id": org.ID, "etag": initial.ETag, "body": map[string]any{"names": []any{map[string]any{"name": "Example Alias", "name_kind": "alias", "source": "user"}}, "media": []any{map[string]any{"media_kind": "logo", "source": "user", "data": "YWJjZGVm"}, map[string]any{"media_kind": "photo", "source": "user", "ordinal": 1, "uri": "https://example.com/photo.png"}}}}
	disclosure, err := backend.OperationDisclosure(t.Context(), "replace_organization_profile", args)
	require.NoError(err)
	assert.NotContains(disclosure, "YWJjZGVm")
	assert.Contains(disclosure, "replace all profile collections")
	result, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", args)
	require.NoError(err)
	profile := operationOutput[mcpserver.OrganizationRecord](t, result)
	t.Run("media", func(t *testing.T) {
		assert := assertpkg.New(t)
		require := requirepkg.New(t)
		require.Len(profile.Profile.Media, 2)
		disclosureArgs := map[string]any{"organization_id": org.ID, "etag": profile.ETag, "body": map[string]any{"media": []any{map[string]any{"media_kind": "logo", "source": "user", "data": "YWJjZGVm"}}}}
		disclosure, err := backend.OperationDisclosure(t.Context(), "replace_organization_profile", disclosureArgs)
		require.NoError(err)
		digest := sha256.Sum256([]byte("abcdef"))
		assert.Contains(disclosure, `"bytes":6`)
		assert.Contains(disclosure, fmt.Sprintf(`"sha256":"%x"`, digest))
		assert.NotContains(disclosure, "YWJjZGVm")
		badArgs := map[string]any{"organization_id": org.ID, "etag": profile.ETag, "body": map[string]any{"media": []any{map[string]any{"media_kind": "logo", "source": "user", "data": "!!!"}}}}
		badDisclosure, err := backend.OperationDisclosure(t.Context(), "replace_organization_profile", badArgs)
		require.NoError(err)
		assert.Contains(badDisclosure, "!!!")
		oversized := base64.StdEncoding.EncodeToString(make([]byte, store.MaxPersonMediaBytes+1))
		tooLarge, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", map[string]any{"organization_id": org.ID, "etag": profile.ETag, "body": map[string]any{"media": []any{map[string]any{"media_kind": "logo", "source": "user", "data": oversized}}}})
		require.NoError(err)
		assertLoopRefusal(t, tooLarge, "invalid_organization", false)
		unchanged, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
		require.NoError(err)
		assert.Equal(profile, operationOutput[mcpserver.OrganizationRecord](t, unchanged))
		var mediaID, uriID int64
		for _, media := range profile.Profile.Media {
			if media.HasData {
				mediaID = media.Envelope.ID
			} else {
				uriID = media.Envelope.ID
			}
		}
		require.NotZero(mediaID)
		require.NotZero(uriID)
		uriResult, err := backend.ExecuteOperation(t.Context(), "get_organization_media", map[string]any{"organization_id": org.ID, "media_id": uriID})
		require.NoError(err)
		assert.True(uriResult.IsError)
		uriData, err := json.Marshal(uriResult.Output)
		require.NoError(err)
		assert.Contains(string(uriData), "profile_media_content_unavailable")
		firstResult, err := backend.ExecuteOperation(t.Context(), "get_organization_media", map[string]any{"organization_id": org.ID, "media_id": mediaID, "length": float64(3)})
		require.NoError(err)
		first := operationOutput[mcpserver.PersonMediaContent](t, firstResult)
		assert.Equal("YWJj", first.DataBase64)
		assert.False(first.Complete)
		nextResult, err := backend.ExecuteOperation(t.Context(), "get_organization_media", map[string]any{"organization_id": org.ID, "media_id": mediaID, "offset": float64(3), "sha256": first.SHA256})
		require.NoError(err)
		next := operationOutput[mcpserver.PersonMediaContent](t, nextResult)
		assert.Equal("ZGVm", next.DataBase64)
		assert.True(next.Complete)
		invalid, err := backend.ExecuteOperation(t.Context(), "get_organization_media", map[string]any{"organization_id": org.ID, "media_id": mediaID, "offset": float64(3)})
		require.NoError(err)
		assert.True(invalid.IsError)
		invalidData, err := json.Marshal(invalid.Output)
		require.NoError(err)
		assert.Contains(string(invalidData), "invalid_arguments")
	})
	stale, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", args)
	require.NoError(err)
	assert.True(stale.IsError)
	staleData, err := json.Marshal(stale.Output)
	require.NoError(err)
	assert.Contains(string(staleData), "organization_revision_conflict")
	clearProfile, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", map[string]any{"organization_id": org.ID, "etag": profile.ETag, "body": map[string]any{}})
	require.NoError(err)
	clearedProfile := operationOutput[mcpserver.OrganizationRecord](t, clearProfile)
	assert.Empty(clearedProfile.Profile.Names)
	history, err := backend.ExecuteOperation(t.Context(), "get_organization_history", map[string]any{"organization_id": org.ID})
	require.NoError(err)
	assert.NotEmpty(operationOutput[mcpserver.OrganizationRecord](t, history).Profile.Names)
	t.Run("attributes", func(t *testing.T) {
		assert := assertpkg.New(t)
		require := requirepkg.New(t)
		invalid, err := backend.ExecuteOperation(t.Context(), "clear_organization_attribute", map[string]any{"organization_id": org.ID, "slug": "industry", "ordinal": -1})
		require.NoError(err)
		assertLoopRefusal(t, invalid, "invalid_ordinal", false)
		absentArgs := map[string]any{"organization_id": org.ID, "body": map[string]any{"definition_slug": "industry", "value": map[string]any{"type": "text", "text": "services"}, "source": "user"}}
		absentDisclosure, err := backend.OperationDisclosure(t.Context(), "set_organization_attribute", absentArgs)
		require.NoError(err)
		assert.Contains(absentDisclosure, `"superseded":null`)
		set, err := backend.ExecuteOperation(t.Context(), "set_organization_attribute", map[string]any{"organization_id": org.ID, "body": map[string]any{"definition_slug": "industry", "value": map[string]any{"type": "text", "text": "software"}, "source": "user"}})
		require.NoError(err)
		write := operationOutput[generated.OrganizationAttributeWrite](t, set)
		require.NotNil(write.Value)
		cases := []struct {
			name string
			args map[string]any
		}{
			{"set_organization_attribute", map[string]any{"organization_id": org.ID, "body": map[string]any{"definition_slug": "industry", "value": map[string]any{"type": "text", "text": "services"}, "source": "user", "expected_value_id": write.Value.ID}}},
			{"clear_organization_attribute", map[string]any{"organization_id": org.ID, "slug": "industry", "expected_value_id": write.Value.ID}},
		}
		for _, tc := range cases {
			t.Run(tc.name+" disclosure", func(t *testing.T) {
				assert := assertpkg.New(t)
				require := requirepkg.New(t)
				disclosure, err := backend.OperationDisclosure(t.Context(), tc.name, tc.args)
				require.NoError(err)
				again, err := backend.OperationDisclosure(t.Context(), tc.name, tc.args)
				require.NoError(err)
				assert.Equal(disclosure, again)
				assert.Contains(disclosure, "software")
				assert.NotContains(disclosure, `"active_until"`)
				assert.NotContains(disclosure, `"superseded_at"`)
				var decoded struct {
					Current mcpserver.OrganizationRecord `json:"current"`
					Preview struct {
						Superseded *generated.OrganizationAttributeValue `json:"superseded"`
					} `json:"preview"`
				}
				require.NoError(json.Unmarshal([]byte(disclosure), &decoded))
				assert.Equal(org.ID, decoded.Current.Profile.Organization.ID)
				require.NotNil(decoded.Preview.Superseded)
				assert.Equal(write.Value.ID, decoded.Preview.Superseded.ID)
				missing := make(map[string]any, len(tc.args))
				for key, value := range tc.args {
					missing[key] = value
				}
				missing["organization_id"] = int64(999999)
				prompt, err := backend.OperationDisclosure(t.Context(), tc.name, missing)
				assert.Empty(prompt)
				var refusal *mcpserver.OperationRefusalError
				require.ErrorAs(err, &refusal)
				assert.Equal("organization_not_found", refusal.Code)
			})
		}
		changed, err := backend.ExecuteOperation(t.Context(), "set_organization_attribute", absentArgs)
		require.NoError(err)
		write = operationOutput[generated.OrganizationAttributeWrite](t, changed)
		require.NotNil(write.Value)
		for _, tc := range cases {
			_, err := backend.OperationDisclosure(t.Context(), tc.name, tc.args)
			var refusal *mcpserver.OperationRefusalError
			require.ErrorAs(err, &refusal)
			assert.Equal("attribute_value_conflict", refusal.Code)
			refused, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
			require.NoError(err)
			assertLoopRefusal(t, refused, "attribute_value_conflict", false)
		}
		injected := map[string]any{"organization_id": org.ID, "slug": "industry?dry_run=true"}
		_, err = backend.OperationDisclosure(t.Context(), "clear_organization_attribute", injected)
		var refusal *mcpserver.OperationRefusalError
		require.ErrorAs(err, &refusal)
		assert.Equal("invalid_attribute_slug", refusal.Code)
		refused, err := backend.ExecuteOperation(t.Context(), "clear_organization_attribute", injected)
		require.NoError(err)
		assertLoopRefusal(t, refused, "invalid_attribute_slug", false)
		activeBefore, err := backend.ExecuteOperation(t.Context(), "list_organization_attributes", map[string]any{"organization_id": org.ID})
		require.NoError(err)
		require.Len(operationOutput[generated.OrganizationAttributesResponse](t, activeBefore).Values, 1)
		assert.Equal(write.Value.ID, operationOutput[generated.OrganizationAttributesResponse](t, activeBefore).Values[0].ID)
		padded, err := backend.ExecuteOperation(t.Context(), "clear_organization_attribute", map[string]any{"organization_id": org.ID, "slug": " industry ", "dry_run": true})
		require.NoError(err)
		assert.True(operationOutput[generated.OrganizationAttributeWrite](t, padded).DryRun)
		preview, err := backend.ExecuteOperation(t.Context(), "clear_organization_attribute", map[string]any{"organization_id": org.ID, "slug": "industry", "expected_value_id": write.Value.ID, "dry_run": true})
		require.NoError(err)
		assert.True(operationOutput[generated.OrganizationAttributeWrite](t, preview).DryRun)
		clear, err := backend.ExecuteOperation(t.Context(), "clear_organization_attribute", map[string]any{"organization_id": org.ID, "slug": "industry", "expected_value_id": write.Value.ID})
		require.NoError(err)
		require.NotNil(operationOutput[generated.OrganizationAttributeWrite](t, clear).Superseded)
		active, err := backend.ExecuteOperation(t.Context(), "list_organization_attributes", map[string]any{"organization_id": org.ID})
		require.NoError(err)
		assert.Empty(operationOutput[generated.OrganizationAttributesResponse](t, active).Values)
		historical, err := backend.ExecuteOperation(t.Context(), "list_organization_attributes", map[string]any{"organization_id": org.ID, "include_superseded": true})
		require.NoError(err)
		assert.Len(operationOutput[generated.OrganizationAttributesResponse](t, historical).Values, 2)
		filtered, err := backend.ExecuteOperation(t.Context(), "list_organization_attributes", map[string]any{"organization_id": org.ID, "include_superseded": true, "definition_slug": "nonexistent"})
		require.NoError(err)
		assert.Empty(operationOutput[generated.OrganizationAttributesResponse](t, filtered).Values)
	})
}

func TestOrganizationLargeProfileResponse(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	require.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	read, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
	require.NoError(err)
	etag := operationOutput[mcpserver.OrganizationRecord](t, read).ETag
	names := make([]any, 0, store.MaxOrganizationProfileValues)
	for i := range store.MaxOrganizationProfileValues {
		names = append(names, map[string]any{"name": fmt.Sprintf("Name %03d %s", i, strings.Repeat("a", 6<<10)), "name_kind": "alias", "source": "user"})
	}
	body := map[string]any{"names": names}
	encoded, err := json.Marshal(body)
	require.NoError(err)
	assert.Less(len(encoded), api.MaxOrganizationProfileRequestBytes)
	replaced, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", map[string]any{"organization_id": org.ID, "etag": etag, "body": body})
	require.NoError(err)
	receipt := operationOutput[mcpserver.OrganizationRecord](t, replaced)
	encoded, err = json.Marshal(receipt)
	require.NoError(err)
	assert.Greater(len(encoded), 1<<20)
	read, err = backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": org.ID})
	require.NoError(err)
	assert.Len(operationOutput[mcpserver.OrganizationRecord](t, read).Profile.Names, store.MaxOrganizationProfileValues)
	history, err := backend.ExecuteOperation(t.Context(), "get_organization_history", map[string]any{"organization_id": org.ID})
	require.NoError(err)
	assertLoopRefusal(t, history, "operation_response_limit_exceeded", false)
}

func TestOrganizationMergeThroughNativeRoutes(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Survivor", Kind: store.OrganizationKindCompany})
	require.NoError(err)
	second, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Losing", Kind: store.OrganizationKindCompany})
	require.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	read, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": first.ID})
	require.NoError(err)
	etag := operationOutput[mcpserver.OrganizationRecord](t, read).ETag
	losingRead, err := backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": second.ID})
	require.NoError(err)
	losingETag := operationOutput[mcpserver.OrganizationRecord](t, losingRead).ETag
	updated, err := backend.ExecuteOperation(t.Context(), "update_organization", map[string]any{"organization_id": second.ID, "etag": losingETag, "body": map[string]any{"name": "Updated Losing", "kind": "company"}})
	require.NoError(err)
	updatedLosing := operationOutput[mcpserver.OrganizationMutation](t, updated)
	stale, err := backend.ExecuteOperation(t.Context(), "merge_organization", map[string]any{"organization_id": first.ID, "etag": etag, "body": map[string]any{"losing_organization_id": second.ID, "losing_revision": second.Revision}})
	require.NoError(err)
	assertLoopRefusal(t, stale, "organization_revision_conflict", false)
	stillFirst, err := st.GetOrganizationContext(t.Context(), first.ID)
	require.NoError(err)
	assert.Nil(stillFirst.MergedIntoID)
	stillSecond, err := st.GetOrganizationContext(t.Context(), second.ID)
	require.NoError(err)
	assert.Nil(stillSecond.MergedIntoID)
	merged, err := backend.ExecuteOperation(t.Context(), "merge_organization", map[string]any{"organization_id": first.ID, "etag": etag, "body": map[string]any{"losing_organization_id": second.ID, "losing_revision": updatedLosing.Organization.Revision}})
	require.NoError(err)
	survivor := operationOutput[mcpserver.OrganizationMutation](t, merged)
	assert.Greater(survivor.Organization.Revision, first.Revision)
	losing, err := st.GetOrganizationContext(t.Context(), second.ID)
	require.NoError(err)
	require.NotNil(losing.MergedIntoID)
	assert.Equal(first.ID, *losing.MergedIntoID)
}

func TestOrganizationSDKRequiresConfirmationAndRejectsUnknownFields(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: "test-org-json", ObjectType: store.AttributeObjectOrganization, Slug: "metadata", Label: "Metadata", ValueType: store.AttributeValueJSON, FieldType: store.AttributeFieldJSON, Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser, APIMutable: true, UICreatable: true, UIEditable: true, IsDeletable: true})
	require.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyOrganizations}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		if approvals == 1 {
			return &sdkmcp.ElicitResult{Action: "decline"}, nil
		}
		if approvals == 2 {
			assert.Contains(request.Params.Message, "create an organization")
		}
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	tools, err := session.ListTools(t.Context(), nil)
	require.NoError(err)
	var createFound bool
	for _, tool := range tools.Tools {
		if tool.Name == "create_organization" {
			createFound = true
		}
	}
	require.True(createFound)
	invalid, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_organization", Arguments: map[string]any{"body": map[string]any{"name": "Example", "unexpected": "value"}}})
	require.NoError(err)
	assert.True(invalid.IsError)
	assert.Zero(approvals)
	missing, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_organization", Arguments: map[string]any{"organization_id": 999, "etag": `"organization-999-r1"`, "body": map[string]any{"name": "Missing Example"}}})
	require.NoError(err)
	require.True(missing.IsError)
	missingData, err := json.Marshal(missing.StructuredContent)
	require.NoError(err)
	var refusal map[string]any
	require.NoError(json.Unmarshal(missingData, &refusal))
	assert.Equal("organization_not_found", refusal["error"])
	assert.Zero(approvals)
	declined, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_organization", Arguments: map[string]any{"body": map[string]any{"name": "Declined Example"}}})
	require.NoError(err)
	assert.True(declined.IsError)
	organizations, err := st.ListOrganizationsContext(t.Context(), store.OrganizationFilter{})
	require.NoError(err)
	assert.Empty(organizations)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_organization", Arguments: map[string]any{"body": map[string]any{"name": "Example SDK", "kind": "company"}}})
	require.NoError(err)
	require.False(result.IsError)
	assert.Equal(2, approvals)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(err)
	var created mcpserver.OrganizationMutation
	require.NoError(json.Unmarshal(data, &created))
	stored, err := st.GetOrganizationContext(t.Context(), created.Organization.ID)
	require.NoError(err)
	assert.Equal("Example SDK", stored.Name)
	seeded, err := backend.ExecuteOperation(t.Context(), "replace_organization_profile", map[string]any{"organization_id": created.Organization.ID, "etag": created.ETag, "body": map[string]any{"media": []any{map[string]any{"media_kind": "logo", "source": "user", "data": "YWJj"}}}})
	require.NoError(err)
	profile := operationOutput[mcpserver.OrganizationRecord](t, seeded)
	require.Len(profile.Profile.Media, 1)
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"get_organization", map[string]any{"organization_id": stored.ID}},
		{"get_organization_history", map[string]any{"organization_id": stored.ID}},
		{"list_organization_attributes", map[string]any{"organization_id": stored.ID}},
		{"get_organization_media", map[string]any{"organization_id": stored.ID, "media_id": profile.Profile.Media[0].Envelope.ID}},
	} {
		read, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: call.name, Arguments: call.args})
		require.NoError(err)
		assert.False(read.IsError, call.name)
		assert.NotNil(read.StructuredContent, call.name)
	}
	set, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_organization_attribute", Arguments: map[string]any{"organization_id": stored.ID, "body": map[string]any{"definition_slug": "metadata", "value": map[string]any{"type": "json", "json": map[string]any{"enabled": true}}, "source": "user"}}})
	require.NoError(err)
	assert.False(set.IsError)
	assert.NotNil(set.StructuredContent)
	assert.Equal(3, approvals)
	listed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_organization_attributes", Arguments: map[string]any{"organization_id": stored.ID}})
	require.NoError(err)
	require.False(listed.IsError)
	data, err = json.Marshal(listed.StructuredContent)
	require.NoError(err)
	var attributes generated.OrganizationAttributesResponse
	require.NoError(json.Unmarshal(data, &attributes))
	require.Len(attributes.Values, 1)
	assert.JSONEq(`{"enabled":true}`, string(attributes.Values[0].Value.JSON))
}

func TestOrganizationErrorsHideDetailsAndPreserveUncertainty(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"organization_failed","message":"private backend details"}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	backend := &daemonMCPOperations{client: client, supported: []string{"create_organization"}}
	result, err := backend.ExecuteOperation(t.Context(), "create_organization", map[string]any{"body": map[string]any{"name": "Example"}})
	require.NoError(err)
	require.True(result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(err)
	assert.NotContains(string(data), "private backend details")
	assert.Contains(string(data), `"operation_may_have_completed":true`)
}

func TestOrganizationWriteFlagForEmbeddedServer(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	for _, prefix := range []string{"mcp-"} {
		cmd := &cobra.Command{}
		addMCPWriteFlags(cmd, prefix)
		var opts mcpserver.ServeOptions
		applyMCPWriteFlags(cmd, prefix, &opts)
		assert.NotContains(opts.OperationWriteFamilies, mcpserver.OperationFamilyOrganizations)
		require.NoError(cmd.Flags().Set(prefix+"allow-organization-writes", "true"))
		opts = mcpserver.ServeOptions{}
		applyMCPWriteFlags(cmd, prefix, &opts)
		assert.Contains(opts.OperationWriteFamilies, mcpserver.OperationFamilyOrganizations)
	}
}
