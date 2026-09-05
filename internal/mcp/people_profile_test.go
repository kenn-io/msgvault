package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
)

// profileReadingPeopleBackend adds the optional ProfileReader surface to the
// recording fake so the tool can be exercised without a daemon.
type profileReadingPeopleBackend struct {
	recordingPeopleBackend

	profilePersonID int64
	profile         *peoplebrowser.PersonProfile
	profileErr      error
}

func (b *profileReadingPeopleBackend) GetPersonProfile(_ context.Context, personID int64) (*peoplebrowser.PersonProfile, error) {
	b.profilePersonID = personID
	return b.profile, b.profileErr
}

func TestMCPGetPersonProfileListedOnlyWithPeopleAndReadOnly(t *testing.T) {
	assert := assert.New(t)
	withoutPeople := toolsByName(t, rawListTools(t, ServeOptions{Engine: &querytest.MockEngine{}}, true))
	assert.NotContains(withoutPeople, ToolGetPersonProfile)

	listed := toolsByName(t, rawListTools(t, peopleToolOptions(&profileReadingPeopleBackend{}), false))
	require.Contains(t, listed, ToolGetPersonProfile)
	assert.Equal([]string{"person_id"}, toolPropertyNames(t, listed[ToolGetPersonProfile]))
	assert.Equal(true, toolReadOnlyHint(t, listed[ToolGetPersonProfile]))
}

func TestMCPGetPersonProfileReturnsOverviewAndExcludesSensitiveData(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	lastInbound := now.Add(-2 * time.Hour)
	displayName := "Test Person"
	primaryChannel, religion, notes, location := "chat", "must not escape", "private notes", "Test City"
	tracked := true
	backend := &profileReadingPeopleBackend{profile: &peoplebrowser.PersonProfile{
		Person:  store.Person{ID: 7, VCardUID: "person-7", DisplayName: &displayName, Revision: 3, ParticipantIDs: []int64{11}},
		Tracked: &tracked,
		ContactState: &store.ContactState{
			PersonID: 7, LastContactAt: &now, LastContactChannel: store.ChannelChat,
			LastInboundAt: &lastInbound, InteractionCount: 42, InferredChannel: store.ChannelChat,
			CadenceStatus: "ok", ComputedAt: now,
		},
		Attributes: []peoplebrowser.AttributeGroup{
			{
				Definition: store.AttributeDefinition{UniversalID: store.AttributeUniversalIDPrimaryChannel, Slug: store.AttributeSlugPrimaryChannel, Label: "Primary channel", ValueType: store.AttributeValueText, Cardinality: store.AttributeCardinalitySingle},
				Current:    []store.PersonAttributeValue{{ID: 70, PersonID: 7, DefinitionSlug: store.AttributeSlugPrimaryChannel, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &primaryChannel}, Source: store.ProvenanceUser, CreatedAt: now}},
			},
			{
				Definition: store.AttributeDefinition{UniversalID: store.AttributeUniversalIDLocation, Slug: store.AttributeSlugLocation, Label: "Location", ValueType: store.AttributeValueText, Cardinality: store.AttributeCardinalitySingle},
				Current:    []store.PersonAttributeValue{{ID: 71, PersonID: 7, DefinitionSlug: store.AttributeSlugLocation, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &location}, Source: store.ProvenanceUser, CreatedAt: now}},
			},
			{
				Definition: store.AttributeDefinition{UniversalID: store.AttributeUniversalIDReligion, Slug: store.AttributeSlugReligion, Label: "Religion", IsSensitive: true},
				Current:    []store.PersonAttributeValue{{ID: 72, PersonID: 7, DefinitionSlug: store.AttributeSlugReligion, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &religion}}},
			},
			{
				Definition: store.AttributeDefinition{UniversalID: store.AttributeUniversalIDNotes, Slug: store.AttributeSlugNotes, Label: "Notes"},
				Current:    []store.PersonAttributeValue{{ID: 73, PersonID: 7, DefinitionSlug: store.AttributeSlugNotes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &notes}}},
			},
			{Definition: store.AttributeDefinition{UniversalID: "user-field", Slug: "nickname", Label: "Nickname", ValueType: store.AttributeValueText, Cardinality: store.AttributeCardinalitySingle}},
		},
		Employments: []peoplebrowser.PersonEmployment{{
			EmploymentID: 5, OrganizationID: 9, OrganizationName: "Example Org", Title: "Engineer",
			StartDate: "2024-03", IsCurrent: true, IsPrimary: true, Source: store.ProvenanceUser,
		}},
		Relationships: []peoplebrowser.PersonRelationshipSummary{{
			RelationshipID: 3, TypeSlug: "partner", CounterpartLabel: "partner", CounterpartPersonID: 8,
			CounterpartDisplayName: "Other Person", Direction: "source", Status: "active", Source: store.ProvenanceUser,
		}},
		ContactPoints: []peoplebrowser.PersonContactPointSummary{{
			Kind: "email", Value: "alice@example.com", TypeLabel: "work", Preferred: true, Source: store.ProvenanceVCardImport,
		}},
		Dates:      []peoplebrowser.PersonDateSummary{{Kind: "birthday", Date: "--04-12", Source: store.ProvenanceUser}},
		Categories: []string{"people"},
	}}

	result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonProfile, map[string]any{"person_id": 7})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.Equal(int64(7), backend.profilePersonID)
	structured := toolStructuredContent(t, result)
	assert.Equal("Test Person", structured["display_name"])
	assert.Equal("person-7", structured["vcard_uid"])
	assert.Equal(true, structured["tracked"])
	assert.Equal("chat", structured["primary_channel"])
	assert.Equal("chat", structured["inferred_channel"])

	contactState, ok := structured["contact_state"].(map[string]any)
	require.True(ok, "contact_state: %#v", structured["contact_state"])
	assert.Equal("2026-08-20T12:30:00Z", contactState["last_contact_at"])
	assert.Equal("2026-08-20T10:30:00Z", contactState["last_inbound_at"])
	assert.InDelta(float64(42), contactState["interaction_count"], 0)
	assert.NotContains(contactState, "last_outbound_at")

	attributes, ok := structured["attributes"].([]any)
	require.True(ok)
	slugs := make([]string, 0, len(attributes))
	for _, raw := range attributes {
		group, ok := raw.(map[string]any)
		require.True(ok)
		slug, _ := group["slug"].(string)
		slugs = append(slugs, slug)
		if slug == "nickname" {
			assert.Equal([]any{}, group["current"], "empty groups keep an empty current list")
		}
	}
	assert.Equal([]string{store.AttributeSlugPrimaryChannel, store.AttributeSlugLocation, "nickname"}, slugs)

	encoded, err := json.Marshal(structured)
	require.NoError(err)
	assert.NotContains(string(encoded), religion, "sensitive attribute values never leave the daemon boundary")
	assert.NotContains(string(encoded), notes, "private Notes stay behind get_person_notes")
	assert.Contains(string(encoded), location)

	employment := firstStructuredRow(t, structured, "employments")
	assert.Equal("Example Org", employment["organization_name"])
	assert.Equal("Engineer", employment["title"])
	relationship := firstStructuredRow(t, structured, "relationships")
	assert.Equal("partner", relationship["counterpart_label"])
	assert.InDelta(float64(8), relationship["counterpart_person_id"], 0)
	contactPoint := firstStructuredRow(t, structured, "contact_points")
	assert.Equal("alice@example.com", contactPoint["value"])
	assert.Equal(true, contactPoint["preferred"])
	date := firstStructuredRow(t, structured, "dates")
	assert.Equal("--04-12", date["date"])
	assert.Equal([]any{"people"}, structured["categories"])
	assert.Equal([]any{"sensitive_attributes", "notes", "addresses", "media"}, structured["excluded"])
}

// firstStructuredRow returns the only row of a structured list field.
func firstStructuredRow(t *testing.T, structured map[string]any, key string) map[string]any {
	t.Helper()
	rows, ok := structured[key].([]any)
	require.True(t, ok, "%s: %#v", key, structured[key])
	require.Len(t, rows, 1, key)
	row, ok := rows[0].(map[string]any)
	require.True(t, ok, "%s row: %#v", key, rows[0])
	return row
}

func TestMCPGetPersonProfileDegradesWithoutContactStateAndTracking(t *testing.T) {
	assert := assert.New(t)
	backend := &profileReadingPeopleBackend{profile: &peoplebrowser.PersonProfile{
		Person: store.Person{ID: 9, VCardUID: "person-9"},
	}}
	result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonProfile, map[string]any{"person_id": 9})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	structured := toolStructuredContent(t, result)
	assert.Equal("person-9", structured["display_name"], "vCard UID stands in for a missing display name")
	assert.Nil(structured["tracked"])
	assert.Nil(structured["contact_state"])
	assert.NotContains(structured, "primary_channel")
	assert.NotContains(structured, "inferred_channel")
	assert.Equal([]any{}, structured["attributes"])
	assert.Equal([]any{}, structured["employments"])
	assert.Equal([]any{}, structured["relationships"])
	assert.Equal([]any{}, structured["contact_points"])
	assert.Equal([]any{}, structured["dates"])
	assert.Equal([]any{}, structured["categories"])
}

func TestMCPGetPersonProfileErrorsAndValidation(t *testing.T) {
	assert := assert.New(t)
	backend := &profileReadingPeopleBackend{profileErr: codedPeopleError{code: "person_profile_not_found"}}
	missing := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonProfile, map[string]any{"person_id": 404})
	assert.Equal(true, missing["isError"])
	assert.Contains(toolErrorTextFromResult(t, missing), "person profile not found")
	assert.Contains(toolErrorTextFromResult(t, missing), "search_people")
	assert.Equal(int64(404), backend.profilePersonID)

	backend.profilePersonID = 0
	invalid := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonProfile, map[string]any{"person_id": 0})
	assert.Equal(true, invalid["isError"])
	assert.Zero(backend.profilePersonID, "validation must not call the backend")

	plain := &handlers{peopleBackend: &recordingPeopleBackend{}}
	result, err := plain.getPersonProfile(t.Context(), toolRequest{arguments: map[string]any{"person_id": float64(7)}})
	assert.Nil(result)
	var internal *internalError
	require.ErrorAs(t, err, &internal, "a backend without profile reads fails loudly instead of guessing")
}
