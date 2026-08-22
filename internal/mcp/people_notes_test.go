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
}

func (b *recordingPeopleBackend) Search(_ context.Context, request peoplebrowser.SearchRequest) (*peoplebrowser.SearchPage, error) {
	b.searchRequest = request
	return b.searchPage, b.searchErr
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
	return ServeOptions{Engine: &querytest.MockEngine{}, PeopleBackend: backend}
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
	withoutPeople := toolsByName(t, rawListTools(t, ServeOptions{Engine: &querytest.MockEngine{}}, true))
	for _, name := range []string{ToolSearchPeople, ToolGetPersonNotes, ToolPromotePerson, ToolUpdatePersonNotes} {
		assert.NotContains(t, withoutPeople, name)
	}

	backend := &recordingPeopleBackend{}
	readOnly := toolsByName(t, rawListTools(t, peopleToolOptions(backend), false))
	assert.Contains(t, readOnly, ToolSearchPeople)
	assert.Contains(t, readOnly, ToolGetPersonNotes)
	assert.NotContains(t, readOnly, ToolPromotePerson)
	assert.NotContains(t, readOnly, ToolUpdatePersonNotes)

	writable := toolsByName(t, rawListTools(t, peopleToolOptions(backend), true))
	for _, name := range []string{ToolSearchPeople, ToolGetPersonNotes, ToolPromotePerson, ToolUpdatePersonNotes} {
		assert.Contains(t, writable, name)
	}
	assert.Equal(t, []string{"cursor", "limit", "query"}, toolPropertyNames(t, writable[ToolSearchPeople]))
	assert.Equal(t, []string{"person_id"}, toolPropertyNames(t, writable[ToolGetPersonNotes]))
	assert.Equal(t, []string{"participant_id"}, toolPropertyNames(t, writable[ToolPromotePerson]))
	assert.Equal(t, []string{"expected_value_id", "mode", "person_id", "text"}, toolPropertyNames(t, writable[ToolUpdatePersonNotes]))
	assert.Equal(t, true, writable[ToolSearchPeople]["annotations"].(map[string]any)["readOnlyHint"])
	assert.Equal(t, true, writable[ToolGetPersonNotes]["annotations"].(map[string]any)["readOnlyHint"])
	assert.Equal(t, false, writable[ToolPromotePerson]["annotations"].(map[string]any)["readOnlyHint"])
	assert.Equal(t, false, writable[ToolUpdatePersonNotes]["annotations"].(map[string]any)["readOnlyHint"])
}

func TestMCPSearchPeopleForwardsArgumentsAndReturnsStructuredPage(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	backend := &recordingPeopleBackend{searchPage: &peoplebrowser.SearchPage{
		Rows:       []query.PersonSummary{{ID: 11, DisplayLabel: "Test Person", Identifiers: []query.PersonIdentifier{}, SourceCounts: []query.SourceCount{}, FirstAt: now, LastAt: now, CacheRevision: "cache-7"}},
		TotalCount: 31, NextCursor: "people-next", CacheRevision: "cache-7",
	}}
	result := rawCallTool(t, peopleToolOptions(backend), ToolSearchPeople, map[string]any{"query": "test", "limit": 25, "cursor": "people-cursor"})
	assert.NotEqual(t, true, result["isError"], "result: %#v", result)
	assert.Equal(t, peoplebrowser.SearchRequest{Query: "test", Limit: 25, Cursor: "people-cursor"}, backend.searchRequest)
	structured := toolStructuredContent(t, result)
	assert.Equal(t, float64(31), structured["total_count"])
	assert.Equal(t, "people-next", structured["next_cursor"])
	assert.Equal(t, "cache-7", structured["cache_revision"])
	rows := structured["rows"].([]any)
	assert.Equal(t, "Test Person", rows[0].(map[string]any)["display_label"])
}

func TestMCPGetPersonNotesReturnsOnlyNotesFields(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	notes, unrelated := "Private context", "must not escape"
	backend := &recordingPeopleBackend{attributes: &peoplebrowser.Attributes{PersonID: 7, Groups: []peoplebrowser.AttributeGroup{
		{Definition: store.AttributeDefinition{Slug: store.AttributeSlugNotes}, Current: []store.PersonAttributeValue{{ID: 71, PersonID: 7, DefinitionSlug: store.AttributeSlugNotes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &notes}, CreatedAt: now}}},
		{Definition: store.AttributeDefinition{Slug: "private_token"}, Current: []store.PersonAttributeValue{{ID: 99, PersonID: 7, DefinitionSlug: "private_token", Value: store.AttributeValue{Type: store.AttributeValueText, Text: &unrelated}}}},
	}}}
	result := rawCallTool(t, peopleToolOptions(backend), ToolGetPersonNotes, map[string]any{"person_id": 7})
	assert.NotEqual(t, true, result["isError"], "result: %#v", result)
	assert.Equal(t, int64(7), backend.attributesPersonID)
	structured := toolStructuredContent(t, result)
	assert.Equal(t, map[string]any{"exists": true, "person_id": float64(7), "text": notes, "updated_at": "2026-08-20T12:30:00Z", "value_id": float64(71)}, structured)
	assert.NotContains(t, result, unrelated)
}

func TestMCPPromotePersonReturnsDurableProfile(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	backend := &recordingPeopleBackend{promotedPerson: &store.Person{ID: 7, VCardUID: "person-7", Revision: 1, ParticipantIDs: []int64{11}, CreatedAt: now, UpdatedAt: now}}
	result := rawCallTool(t, peopleToolOptions(backend), ToolPromotePerson, map[string]any{"participant_id": 11})
	assert.NotEqual(t, true, result["isError"], "result: %#v", result)
	assert.Equal(t, int64(11), backend.promoteParticipantID)
	structured := toolStructuredContent(t, result)
	assert.Equal(t, float64(7), structured["id"])
	assert.Equal(t, []any{float64(11)}, structured["participant_ids"])
}

func TestMCPUpdatePersonNotesAppendIsAtomicAndRejectsCAS(t *testing.T) {
	text := "Existing\nFragment"
	backend := &recordingPeopleBackend{
		attributes:  &peoplebrowser.Attributes{PersonID: 7},
		appendWrite: &store.PersonAttributeWrite{Value: &store.PersonAttributeValue{ID: 72, PersonID: 7, DefinitionSlug: store.AttributeSlugNotes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &text}}},
	}
	result := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, map[string]any{"person_id": 7, "text": "Fragment"})
	assert.NotEqual(t, true, result["isError"], "result: %#v", result)
	assert.Equal(t, int64(7), backend.attributesPersonID)
	assert.Equal(t, peoplebrowser.AppendNoteRequest{PersonID: 7, Text: "Fragment", Actor: "mcp"}, backend.appendRequest)
	structured := toolStructuredContent(t, result)
	assert.Equal(t, float64(72), structured["current"].(map[string]any)["id"])
	assert.Nil(t, structured["superseded"])

	backend.appendRequest = peoplebrowser.AppendNoteRequest{}
	rejected := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, map[string]any{"person_id": 7, "text": "Fragment", "mode": "append", "expected_value_id": 71})
	assert.Equal(t, true, rejected["isError"])
	assert.Contains(t, toolErrorTextFromResult(t, rejected), "expected_value_id is not allowed with append")
	assert.Equal(t, peoplebrowser.AppendNoteRequest{}, backend.appendRequest)
}

func TestMCPUpdatePersonNotesReplaceRequiresAndForwardsCAS(t *testing.T) {
	oldText, newText := "Old", "New"
	current := store.PersonAttributeValue{ID: 71, PersonID: 7, DefinitionSlug: store.AttributeSlugNotes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &oldText}}
	backend := &recordingPeopleBackend{
		attributes: &peoplebrowser.Attributes{PersonID: 7, Groups: []peoplebrowser.AttributeGroup{{Definition: store.AttributeDefinition{Slug: store.AttributeSlugNotes}, Current: []store.PersonAttributeValue{current}}}},
		setWrite:   &store.PersonAttributeWrite{Value: &store.PersonAttributeValue{ID: 72, PersonID: 7, DefinitionSlug: store.AttributeSlugNotes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &newText}}, Superseded: &current},
	}
	missingCAS := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, map[string]any{"person_id": 7, "text": newText, "mode": "replace"})
	assert.Equal(t, true, missingCAS["isError"])
	assert.Contains(t, toolErrorTextFromResult(t, missingCAS), "expected_value_id is required")

	result := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, map[string]any{"person_id": 7, "text": newText, "mode": "replace", "expected_value_id": 71})
	assert.NotEqual(t, true, result["isError"], "result: %#v", result)
	require.NotNil(t, backend.setRequest.ExpectedValueID)
	assert.Equal(t, int64(71), *backend.setRequest.ExpectedValueID)
	assert.Equal(t, int64(7), backend.setRequest.PersonID)
	assert.Equal(t, store.AttributeSlugNotes, backend.setRequest.Slug)
	assert.Equal(t, store.AttributeValueText, backend.setRequest.Value.Type)
	assert.Equal(t, newText, *backend.setRequest.Value.Text)
}

func TestMCPUpdatePersonNotesReplaceCreationRejectsCAS(t *testing.T) {
	newText := "First note"
	backend := &recordingPeopleBackend{attributes: &peoplebrowser.Attributes{PersonID: 7}, setWrite: &store.PersonAttributeWrite{Value: &store.PersonAttributeValue{ID: 72, PersonID: 7, DefinitionSlug: store.AttributeSlugNotes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &newText}}}}
	rejected := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, map[string]any{"person_id": 7, "text": newText, "mode": "replace", "expected_value_id": 71})
	assert.Equal(t, true, rejected["isError"])
	assert.Contains(t, toolErrorTextFromResult(t, rejected), "expected_value_id must be omitted")

	result := rawCallTool(t, peopleToolOptions(backend), ToolUpdatePersonNotes, map[string]any{"person_id": 7, "text": newText, "mode": "replace"})
	assert.NotEqual(t, true, result["isError"], "result: %#v", result)
	assert.Nil(t, backend.setRequest.ExpectedValueID)
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
			result := rawCallTool(t, peopleToolOptions(test.backend), ToolUpdatePersonNotes, map[string]any{"person_id": 11, "text": "Note", "mode": test.mode})
			assert.Equal(t, true, result["isError"])
			message := toolErrorTextFromResult(t, result)
			assert.Contains(t, message, "promote_person")
			assert.Contains(t, message, "participant_id")
			assert.Zero(t, test.backend.promoteParticipantID)
			assert.Equal(t, peoplebrowser.AppendNoteRequest{}, test.backend.appendRequest)
		})
	}
}

func TestMCPPersonNotesValidationDoesNotCallBackend(t *testing.T) {
	backend := &recordingPeopleBackend{}
	for _, args := range []map[string]any{{"person_id": 7, "text": "   "}, {"person_id": 7, "text": "Note", "mode": "overwrite"}, {"person_id": 0, "text": "Note"}} {
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
