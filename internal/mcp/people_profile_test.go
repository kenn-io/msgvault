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
	assert.Equal([]any{"sensitive_attributes", "notes", "media"}, structured["excluded"])
}

func TestMCPGetPersonProfileReportsEmailsPhonesAndAddress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &profileReadingPeopleBackend{profile: &peoplebrowser.PersonProfile{
		Person: store.Person{ID: 12, VCardUID: "person-12"},
		ContactPoints: []peoplebrowser.PersonContactPointSummary{
			{Kind: "email", Value: "alice@example.com", NormalizedValue: "alice@example.com", TypeLabel: "work", Source: store.ProvenanceVCardImport},
			{Kind: "phone", Value: "+15555550100", NormalizedValue: "+15555550100", Preferred: true, Source: store.ProvenanceUser},
			{Kind: "email", Value: "alice.home@example.com", NormalizedValue: "alice.home@example.com", Preferred: true, Source: store.ProvenanceUser},
			{Kind: "email", Value: "ALICE@example.com", NormalizedValue: "alice@example.com", Source: store.ProvenanceExtraction},
			{Kind: "email", Value: "alice.handle@example.com", NormalizedValue: "alice.handle@example.com", ServiceSlug: "imessage", Source: store.ProvenanceExtraction},
			{Kind: "phone", Value: "+1 555 555 0100", NormalizedValue: "+15555550100", ServiceSlug: "sms", Source: store.ProvenanceExtraction},
			{Kind: "phone", Value: "+15555550188", NormalizedValue: "+15555550188", ServiceSlug: "signal", Source: store.ProvenanceUser},
			{Kind: "username", Value: "alice", ServiceSlug: "chat", Source: store.ProvenanceUser},
		},
		Addresses: []peoplebrowser.PersonAddressSummary{
			{
				Kind: "birth_place", Locality: "Example Town", OriginalValue: ";;;Example Town;;;",
				Preferred: true, Source: store.ProvenanceVCardImport,
			},
			{
				Kind: "postal", StreetAddress: "2 Example Avenue", Locality: "Example City",
				OriginalValue: ";;2 Example Avenue;Example City;;;", Source: store.ProvenanceExtraction,
			},
			{
				Kind: "postal", Label: "Home", StreetAddress: "1 Example Street", Locality: "Example City",
				Region: "EX", PostalCode: "12345", CountryName: "Exampleland", CountryCode: "EX",
				OriginalValue: ";;1 Example Street;Example City;EX;12345;Exampleland",
				Preferred:     true, Source: store.ProvenanceUser,
			},
		},
	}}

	result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonProfile, map[string]any{"person_id": 12})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	structured := toolStructuredContent(t, result)
	assert.Equal([]any{"alice.home@example.com", "alice@example.com"}, structured["emails"],
		"preferred first, one entry per distinct address, and no service-scoped handle")
	assert.Equal([]any{"+15555550100", "+15555550188"}, structured["phones"],
		"numbers collapse on their normalized form, and a number carried by a service still reaches the person")

	address, ok := structured["address"].(map[string]any)
	require.True(ok, "address: %#v", structured["address"])
	assert.Equal("postal", address["kind"], "a preferred postal address is the primary one")
	assert.Equal("Home", address["label"])
	assert.Equal("1 Example Street", address["street_address"])
	assert.Equal("Example City", address["locality"])
	assert.Equal("EX", address["region"])
	assert.Equal("12345", address["postal_code"])
	assert.Equal("Exampleland", address["country_name"])
	assert.Equal("EX", address["country_code"])
	assert.Equal(";;1 Example Street;Example City;EX;12345;Exampleland", address["original_value"])
	assert.Equal(true, address["preferred"])
	assert.Equal(string(store.ProvenanceUser), address["source"])
	assert.NotContains(address, "post_office_box", "components the source did not carry stay out")

	points, ok := structured["contact_points"].([]any)
	require.True(ok, "contact_points: %#v", structured["contact_points"])
	assert.Len(points, 8, "the new fields summarize contact points instead of replacing them")
}

func TestMCPGetPersonProfileAddressSkipsBirthAndDeathPlaces(t *testing.T) {
	birthPlace := peoplebrowser.PersonAddressSummary{
		Kind: "birth_place", Locality: "Example Town", OriginalValue: ";;;Example Town;;;",
		Preferred: true, Source: store.ProvenanceVCardImport,
	}
	deathPlace := peoplebrowser.PersonAddressSummary{
		Kind: "death_place", Locality: "Example Village", OriginalValue: ";;;Example Village;;;",
		Preferred: true, Source: store.ProvenanceVCardImport,
	}
	postal := peoplebrowser.PersonAddressSummary{
		Kind: "postal", StreetAddress: "1 Example Street", Locality: "Example City",
		OriginalValue: ";;1 Example Street;Example City;;;", Source: store.ProvenanceUser,
	}
	tests := []struct {
		name       string
		addresses  []peoplebrowser.PersonAddressSummary
		wantStreet string
	}{
		{name: "birth place only", addresses: []peoplebrowser.PersonAddressSummary{birthPlace}},
		{name: "death place only", addresses: []peoplebrowser.PersonAddressSummary{deathPlace}},
		{
			name:       "preferred birth place listed before a postal address",
			addresses:  []peoplebrowser.PersonAddressSummary{birthPlace, postal},
			wantStreet: "1 Example Street",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			backend := &profileReadingPeopleBackend{profile: &peoplebrowser.PersonProfile{
				Person: store.Person{ID: 14, VCardUID: "person-14"}, Addresses: test.addresses,
			}}
			result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonProfile, map[string]any{"person_id": 14})
			assert.NotEqual(true, result["isError"], "result: %#v", result)
			structured := toolStructuredContent(t, result)
			if test.wantStreet == "" {
				assert.Nil(structured["address"], "a birth or death place is not where the person lives")
				return
			}
			address, ok := structured["address"].(map[string]any)
			require.True(t, ok, "address: %#v", structured["address"])
			assert.Equal("postal", address["kind"])
			assert.Equal(test.wantStreet, address["street_address"])
		})
	}
}

func TestMCPGetPersonProfileWithoutEmailsPhonesOrAddress(t *testing.T) {
	assert := assert.New(t)
	backend := &profileReadingPeopleBackend{profile: &peoplebrowser.PersonProfile{
		Person: store.Person{ID: 13, VCardUID: "person-13"},
		ContactPoints: []peoplebrowser.PersonContactPointSummary{
			{Kind: "username", Value: "alice", ServiceSlug: "chat", Source: store.ProvenanceUser},
		},
	}}
	result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonProfile, map[string]any{"person_id": 13})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	structured := toolStructuredContent(t, result)
	assert.Equal([]any{}, structured["emails"])
	assert.Equal([]any{}, structured["phones"])
	assert.Nil(structured["address"])
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
	assert.Equal([]any{}, structured["emails"])
	assert.Equal([]any{}, structured["phones"])
	assert.Nil(structured["address"])
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
