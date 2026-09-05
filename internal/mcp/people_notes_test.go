package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
)

type recordingPeopleBackend struct {
	peoplebrowser.Backend

	searchRequest        peoplebrowser.SearchRequest
	searchPage           *peoplebrowser.SearchPage
	searchErr            error
	profiles             []store.Person
	profilesErr          error
	attributesPersonID   int64
	attributes           *peoplebrowser.Attributes
	attributesErr        error
	promoteParticipantID int64
	promotedPerson       *store.Person
	promoteErr           error
	setRequest           peoplebrowser.SetAttributeRequest
	setWrite             *store.PersonAttributeWrite
	setErr               error
	appendRequest        peoplebrowser.AppendNoteRequest
	appendWrite          *store.PersonAttributeWrite
	appendErr            error
	contactParticipantID int64
	contact              *query.PersonSummary
	contactErr           error
	calendarRequest      peoplebrowser.CalendarRequest
	calendar             *query.RelationshipCalendarResponse
	calendarErr          error
}

func (b *recordingPeopleBackend) GetContact(_ context.Context, participantID int64) (*query.PersonSummary, error) {
	b.contactParticipantID = participantID
	return b.contact, b.contactErr
}

func (b *recordingPeopleBackend) RelationshipCalendar(
	_ context.Context, request peoplebrowser.CalendarRequest,
) (*query.RelationshipCalendarResponse, error) {
	b.calendarRequest = request
	return b.calendar, b.calendarErr
}

func (b *recordingPeopleBackend) Search(_ context.Context, request peoplebrowser.SearchRequest) (*peoplebrowser.SearchPage, error) {
	b.searchRequest = request
	return b.searchPage, b.searchErr
}

func (b *recordingPeopleBackend) ListProfiles(_ context.Context) ([]store.Person, error) {
	return b.profiles, b.profilesErr
}

func (b *recordingPeopleBackend) ListAttributes(_ context.Context, personID int64) (*peoplebrowser.Attributes, error) {
	b.attributesPersonID = personID
	return b.attributes, b.attributesErr
}

func (b *recordingPeopleBackend) Promote(_ context.Context, participantID int64) (*store.Person, error) {
	b.promoteParticipantID = participantID
	return b.promotedPerson, b.promoteErr
}

func (b *recordingPeopleBackend) SetAttribute(_ context.Context, request peoplebrowser.SetAttributeRequest) (*store.PersonAttributeWrite, error) {
	b.setRequest = request
	return b.setWrite, b.setErr
}

func (b *recordingPeopleBackend) AppendNote(_ context.Context, request peoplebrowser.AppendNoteRequest) (*store.PersonAttributeWrite, error) {
	b.appendRequest = request
	return b.appendWrite, b.appendErr
}

type codedPeopleError struct{ code string }

func (e codedPeopleError) Error() string        { return e.code }
func (e codedPeopleError) APIErrorCode() string { return e.code }

func peopleToolOptions(backend peoplebrowser.Backend) ServeOptions {
	return ServeOptions{
		Engine: &querytest.MockEngine{}, PeopleBackend: backend,
		AllowProfileWrites: true,
	}
}

func toolStructuredContent(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	structured, ok := result["structuredContent"].(map[string]any)
	require.True(t, ok, "result: %#v", result)
	return structured
}

func toolErrorTextFromResult(t *testing.T, result map[string]any) string {
	t.Helper()
	return task3ToolErrorText(t, task3RPCResponse{Result: result})
}

func TestMCPPeopleCapabilityAndWritePolicy(t *testing.T) {
	assert := assert.New(t)
	withoutPeople := toolsByName(t, rawListTools(t, ServeOptions{Engine: &querytest.MockEngine{}}, true))
	for _, name := range []string{ToolSearchPeople, ToolGetPersonNotes, ToolGetPersonProfile, ToolGetPersonRelationship, ToolPromotePerson, ToolUpdatePersonNotes} {
		assert.NotContains(withoutPeople, name)
	}

	backend := &recordingPeopleBackend{}
	readOnly := toolsByName(t, rawListTools(t, peopleToolOptions(backend), false))
	assert.Contains(readOnly, ToolSearchPeople)
	assert.Contains(readOnly, ToolGetPersonNotes)
	assert.Contains(readOnly, ToolGetPersonProfile)
	assert.Contains(readOnly, ToolGetPersonRelationship)
	assert.NotContains(readOnly, ToolPromotePerson)
	assert.NotContains(readOnly, ToolUpdatePersonNotes)

	defaultWrites := toolsByName(t, rawListTools(t, ServeOptions{
		Engine: &querytest.MockEngine{}, PeopleBackend: backend,
	}, true))
	assert.Contains(defaultWrites, ToolSearchPeople)
	assert.NotContains(defaultWrites, ToolPromotePerson)
	assert.NotContains(defaultWrites, ToolUpdatePersonNotes)

	writable := toolsByName(t, rawListTools(t, peopleToolOptions(backend), true))
	for _, name := range []string{ToolSearchPeople, ToolGetPersonNotes, ToolGetPersonProfile, ToolGetPersonRelationship, ToolPromotePerson, ToolUpdatePersonNotes} {
		assert.Contains(writable, name)
	}
	assert.Equal([]string{"cursor", "limit", "query"}, toolPropertyNames(t, writable[ToolSearchPeople]))
	assert.Equal([]string{"person_id"}, toolPropertyNames(t, writable[ToolGetPersonNotes]))
	assert.Equal([]string{"participant_id", "timezone", "year"}, toolPropertyNames(t, writable[ToolGetPersonRelationship]))
	assert.Equal([]string{"participant_id"}, toolPropertyNames(t, writable[ToolPromotePerson]))
	assert.Equal([]string{"expected_value_id", "mode", "person_id", bodyFormatText}, toolPropertyNames(t, writable[ToolUpdatePersonNotes]))
	assert.Equal(true, toolReadOnlyHint(t, writable[ToolSearchPeople]))
	assert.Equal(true, toolReadOnlyHint(t, writable[ToolGetPersonNotes]))
	assert.Equal(true, toolReadOnlyHint(t, writable[ToolGetPersonRelationship]))
	for _, phrase := range []string{"emotional truth", "consent", "authorization"} {
		assert.Contains(writable[ToolGetPersonRelationship]["description"], phrase)
	}
	assert.Equal(false, toolReadOnlyHint(t, writable[ToolPromotePerson]))
	assert.Equal(false, toolReadOnlyHint(t, writable[ToolUpdatePersonNotes]))
}

func toolReadOnlyHint(t *testing.T, tool map[string]any) any {
	t.Helper()
	annotations, ok := tool["annotations"].(map[string]any)
	require.True(t, ok)
	return annotations["readOnlyHint"]
}

func TestMCPGetPersonRelationshipReturnsSummaryAndOptionalCalendar(t *testing.T) {
	assert := assert.New(t)
	contact := query.PersonSummary{
		ID: 7, DisplayLabel: "Test Person", CurrentRelationshipTemperature: 62,
		PeakRelationshipTemperature: 87, PeakRelationshipYear: 2018,
	}
	calendar := &query.RelationshipCalendarResponse{
		CanonicalID: 7, Year: 2025, Timezone: "America/New_York",
		Days:            []query.RelationshipCalendarDay{{Date: "2025-01-02", Total: 4}},
		PeakTemperature: 87, PeakYear: 2018, ScoringTimezone: "UTC",
		ScoreVersion: 1, EffectiveDate: "2026-08-22", CacheRevision: "cache-8", IdentityRevision: 12,
	}
	calendar.Current.Temperature = 62
	backend := &recordingPeopleBackend{contact: &contact, calendar: calendar}

	result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonRelationship, map[string]any{
		"participant_id": 7, "year": 2025, "timezone": "America/New_York",
	})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.Equal(int64(7), backend.contactParticipantID)
	assert.Equal(peoplebrowser.CalendarRequest{
		ParticipantID: 7, Year: 2025, Timezone: "America/New_York",
	}, backend.calendarRequest)
	structured := toolStructuredContent(t, result)
	assert.Equal("Test Person", structured["display_label"])
	current, ok := structured["current"].(map[string]any)
	require.True(t, ok)
	assert.InDelta(float64(62), current["temperature"], 0)
	assert.InDelta(float64(87), structured["peak_temperature"], 0)
	assert.InDelta(float64(2025), structured["year"], 0)
	assert.Len(structured["days"], 1)

	backend.calendarRequest = peoplebrowser.CalendarRequest{}
	result = rawCallTool(t, peopleToolOptions(backend), ToolGetPersonRelationship, map[string]any{
		"participant_id": 7,
	})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.Equal("UTC", backend.calendarRequest.Timezone)
	assert.NotZero(backend.calendarRequest.Year)
	assert.NotContains(toolStructuredContent(t, result), "days")
}

func TestMCPGetPersonRelationshipValidationDoesNotCallBackend(t *testing.T) {
	backend := &recordingPeopleBackend{}
	result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonRelationship, map[string]any{
		"participant_id": 0, "year": 2025,
	})
	assert.Equal(t, true, result["isError"])
	assert.Zero(t, backend.contactParticipantID)
	assert.Equal(t, peoplebrowser.CalendarRequest{}, backend.calendarRequest)
}

func TestMCPSearchPeopleForwardsArgumentsAndReturnsStructuredPage(t *testing.T) {
	assert := assert.New(t)
	now := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	backend := &recordingPeopleBackend{searchPage: &peoplebrowser.SearchPage{
		Rows:       []query.PersonSummary{{ID: 11, DisplayLabel: "Test Person", Identifiers: []query.PersonIdentifier{}, SourceCounts: []query.SourceCount{}, FirstAt: now, LastAt: now, CacheRevision: "cache-7"}},
		TotalCount: 31, NextCursor: "people-next", CacheRevision: "cache-7",
	}}
	result := rawCallTool(t, peopleToolOptions(backend), ToolSearchPeople, map[string]any{"query": "test", "limit": 25, "cursor": "people-cursor"})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.Equal(peoplebrowser.SearchRequest{Query: "test", Limit: 25, Cursor: "people-cursor"}, backend.searchRequest)
	structured := toolStructuredContent(t, result)
	assert.InDelta(float64(31), structured["total_count"], 0)
	assert.Equal("people-next", structured["next_cursor"])
	assert.Equal("cache-7", structured["cache_revision"])
	rows, ok := structured["rows"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, rows)
	row, ok := rows[0].(map[string]any)
	require.True(t, ok)
	assert.Equal("Test Person", row["display_label"])
	assert.InDelta(float64(0), row["current_relationship_temperature"], 0)
}

func TestMCPSearchPeopleIncludesCuratedOnlyDisplayNameAndProfileID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	displayName := "Curated Alias"
	now := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	backend := &recordingPeopleBackend{
		searchPage: &peoplebrowser.SearchPage{Rows: []query.PersonSummary{}, CacheRevision: "cache-7"},
		profiles: []store.Person{{
			ID: 7, VCardUID: "person-7", DisplayName: &displayName, Revision: 3,
			ParticipantIDs: []int64{11}, CreatedAt: now, UpdatedAt: now,
		}},
		contact: &query.PersonSummary{
			ID: 11, DisplayLabel: "Observed Name", Identifiers: []query.PersonIdentifier{},
			SourceCounts: []query.SourceCount{}, FirstAt: now, LastAt: now,
			CacheRevision: "cache-7",
		},
	}

	result := rawCallTool(t, peopleToolOptions(backend), ToolSearchPeople, map[string]any{
		"query": "curated alias", "limit": 20,
	})

	assert.NotEqual(true, result["isError"], "result: %#v", result)
	structured := toolStructuredContent(t, result)
	rows, ok := structured["rows"].([]any)
	require.True(ok)
	require.Len(rows, 1)
	row, ok := rows[0].(map[string]any)
	require.True(ok)
	assert.Equal("Curated Alias", row["display_label"])
	assert.InDelta(float64(11), row["id"], 0)
	assert.InDelta(float64(7), row["person_id"], 0)
	profile, ok := row["profile"].(map[string]any)
	require.True(ok)
	assert.InDelta(float64(7), profile["id"], 0)
}

func TestMCPSearchPeopleKeepsObservedPageWhenProfileQuerySpansContactFields(t *testing.T) {
	assert := assert.New(t)
	displayName := "Alice Example"
	backend := &recordingPeopleBackend{
		searchPage: &peoplebrowser.SearchPage{
			Rows: []query.PersonSummary{{
				ID: 22, DisplayLabel: "Alice Example Team",
				Identifiers: []query.PersonIdentifier{}, SourceCounts: []query.SourceCount{},
			}},
			TotalCount: 1, CacheRevision: "cache-7",
		},
		profiles: []store.Person{{
			ID: 7, VCardUID: "person-7", DisplayName: &displayName, Revision: 3,
			ParticipantIDs: []int64{11},
		}},
		contact: &query.PersonSummary{
			ID: 11, DisplayLabel: "Alice",
			Identifiers:  []query.PersonIdentifier{{DisplayValue: "Example"}},
			SourceCounts: []query.SourceCount{},
		},
	}

	result := rawCallTool(t, peopleToolOptions(backend), ToolSearchPeople, map[string]any{
		"query": "Alice Example", "limit": 20,
	})

	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.NotEmpty(toolStructuredContent(t, result)["next_cursor"])
}

func TestMCPGetPersonNotesReturnsOnlyNotesFields(t *testing.T) {
	assert := assert.New(t)
	now := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	notes, unrelated := "Private context", "must not escape"
	backend := &recordingPeopleBackend{attributes: &peoplebrowser.Attributes{PersonID: 7, Groups: []peoplebrowser.AttributeGroup{
		{Definition: store.AttributeDefinition{UniversalID: store.AttributeUniversalIDNotes, Slug: store.AttributeSlugNotes}, Current: []store.PersonAttributeValue{{ID: 71, PersonID: 7, DefinitionSlug: store.AttributeSlugNotes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &notes}, Source: store.ProvenanceEnrichment, CreatedAt: now}}},
		{Definition: store.AttributeDefinition{Slug: "private_token"}, Current: []store.PersonAttributeValue{{ID: 99, PersonID: 7, DefinitionSlug: "private_token", Value: store.AttributeValue{Type: store.AttributeValueText, Text: &unrelated}}}},
	}}}
	result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonNotes, map[string]any{"person_id": 7})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.Equal(int64(7), backend.attributesPersonID)
	structured := toolStructuredContent(t, result)
	assert.Equal(map[string]any{"exists": true, "person_id": float64(7), "source": "enrichment", bodyFormatText: notes, "updated_at": "2026-08-20T12:30:00Z", "value_id": float64(71)}, structured)
	assert.NotContains(result, unrelated)
}

func TestMCPPromotePersonReturnsDurableProfile(t *testing.T) {
	assert := assert.New(t)
	now := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	backend := &recordingPeopleBackend{promotedPerson: &store.Person{ID: 7, VCardUID: "person-7", Revision: 1, ParticipantIDs: []int64{11}, CreatedAt: now, UpdatedAt: now}}
	result := rawCallTool(t, peopleToolOptions(backend), ToolPromotePerson, map[string]any{"participant_id": 11})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.Equal(int64(11), backend.promoteParticipantID)
	structured := toolStructuredContent(t, result)
	assert.InDelta(float64(7), structured["id"], 0)
	assert.Equal([]any{float64(11)}, structured["participant_ids"])
}

func TestMCPUpdatePersonNotesAppendIsAtomicAndRejectsCAS(t *testing.T) {
	assert := assert.New(t)
	text := "Existing\nFragment"
	backend := &recordingPeopleBackend{
		attributes:  &peoplebrowser.Attributes{PersonID: 7},
		appendWrite: &store.PersonAttributeWrite{Value: &store.PersonAttributeValue{ID: 72, PersonID: 7, DefinitionSlug: store.AttributeSlugNotes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &text}}},
	}
	result := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, map[string]any{"person_id": 7, bodyFormatText: "Fragment"})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.Equal(int64(7), backend.attributesPersonID)
	assert.Equal(peoplebrowser.AppendNoteRequest{
		PersonID: 7, Text: "Fragment", Source: store.ProvenanceEnrichment, Actor: "mcp",
	}, backend.appendRequest)
	structured := toolStructuredContent(t, result)
	current, ok := structured["current"].(map[string]any)
	require.True(t, ok)
	assert.InDelta(float64(72), current["id"], 0)
	assert.Nil(structured["superseded"])

	backend.appendRequest = peoplebrowser.AppendNoteRequest{}
	rejected := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, map[string]any{"person_id": 7, bodyFormatText: "Fragment", "mode": "append", "expected_value_id": 71})
	assert.Equal(true, rejected["isError"])
	assert.Contains(toolErrorTextFromResult(t, rejected), "expected_value_id is not allowed with append")
	assert.Equal(peoplebrowser.AppendNoteRequest{}, backend.appendRequest)
}

func TestMCPUpdatePersonNotesReplaceRequiresAndForwardsCAS(t *testing.T) {
	assert := assert.New(t)
	oldText, newText := "Old", "New"
	current := store.PersonAttributeValue{ID: 71, PersonID: 7, DefinitionSlug: store.AttributeSlugNotes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &oldText}}
	backend := &recordingPeopleBackend{
		attributes: &peoplebrowser.Attributes{PersonID: 7, Groups: []peoplebrowser.AttributeGroup{{Definition: store.AttributeDefinition{UniversalID: store.AttributeUniversalIDNotes, Slug: "notes_system"}, Current: []store.PersonAttributeValue{current}}}},
		setWrite:   &store.PersonAttributeWrite{Value: &store.PersonAttributeValue{ID: 72, PersonID: 7, DefinitionSlug: store.AttributeSlugNotes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &newText}}, Superseded: &current},
	}
	missingCAS := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, map[string]any{"person_id": 7, bodyFormatText: newText, "mode": "replace"})
	assert.Equal(true, missingCAS["isError"])
	assert.Contains(toolErrorTextFromResult(t, missingCAS), "expected_value_id is required")

	result := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, map[string]any{"person_id": 7, bodyFormatText: newText, "mode": "replace", "expected_value_id": 71})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	require.NotNil(t, backend.setRequest.ExpectedValueID)
	assert.Equal(int64(71), *backend.setRequest.ExpectedValueID)
	assert.Equal(int64(7), backend.setRequest.PersonID)
	assert.Equal("notes_system", backend.setRequest.Slug)
	assert.Equal(store.AttributeValueText, backend.setRequest.Value.Type)
	assert.Equal(newText, *backend.setRequest.Value.Text)
	assert.Equal(store.ProvenanceEnrichment, backend.setRequest.Source)
	assert.Equal("mcp", backend.setRequest.Actor)
}

func TestMCPUpdatePersonNotesReplaceCreationRejectsCAS(t *testing.T) {
	assert := assert.New(t)
	newText := "First note"
	backend := &recordingPeopleBackend{attributes: &peoplebrowser.Attributes{PersonID: 7, Groups: []peoplebrowser.AttributeGroup{{Definition: store.AttributeDefinition{UniversalID: store.AttributeUniversalIDNotes, Slug: "notes_system"}}}}, setWrite: &store.PersonAttributeWrite{Value: &store.PersonAttributeValue{ID: 72, PersonID: 7, DefinitionSlug: "notes_system", Value: store.AttributeValue{Type: store.AttributeValueText, Text: &newText}}}}
	rejected := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, map[string]any{"person_id": 7, bodyFormatText: newText, "mode": "replace", "expected_value_id": 71})
	assert.Equal(true, rejected["isError"])
	assert.Contains(toolErrorTextFromResult(t, rejected), "expected_value_id must be omitted")

	result := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, map[string]any{"person_id": 7, bodyFormatText: newText, "mode": "replace"})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.Nil(backend.setRequest.ExpectedValueID)
}

func TestMCPUpdatePersonNotesRequiresExplicitPromotion(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		backend    *recordingPeopleBackend
	}{
		{name: "append", mode: "append", backend: &recordingPeopleBackend{attributesErr: codedPeopleError{code: "person_profile_not_found"}}},
		{name: "replace", mode: "replace", backend: &recordingPeopleBackend{attributesErr: codedPeopleError{code: "person_profile_not_found"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			result := rawCallTool(t, peopleToolOptions(test.backend), ToolUpdatePersonNotes, map[string]any{"person_id": 11, bodyFormatText: "Note", "mode": test.mode})
			assert.Equal(true, result["isError"])
			message := toolErrorTextFromResult(t, result)
			assert.Contains(message, "promote_person")
			assert.Contains(message, "participant_id")
			assert.Zero(test.backend.promoteParticipantID)
			assert.Equal(peoplebrowser.AppendNoteRequest{}, test.backend.appendRequest)
		})
	}
}

func TestMCPPersonNotesValidationDoesNotCallBackend(t *testing.T) {
	backend := &recordingPeopleBackend{}
	for _, args := range []map[string]any{{"person_id": 7, bodyFormatText: "   "}, {"person_id": 7, bodyFormatText: "Note", "mode": "overwrite"}, {"person_id": 0, bodyFormatText: "Note"}} {
		result := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, args)
		assert.Equal(t, true, result["isError"])
	}
	assert.Equal(t, peoplebrowser.AppendNoteRequest{}, backend.appendRequest)
	assert.Equal(t, peoplebrowser.SetAttributeRequest{}, backend.setRequest)
}

func TestMCPPersonNotesStdioRetainsTrustedWrites(t *testing.T) {
	backend := &recordingPeopleBackend{promotedPerson: &store.Person{ID: 7, VCardUID: "person-7", ParticipantIDs: []int64{11}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}}
	peer := newTask5RawStdioPeer(t, peopleToolOptions(backend))
	response := peer.call(t, task3ToolCallBody(1, ToolPromotePerson, `{"participant_id":11}`))
	require.Nil(t, response.Error)
	assert.NotEqual(t, true, response.Result["isError"], "result: %#v", response.Result)
	assert.Equal(t, int64(11), backend.promoteParticipantID)
}
