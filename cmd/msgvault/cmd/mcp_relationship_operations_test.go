package cmd

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var relationshipMCPToolNames = []string{
	"list_organizations", "create_organization", "get_organization", "update_organization", "remove_organization", "merge_organizations", "get_organization_history", "update_organization_profile", "list_organization_attributes", "set_organization_attribute", "remove_organization_attribute", "get_organization_record_media",
	"create_employment", "get_employment", "update_employment", "remove_employment", "end_employment", "set_primary_employment", "list_person_employments", "list_organization_employments",
	"list_relationship_types", "create_relationship_type", "get_relationship_type", "update_relationship_type", "remove_relationship_type", "list_person_relationships", "create_person_relationship", "get_person_relationship_record", "update_person_relationship", "remove_person_relationship", "list_person_relationship_reviews", "get_person_network",
}

func TestMCPRelationshipDiscoveryUsesActualOwningRoutes(t *testing.T) {
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t), config: cfg}, Logger: slog.New(slog.DiscardHandler)}))
	names := backend.capabilities()
	for _, name := range relationshipMCPToolNames {
		assertions.True(slices.Contains(names, name), "missing owning tool: %s", name)
	}
}

func TestMCPRelationshipOrganizationEmploymentLifecycleAndNetwork(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, person, _, session, approvals := recordSDKFixture(t)
	created := callRecordTool[struct {
		ETag         string                 `json:"etag"`
		Organization generated.Organization `json:"organization"`
	}](t, session, "create_organization", map[string]any{"organization": map[string]any{"name": "Synthetic Research Group", "kind": "company", "primary_domain": "example.test"}})
	requirements.NotEmpty(created.ETag)
	native, err := st.GetOrganizationContext(t.Context(), created.Organization.ID)
	requirements.NoError(err)
	assertions.Equal("Synthetic Research Group", native.Name)
	employment := callRecordTool[struct {
		ETag       string               `json:"etag"`
		Employment generated.Employment `json:"employment"`
	}](t, session, "create_employment", map[string]any{"employment": map[string]any{"person_id": person.ID, "organization_id": created.Organization.ID, "title": "Synthetic Analyst", "source": "user", "is_current": true, "is_primary": false}})
	assertions.True(employment.Employment.IsCurrent)
	assertions.False(employment.Employment.IsPrimary)
	network := callRecordTool[generated.PersonNetwork](t, session, "get_person_network", map[string]any{"person_id": person.ID, "depth": 1})
	assertions.Equal(person.ID, network.RootPersonID)
	requirements.Len(network.Nodes, 2)
	requirements.Len(network.Edges, 1)
	assertions.Equal(generated.NetworkEdgeKind("employment"), network.Edges[0].Kind)
	primary := callRecordTool[struct {
		ETag       string               `json:"etag"`
		Employment generated.Employment `json:"employment"`
	}](t, session, "set_primary_employment", map[string]any{"employment_id": employment.Employment.ID, "etag": employment.ETag})
	assertions.True(primary.Employment.IsPrimary)
	count := *approvals
	stale, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "end_employment", Arguments: map[string]any{"employment_id": employment.Employment.ID, "etag": employment.ETag, "end_date": "2026-09"}})
	requirements.NoError(err)
	assertions.True(stale.IsError)
	assertions.Equal(count, *approvals, "original employment ETag stays stale")
	currentOrg := callRecordTool[struct {
		ETag    string                        `json:"etag"`
		Profile generated.OrganizationProfile `json:"profile"`
	}](t, session, "get_organization", map[string]any{"organization_id": created.Organization.ID})
	rejected, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "remove_organization", Arguments: map[string]any{"organization_id": created.Organization.ID, "etag": currentOrg.ETag}})
	requirements.NoError(err)
	assertions.True(rejected.IsError)
	assertions.Contains(settingsMCPDiagnostic(rejected), "organization_has_employments")
	_, err = st.GetOrganizationContext(t.Context(), created.Organization.ID)
	requirements.NoError(err)
	ended := callRecordTool[struct {
		ETag       string               `json:"etag"`
		Employment generated.Employment `json:"employment"`
	}](t, session, "end_employment", map[string]any{"employment_id": primary.Employment.ID, "etag": primary.ETag, "end_date": "2026-09"})
	assertions.False(ended.Employment.IsCurrent)
	assertions.False(ended.Employment.IsPrimary)
	network = callRecordTool[generated.PersonNetwork](t, session, "get_person_network", map[string]any{"person_id": person.ID, "depth": 1})
	assertions.Empty(network.Edges)
	network = callRecordTool[generated.PersonNetwork](t, session, "get_person_network", map[string]any{"person_id": person.ID, "depth": 1, "include_ended": true})
	requirements.Len(network.Edges, 1)
	for _, depth := range []int{0, 4} {
		refused, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_network", Arguments: map[string]any{"person_id": person.ID, "depth": depth}})
		assertions.True(err != nil || refused != nil && refused.IsError, "owning depth must remain bounded")
	}
}

func TestMCPRelationshipDeclaredEdgesDirectionsAndNullableNotes(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, person, _, session, approvals := recordSDKFixture(t)
	participant, err := st.EnsureParticipant("related@example.test", "Synthetic Related Person", "example.test")
	requirements.NoError(err)
	other, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	typ := callRecordTool[mcpserver.RelationshipTypeRecord](t, session, "create_relationship_type", map[string]any{"relationship_type": map[string]any{"slug": "synthetic-adviser", "forward_label": "adviser", "reverse_label": "advisee", "is_symmetric": false, "description": "Synthetic type description"}})
	changed := callRecordTool[mcpserver.RelationshipTypeRecord](t, session, "update_relationship_type", map[string]any{"relationship_type_id": typ.RelationshipType.ID, "etag": typ.ETag, "changes": map[string]any{"description": ""}})
	assertions.Nil(changed.RelationshipType.Description, "explicit empty string retains native clear")
	edge := callRecordTool[mcpserver.PersonRelationshipRecord](t, session, "create_person_relationship", map[string]any{"relationship": map[string]any{"source_person_id": person.ID, "target_person_id": other.ID, "relationship_type_slug": typ.RelationshipType.Slug, "start_date": "2026-08", "notes": "Synthetic private relationship note"}})
	requirements.NotNil(edge.Relationship.Notes)
	read := callRecordTool[mcpserver.PersonRelationshipRecord](t, session, "get_person_relationship_record", map[string]any{"relationship_id": edge.Relationship.ID})
	assertions.Nil(read.Relationship.Notes)
	read = callRecordTool[mcpserver.PersonRelationshipRecord](t, session, "get_person_relationship_record", map[string]any{"relationship_id": edge.Relationship.ID, "include_sensitive": true})
	requirements.NotNil(read.Relationship.Notes)
	assertions.Equal("Synthetic private relationship note", *read.Relationship.Notes)
	forward := callRecordTool[generated.PersonRelationshipsResponse](t, session, "list_person_relationships", map[string]any{"person_id": person.ID})
	reverse := callRecordTool[generated.PersonRelationshipsResponse](t, session, "list_person_relationships", map[string]any{"person_id": other.ID})
	requirements.Len(forward.Relationships, 1)
	requirements.Len(reverse.Relationships, 1)
	assertions.Equal("outgoing", forward.Relationships[0].Direction)
	assertions.Equal("incoming", reverse.Relationships[0].Direction)
	assertions.Equal("advisee", forward.Relationships[0].CounterpartLabel)
	assertions.Equal("adviser", reverse.Relationships[0].CounterpartLabel)
	assertions.Nil(reverse.Relationships[0].Relationship.Notes)
	inUse, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "remove_relationship_type", Arguments: map[string]any{"relationship_type_id": changed.RelationshipType.ID, "etag": changed.ETag}})
	requirements.NoError(err)
	assertions.True(inUse.IsError)
	assertions.Contains(settingsMCPDiagnostic(inUse), "relationship_type_in_use")
	cleared := callRecordTool[mcpserver.PersonRelationshipRecord](t, session, "update_person_relationship", map[string]any{"relationship_id": edge.Relationship.ID, "etag": edge.ETag, "changes": map[string]any{"notes": nil}})
	assertions.Nil(cleared.Relationship.Notes)
	native, err := st.GetPersonRelationshipContext(t.Context(), edge.Relationship.ID)
	requirements.NoError(err)
	assertions.Nil(native.Notes)
	count := *approvals
	stale, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_person_relationship", Arguments: map[string]any{"relationship_id": edge.Relationship.ID, "etag": edge.ETag, "changes": map[string]any{"end_date": "2026-09"}}})
	requirements.NoError(err)
	assertions.True(stale.IsError)
	assertions.Equal(count, *approvals)
	ended := callRecordTool[mcpserver.PersonRelationshipRecord](t, session, "update_person_relationship", map[string]any{"relationship_id": edge.Relationship.ID, "etag": cleared.ETag, "changes": map[string]any{"end_date": "2026-09"}})
	requirements.NotNil(ended.Relationship.EndDate)
	assertions.Empty(callRecordTool[generated.PersonRelationshipsResponse](t, session, "list_person_relationships", map[string]any{"person_id": person.ID}).Relationships)
	assertions.Len(callRecordTool[generated.PersonRelationshipsResponse](t, session, "list_person_relationships", map[string]any{"person_id": person.ID, "include_ended": true}).Relationships, 1)
	removed := callRecordTool[struct {
		Removed bool `json:"removed"`
	}](t, session, "remove_person_relationship", map[string]any{"relationship_id": edge.Relationship.ID, "etag": ended.ETag})
	assertions.True(removed.Removed)
	_, err = st.GetPersonRelationshipContext(t.Context(), edge.Relationship.ID)
	requirements.ErrorIs(err, store.ErrPersonRelationshipNotFound)
	removed = callRecordTool[struct {
		Removed bool `json:"removed"`
	}](t, session, "remove_relationship_type", map[string]any{"relationship_type_id": changed.RelationshipType.ID, "etag": changed.ETag})
	assertions.True(removed.Removed)
	seeded := callRecordTool[generated.RelationshipTypesResponse](t, session, "list_relationship_types", map[string]any{})
	requirements.NotEmpty(seeded.RelationshipTypes)
	system := callRecordTool[mcpserver.RelationshipTypeRecord](t, session, "get_relationship_type", map[string]any{"relationship_type_id": seeded.RelationshipTypes[0].ID})
	rejected, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "remove_relationship_type", Arguments: map[string]any{"relationship_type_id": system.RelationshipType.ID, "etag": system.ETag}})
	requirements.NoError(err)
	assertions.True(rejected.IsError)
	assertions.Contains(settingsMCPDiagnostic(rejected), "relationship_type_not_deletable")
}

func TestMCPRelationshipOrganizationNativeReplacementAndMergeGuards(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, _, _, session, approvals := recordSDKFixture(t)
	org := callRecordTool[mcpserver.OrganizationRecord](t, session, "create_organization", map[string]any{"organization": map[string]any{"name": "Synthetic Organization", "kind": "company", "primary_domain": nil, "description": nil}})
	assertions.Nil(org.Organization.PrimaryDomain)
	profile := callRecordTool[mcpserver.OrganizationProfileRecord](t, session, "update_organization_profile", map[string]any{"organization_id": org.Organization.ID, "etag": org.ETag, "profile": map[string]any{
		"names":       []any{map[string]any{"name_kind": "alias", "name": "Synthetic Alias", "source": "user", "ordinal": 0, "vcard_group": nil}},
		"identifiers": []any{map[string]any{"identifier_kind": "domain", "identifier_value": "example.test", "source": "user"}, map[string]any{"identifier_kind": "tax_id", "identifier_value": "SYNTHETIC-TAX-1", "source": "user"}},
	}})
	requirements.NotNil(profile.Profile.Names)
	requirements.Len(*profile.Profile.Names, 1)
	assertions.Equal(org.Organization.Revision+1, profile.Profile.Organization.Revision)
	ordinary := callRecordTool[mcpserver.OrganizationProfileRecord](t, session, "get_organization", map[string]any{"organization_id": org.Organization.ID})
	assertions.Nil(ordinary.Profile.Identifiers)
	selected := callRecordTool[mcpserver.OrganizationProfileRecord](t, session, "get_organization", map[string]any{"organization_id": org.Organization.ID, "fields": []any{"identifiers"}})
	requirements.NotNil(selected.Profile.Identifiers)
	assertions.Len(*selected.Profile.Identifiers, 1)
	sensitive := callRecordTool[mcpserver.OrganizationProfileRecord](t, session, "get_organization", map[string]any{"organization_id": org.Organization.ID, "fields": []any{"identifiers"}, "include_sensitive": true})
	requirements.NotNil(sensitive.Profile.Identifiers)
	assertions.Len(*sensitive.Profile.Identifiers, 2)
	cleared := callRecordTool[mcpserver.OrganizationProfileRecord](t, session, "update_organization_profile", map[string]any{"organization_id": org.Organization.ID, "etag": profile.ETag, "profile": map[string]any{}})
	assertions.Empty(*cleared.Profile.Names)
	history := callRecordTool[mcpserver.OrganizationProfileRecord](t, session, "get_organization_history", map[string]any{"organization_id": org.Organization.ID, "fields": []any{"names"}})
	requirements.NotNil(history.Profile.Names)
	assertions.Len(*history.Profile.Names, 1)
	losing := callRecordTool[mcpserver.OrganizationRecord](t, session, "create_organization", map[string]any{"organization": map[string]any{"name": "Synthetic Losing Organization", "kind": "other"}})
	changed := callRecordTool[mcpserver.OrganizationRecord](t, session, "update_organization", map[string]any{"organization_id": losing.Organization.ID, "etag": losing.ETag, "organization": map[string]any{"name": "Synthetic Losing Updated", "kind": "other", "retired": false}})
	count := *approvals
	stale, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "merge_organizations", Arguments: map[string]any{"organization_id": org.Organization.ID, "etag": cleared.ETag, "merge": map[string]any{"losing_organization_id": losing.Organization.ID, "losing_revision": losing.Organization.Revision}}})
	requirements.NoError(err)
	assertions.True(stale.IsError)
	assertions.Equal(count, *approvals)
	merged := callRecordTool[mcpserver.OrganizationRecord](t, session, "merge_organizations", map[string]any{"organization_id": org.Organization.ID, "etag": cleared.ETag, "merge": map[string]any{"losing_organization_id": losing.Organization.ID, "losing_revision": changed.Organization.Revision}})
	assertions.Equal(org.Organization.ID, merged.Organization.ID)
	native, err := st.GetOrganizationContext(t.Context(), losing.Organization.ID)
	requirements.NoError(err)
	requirements.NotNil(native.MergedIntoID)
	assertions.Equal(org.Organization.ID, *native.MergedIntoID)
	listing := callRecordTool[generated.OrganizationsResponse](t, session, "list_organizations", map[string]any{"q": "Synthetic Organization", "limit": 1, "offset": 0})
	requirements.Len(listing.Organizations, 1)
	assertions.Equal(org.Organization.ID, listing.Organizations[0].ID)
	rejected, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "remove_organization", Arguments: map[string]any{"organization_id": org.Organization.ID, "etag": merged.ETag}})
	requirements.NoError(err)
	assertions.True(rejected.IsError)
	assertions.Contains(settingsMCPDiagnostic(rejected), "invalid_organization", "merge redirects preserve history")
	disposable := callRecordTool[mcpserver.OrganizationRecord](t, session, "create_organization", map[string]any{"organization": map[string]any{"name": "Synthetic Disposable Organization", "kind": "other"}})
	removed := callRecordTool[struct {
		Removed bool `json:"removed"`
	}](t, session, "remove_organization", map[string]any{"organization_id": disposable.Organization.ID, "etag": disposable.ETag})
	assertions.True(removed.Removed)
}

func TestMCPRelationshipImportedReviewRemainsPendingAndSelective(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, person, _, session, _ := recordSDKFixture(t)
	staged, err := st.ResolveRelatedValueContext(t.Context(), store.RelatedImport{PersonID: person.ID, RawValue: "Synthetic private RELATED text", RawType: "synthetic_unmapped_type", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceCardDAVImport, SourceResourceUID: new("synthetic-resource"), VCardIdentity: store.VCardIdentity{Property: "RELATED", Group: new("synthetic-group")}, Actor: "system"})
	requirements.NoError(err)
	requirements.NotNil(staged.Review)
	ordinary, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_relationship_reviews", Arguments: map[string]any{"status": "pending", "person_id": person.ID}})
	requirements.NoError(err)
	requirements.False(ordinary.IsError, settingsMCPDiagnostic(ordinary))
	data, err := json.Marshal(ordinary.StructuredContent)
	requirements.NoError(err)
	assertions.NotContains(string(data), "Synthetic private RELATED text")
	assertions.NotContains(string(data), "raw_related_value")
	assertions.Contains(string(data), "synthetic-resource")
	private := callRecordTool[mcpserver.RelationshipReviewSelections](t, session, "list_person_relationship_reviews", map[string]any{"status": "pending", "person_id": person.ID, "include_sensitive": true})
	requirements.Len(private.Reviews, 1)
	requirements.NotNil(private.Reviews[0].RawRelatedValue)
	assertions.Equal("Synthetic private RELATED text", *private.Reviews[0].RawRelatedValue)
	native, err := st.ListRelationshipReviewsContext(t.Context(), store.RelationshipReviewListOptions{PersonID: person.ID})
	requirements.NoError(err)
	requirements.Len(native, 1)
	assertions.Equal(store.RelationshipReviewPending, native[0].Status)
	assertions.Nil(native[0].AcceptedRelationshipID)
	empty := callRecordTool[mcpserver.RelationshipReviewSelections](t, session, "list_person_relationship_reviews", map[string]any{"status": "rejected"})
	assertions.Empty(empty.Reviews)
}

func TestMCPRelationshipOrganizationAttributesAndStoredMedia(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, _, _, session, approvals := recordSDKFixture(t)
	org := callRecordTool[mcpserver.OrganizationRecord](t, session, "create_organization", map[string]any{"organization": map[string]any{"name": "Synthetic Attribute Organization", "kind": "company"}})
	definition := callRecordTool[mcpserver.AttributeDefinitionRecord](t, session, "create_attribute_definition", map[string]any{"definition": map[string]any{"object_type": "organization", "slug": "synthetic_private_toggle", "label": "Synthetic private toggle", "value_type": "boolean", "field_type": "checkbox", "is_sensitive": true}})
	preview := callRecordTool[generated.OrganizationAttributeWrite](t, session, "set_organization_attribute", map[string]any{"organization_id": org.Organization.ID, "attribute": map[string]any{"definition_slug": definition.Definition.Slug, "value": map[string]any{"type": "boolean", "boolean": false}, "ordinal": 0, "source": "user", "dry_run": true}})
	assertions.True(preview.DryRun)
	ordinary := callRecordTool[generated.OrganizationAttributesResponse](t, session, "list_organization_attributes", map[string]any{"organization_id": org.Organization.ID})
	assertions.Empty(ordinary.Values)
	written := callRecordTool[generated.OrganizationAttributeWrite](t, session, "set_organization_attribute", map[string]any{"organization_id": org.Organization.ID, "attribute": map[string]any{"definition_slug": definition.Definition.Slug, "value": map[string]any{"type": "boolean", "boolean": false}, "ordinal": 0, "source": "user"}})
	requirements.NotNil(written.Value)
	requirements.NotNil(written.Value.Value.Boolean)
	assertions.False(*written.Value.Value.Boolean)
	ordinary = callRecordTool[generated.OrganizationAttributesResponse](t, session, "list_organization_attributes", map[string]any{"organization_id": org.Organization.ID, "include_sensitive": true})
	assertions.Empty(ordinary.Values, "flag alone does not select private slug")
	selected := callRecordTool[generated.OrganizationAttributesResponse](t, session, "list_organization_attributes", map[string]any{"organization_id": org.Organization.ID, "fields": []any{definition.Definition.Slug}, "include_sensitive": true})
	requirements.Len(selected.Values, 1)
	next := callRecordTool[generated.OrganizationAttributeWrite](t, session, "set_organization_attribute", map[string]any{"organization_id": org.Organization.ID, "attribute": map[string]any{"definition_slug": definition.Definition.Slug, "value": map[string]any{"type": "boolean", "boolean": true}, "source": "user", "expected_value_id": written.Value.ID}})
	requirements.NotNil(next.Value)
	refused, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "remove_organization_attribute", Arguments: map[string]any{"organization_id": org.Organization.ID, "slug": definition.Definition.Slug, "expected_value_id": written.Value.ID}})
	requirements.NoError(err)
	assertions.True(refused.IsError)
	assertions.Contains(settingsMCPDiagnostic(refused), "attribute_value_conflict")
	removed := callRecordTool[generated.OrganizationAttributeWrite](t, session, "remove_organization_attribute", map[string]any{"organization_id": org.Organization.ID, "slug": definition.Definition.Slug, "expected_value_id": next.Value.ID})
	assertions.False(removed.DryRun)
	historical := callRecordTool[generated.OrganizationAttributesResponse](t, session, "list_organization_attributes", map[string]any{"organization_id": org.Organization.ID, "definition_slug": definition.Definition.Slug, "include_superseded": true, "fields": []any{definition.Definition.Slug}, "include_sensitive": true})
	assertions.Len(historical.Values, 2)
	count := *approvals
	unsafe, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_organization_attribute", Arguments: map[string]any{"organization_id": org.Organization.ID, "attribute": map[string]any{"definition_slug": definition.Definition.Slug, "value": map[string]any{"type": "integer", "integer": int64(9007199254740993)}, "source": "user"}}})
	requirements.NoError(err)
	assertions.True(unsafe.IsError)
	assertions.Equal(count, *approvals)

	requests := atomic.Int32{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusOK) }))
	defer remote.Close()
	current := callRecordTool[mcpserver.OrganizationProfileRecord](t, session, "get_organization", map[string]any{"organization_id": org.Organization.ID})
	media := callRecordTool[mcpserver.OrganizationProfileRecord](t, session, "update_organization_profile", map[string]any{"organization_id": org.Organization.ID, "etag": current.ETag, "profile": map[string]any{"media": []any{
		map[string]any{"media_kind": "logo", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("synthetic inline media")), "source": "user"},
		map[string]any{"media_kind": "photo", "uri": remote.URL, "source": "user"},
	}}})
	requirements.NotNil(media.Profile.Media)
	requirements.Len(*media.Profile.Media, 2)
	inline := (*media.Profile.Media)[0]
	uri := (*media.Profile.Media)[1]
	blob := callRecordTool[mcpserver.OrganizationRecordMedia](t, session, "get_organization_record_media", map[string]any{"organization_id": org.Organization.ID, "media_id": inline.Envelope.ID})
	assertions.Equal([]byte("synthetic inline media"), blob.Data)
	assertions.Equal("untrusted_data", blob.ContentTrust)
	unavailable, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_organization_record_media", Arguments: map[string]any{"organization_id": org.Organization.ID, "media_id": uri.Envelope.ID}})
	requirements.NoError(err)
	assertions.True(unavailable.IsError)
	assertions.Contains(settingsMCPDiagnostic(unavailable), "profile_media_content_unavailable")
	assertions.Equal(int32(0), requests.Load())
	native, err := st.GetOrganizationProfileContext(t.Context(), org.Organization.ID, false)
	requirements.NoError(err)
	requirements.Len(native.Media, 2)
	other := callRecordTool[mcpserver.OrganizationRecord](t, session, "create_organization", map[string]any{"organization": map[string]any{"name": "Synthetic Other Organization", "kind": "other"}})
	wrong, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_organization_record_media", Arguments: map[string]any{"organization_id": other.Organization.ID, "media_id": inline.Envelope.ID}})
	requirements.NoError(err)
	assertions.True(wrong.IsError)
	assertions.Contains(settingsMCPDiagnostic(wrong), "profile_media_not_found")
}

func TestMCPRelationshipDiscoveryOmitsIncompleteAndDelegatedOwners(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t), config: cfg}, Logger: slog.New(slog.DiscardHandler)}))
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for _, route := range mcpRelationshipRoutes {
		changed := *capabilities
		changed.Routes = slices.DeleteFunc(slices.Clone(capabilities.Routes), func(value apiprotocol.MCPRouteDescriptor) bool { return value.OperationID == route.id })
		assertions.NotContains(newDaemonMCPOperations(backend.client, &changed).capabilities(), route.name)
		checks := []struct {
			id                string
			query, properties []string
		}{{route.id, route.query, route.properties}, {route.contextID, route.contextQuery, nil}}
		for _, check := range checks {
			for _, query := range check.query {
				changed.Routes = slices.Clone(capabilities.Routes)
				for i := range changed.Routes {
					if changed.Routes[i].OperationID == check.id {
						changed.Routes[i].QueryParameters = slices.DeleteFunc(slices.Clone(changed.Routes[i].QueryParameters), func(value string) bool { return value == query })
					}
				}
				assertions.NotContains(newDaemonMCPOperations(backend.client, &changed).capabilities(), route.name, "missing query %s", query)
			}
			for _, property := range check.properties {
				changed.Routes = slices.Clone(capabilities.Routes)
				for i := range changed.Routes {
					if changed.Routes[i].OperationID == check.id {
						changed.Routes[i].RequestProperties = slices.DeleteFunc(slices.Clone(changed.Routes[i].RequestProperties), func(value string) bool { return value == property })
					}
				}
				assertions.NotContains(newDaemonMCPOperations(backend.client, &changed).capabilities(), route.name, "missing native property %s", property)
			}
		}
		if route.contextID != "" {
			changed.Routes = slices.DeleteFunc(slices.Clone(capabilities.Routes), func(value apiprotocol.MCPRouteDescriptor) bool { return value.OperationID == route.contextID })
			assertions.NotContains(newDaemonMCPOperations(backend.client, &changed).capabilities(), route.name)
		}
	}
	delegated := *capabilities
	delegated.Delegated = true
	names := newDaemonMCPOperations(backend.client, &delegated).capabilities()
	for _, name := range relationshipMCPToolNames {
		assertions.NotContains(names, name)
	}
}

func TestMCPRelationshipEmploymentReplacementDeletionAndNetworkTruncation(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, person, _, session, _ := recordSDKFixture(t)
	org := callRecordTool[mcpserver.OrganizationRecord](t, session, "create_organization", map[string]any{"organization": map[string]any{"name": "Synthetic Employment Organization", "kind": "company"}})
	employment := callRecordTool[mcpserver.EmploymentRecord](t, session, "create_employment", map[string]any{"employment": map[string]any{"person_id": person.ID, "organization_id": org.Organization.ID, "source": "user", "title": "Synthetic Initial", "is_current": true, "is_primary": true}})
	changed := callRecordTool[mcpserver.EmploymentRecord](t, session, "update_employment", map[string]any{"employment_id": employment.Employment.ID, "etag": employment.ETag, "employment": map[string]any{"person_id": person.ID, "organization_id": org.Organization.ID, "source": "user", "is_current": false, "is_primary": false, "title": nil, "end_date": "2026-09"}})
	assertions.False(changed.Employment.IsCurrent)
	assertions.False(changed.Employment.IsPrimary)
	assertions.Nil(changed.Employment.Title)
	history := callRecordTool[generated.EmploymentsResponse](t, session, "list_person_employments", map[string]any{"person_id": person.ID, "limit": 1, "offset": 0})
	requirements.Len(history.Employments, 1)
	assertions.Equal(employment.Employment.ID, history.Employments[0].ID)
	current := callRecordTool[generated.EmploymentsResponse](t, session, "list_organization_employments", map[string]any{"organization_id": org.Organization.ID, "current_only": true})
	assertions.Empty(current.Employments)
	removed := callRecordTool[struct {
		Removed bool `json:"removed"`
	}](t, session, "remove_employment", map[string]any{"employment_id": employment.Employment.ID, "etag": changed.ETag})
	assertions.True(removed.Removed)
	_, err := st.GetEmploymentContext(t.Context(), employment.Employment.ID)
	requirements.ErrorIs(err, store.ErrEmploymentNotFound)
	for index := range 251 {
		participant, err := st.EnsureParticipant(fmt.Sprintf("network-%d@example.test", index), fmt.Sprintf("Synthetic Network %03d", index), "example.test")
		requirements.NoError(err)
		peer, _, err := st.CreatePersonFromParticipant(participant)
		requirements.NoError(err)
		_, err = st.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{SourcePersonID: person.ID, TargetPersonID: peer.ID, TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "user"})
		requirements.NoError(err)
	}
	graph := callRecordTool[generated.PersonNetwork](t, session, "get_person_network", map[string]any{"person_id": person.ID, "depth": 1})
	assertions.True(graph.Truncated)
	assertions.Len(graph.Nodes, 250)
	assertions.LessOrEqual(len(graph.Edges), 500)
	for _, node := range graph.Nodes {
		assertions.LessOrEqual(node.Hop, int64(1))
	}
}

func TestMCPRelationshipRefusesNonNullableMediaTextBeforeApproval(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, _, _, session, approvals := recordSDKFixture(t)
	org := callRecordTool[mcpserver.OrganizationRecord](t, session, "create_organization", map[string]any{"organization": map[string]any{"name": "Synthetic Nonnullable Media", "kind": "other"}})
	count := *approvals
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_organization_profile", Arguments: map[string]any{"organization_id": org.Organization.ID, "etag": org.ETag, "profile": map[string]any{"media": []any{map[string]any{"media_kind": "logo", "uri": "https://example.test/synthetic-logo", "source": "user", "original_value": nil}}}}})
	assertions.True(err != nil || called != nil && called.IsError, "native media original_value is nonnullable; omit to use its default")
	assertions.Equal(count, *approvals)
	profile, err := st.GetOrganizationProfileContext(t.Context(), org.Organization.ID, false)
	requirements.NoError(err)
	assertions.Empty(profile.Media)
	assertions.Equal(org.Organization.Revision, profile.Organization.Revision)
}
