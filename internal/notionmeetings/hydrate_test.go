package notionmeetings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.kenn.io/msgvault/internal/meetingcontent"
	"hegel.dev/go/hegel"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeHydrationSource struct {
	blocks        map[string]*Block
	blockErrs     map[string]error
	children      map[string]map[string]*BlockPage
	childrenErrs  map[string]error
	markdown      *MarkdownPage
	users         map[string]*UserPage
	usersErr      error
	userErrs      map[string]error
	usersCalls    int
	markdownCalls int
}

func (f *fakeHydrationSource) RetrieveBlock(_ context.Context, id string) (*Block, error) {
	if err := f.blockErrs[id]; err != nil {
		return nil, err
	}
	block, ok := f.blocks[id]
	if !ok {
		return nil, errors.New("missing block")
	}
	return block, nil
}

func (f *fakeHydrationSource) RetrieveBlockChildren(_ context.Context, id, cursor string) (*BlockPage, error) {
	if err := f.childrenErrs[id+"/"+cursor]; err != nil {
		return nil, err
	}
	page, ok := f.children[id][cursor]
	if !ok {
		return nil, errors.New("missing children page")
	}
	return page, nil
}

func (f *fakeHydrationSource) RetrievePageMarkdown(_ context.Context, _ string, _ bool) (*MarkdownPage, error) {
	f.markdownCalls++
	return f.markdown, nil
}

func (f *fakeHydrationSource) ListUsers(_ context.Context, cursor string) (*UserPage, error) {
	f.usersCalls++
	if err := f.userErrs[cursor]; err != nil {
		return nil, err
	}
	if f.usersErr != nil {
		return nil, f.usersErr
	}
	return f.users[cursor], nil
}

func paragraph(id, text string, children bool) Block {
	raw, err := json.Marshal(map[string]any{"object": "block", "id": id, "type": "paragraph", "has_children": children, "paragraph": RichTextBlock{RichText: []RichText{{PlainText: text}}}})
	if err != nil {
		panic(err)
	}
	return Block{
		Object: "block", ID: id, Type: "paragraph", HasChildren: children,
		Paragraph: RichTextBlock{RichText: []RichText{{PlainText: text}}}, Raw: raw,
	}
}

func hydrationMeeting() MeetingNote {
	raw := json.RawMessage(`{"object":"block","id":"meeting-1","type":"meeting_notes","future":{"kept":true}}`)
	return MeetingNote{
		Object: "block", ID: "meeting-1", Type: "meeting_notes", Raw: raw,
		Parent:      Parent{Type: "page_id", PageID: "page-1"},
		CreatedTime: "2026-08-29T09:59:00Z", LastEditedTime: "2026-08-29T10:35:00Z",
		CreatedBy: UserRef{ID: "creator-1", Object: "user"},
		MeetingNotes: MeetingNotesData{
			Title:  []RichText{{PlainText: "Weekly planning"}},
			Status: "notes_ready",
			Children: MeetingChildren{
				SummaryBlockID: "summary-1", NotesBlockID: "notes-1", TranscriptBlockID: "transcript-1",
			},
			CalendarEvent: MeetingCalendarEvent{
				StartTime: "2026-08-29T10:00:00Z", EndTime: "2026-08-29T10:30:00Z",
				Attendees: []string{"user-1", "user-2"},
			},
		},
	}
}

func completeHydrationSource() *fakeHydrationSource {
	meeting := hydrationMeeting()
	raw, err := json.Marshal(meeting)
	if err != nil {
		panic(err)
	}
	var meetingBlock Block
	if err := json.Unmarshal(raw, &meetingBlock); err != nil {
		panic(err)
	}

	summaryRoot := paragraph("summary-1", "Decide the release scope.", false)
	notesRoot := paragraph("notes-1", "Owner will prepare the rollout.", true)
	transcriptRoot := paragraph("transcript-1", "Test Speaker: Ready to ship.", false)
	nested := paragraph("nested-1", "Nested action item.", false)
	return &fakeHydrationSource{
		blocks: map[string]*Block{
			"meeting-1": &meetingBlock, "summary-1": &summaryRoot,
			"notes-1": &notesRoot, "transcript-1": &transcriptRoot,
		},
		children: map[string]map[string]*BlockPage{
			"page-1": {"": {Results: []Block{meetingBlock}}},
			"notes-1": {
				"": {Results: []Block{nested}, Raw: json.RawMessage(`{"results":[{"id":"nested-1"}],"has_more":false}`)},
			},
		},
		markdown: &MarkdownPage{
			Object: "page_markdown", ID: "page-1",
			Markdown: "# Weekly planning\n\n## Transcript\nTest Speaker: Ready to ship.",
			Raw:      json.RawMessage(`{"object":"page_markdown","id":"page-1","markdown":"redacted in test"}`),
		},
		users: map[string]*UserPage{
			"": {
				Results: []User{
					{ID: "user-1", Name: "Test Attendee", Type: "person", Person: UserPerson{Email: "attendee@example.com", EmailVerified: true}},
					{ID: "user-2", Name: "Unresolved Attendee", Type: "person", Person: UserPerson{Email: "unverified@example.com"}},
				},
			},
		},
	}
}

func installMeetingHeader(t *testing.T, source *fakeHydrationSource, meeting MeetingNote) {
	t.Helper()
	raw, err := json.Marshal(meeting)
	require.NoError(t, err)
	var block Block
	require.NoError(t, json.Unmarshal(raw, &block))
	source.blocks[meeting.ID] = &block
}

func TestHydratorBuildsCanonicalSnapshotWithoutInventingOrganizer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hydrated, err := NewHydrator(completeHydrationSource()).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(err)
	assert.False(hydrated.AttendeeResolutionDegraded)
	assert.Equal("Decide the release scope.", hydrated.Summary)
	assert.Contains(hydrated.Notes, "Nested action item.")
	assert.Equal("Test Speaker: Ready to ship.", hydrated.Transcript)

	snapshot, err := hydrated.ArchiveSnapshot(17, "work", "user@example.com")
	require.NoError(err)
	assert.Equal(int64(17), snapshot.SourceID)
	assert.Equal("meeting-1", snapshot.SourceMessageID)
	assert.Equal("meeting-1", snapshot.SourceConversationID)
	assert.Nil(snapshot.Organizer, "created_by must not become organizer")
	require.Len(snapshot.Attendees, 1)
	assert.Equal("attendee@example.com", snapshot.Attendees[0].Email)
	assert.Contains(snapshot.Body, "Attendees: Test Attendee, Unresolved Attendee")
	assert.Contains(snapshot.Body, "Summary:\nDecide the release scope.")
	assert.Contains(snapshot.Body, "Notes:\nOwner will prepare the rollout.\nNested action item.")
	assert.Contains(snapshot.Body, "Transcript:\nTest Speaker: Ready to ship.")
	assert.Equal(RawFormat, snapshot.RawFormat)
	assert.Contains(string(snapshot.Raw), `"discovery"`)
	assert.NotContains(string(snapshot.Raw), `"page_markdown"`)
	assert.Contains(string(snapshot.Raw), `"attendee_labels":["Test Attendee","Unresolved Attendee"]`)
	assert.NotContains(string(snapshot.Raw), "unverified@example.com")
	assert.NotContains(string(snapshot.Raw), "user@example.com", "configured identity must not enter provider evidence")
	assert.Contains(string(snapshot.Metadata), `"creator_user_id":"creator-1"`)
	assert.Contains(string(snapshot.Metadata), `"unresolved_attendee_ids":["user-2"]`)
}

func TestHydratorDegradesWhenTranscriptBlockIsNotReady(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "provider reports transcript block not found",
			err:  &APIError{Kind: ErrProvider, Status: 404, Code: "object_not_found"},
		},
		{
			name: "provider transient retry budget exhausted",
			err:  &APIError{Kind: ErrRateLimited, Status: 503, Code: "service_unavailable"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			source := completeHydrationSource()
			source.blockErrs = map[string]error{"transcript-1": tt.err}
			source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Published in Markdown.\n\n## Action items\nFollow up."

			hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.NoError(t, err)
			assert.Equal("Decide the release scope.", hydrated.Summary)
			assert.Contains(hydrated.Notes, "Nested action item.")
			assert.Equal("Test Speaker: Published in Markdown.", hydrated.Transcript)
			assert.Contains(hydrated.Warnings, "transcript blocks were incomplete; retained available meeting content")
			assert.Equal(1, source.markdownCalls)
			assert.Len(hydrated.AttendeeLabels, 2)
		})
	}
}

func TestHydratorKeepsSystemicTranscriptErrorsFatal(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "unauthorized", err: &APIError{Kind: ErrUnauthorized, Status: 401, Code: "unauthorized"}},
		{name: "read content forbidden", err: &APIError{Kind: ErrReadContent, Status: 403, Code: "restricted_resource"}},
		{name: "malformed response", err: fmt.Errorf("%w: invalid block", ErrMalformedResponse)},
		{name: "context canceled", err: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := completeHydrationSource()
			source.blockErrs = map[string]error{"transcript-1": tt.err}

			hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.Error(t, err)
			assert.Nil(t, hydrated)
			assert.ErrorIs(t, err, tt.err)
		})
	}
}

func TestHydratorRejectsNonMeetingRootBlock(t *testing.T) {
	source := completeHydrationSource()
	source.blocks["meeting-1"] = &Block{
		Object: "block", ID: "meeting-1", Type: "paragraph",
		Raw: json.RawMessage(`{"object":"block","id":"meeting-1","type":"paragraph","has_children":false,"paragraph":{}}`),
	}

	hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())

	require.ErrorIs(t, err, ErrMalformedResponse)
	assert.Nil(t, hydrated)
}

func TestHydratorWarnsAboutIncompleteMarkdown(t *testing.T) {
	for _, kind := range []string{"truncated", "unknown blocks", "without transcript"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			source := completeHydrationSource()
			empty := paragraph("transcript-1", "", false)
			source.blocks["transcript-1"] = &empty
			source.markdown.Markdown = "## Transcript\nPartial transcript."
			if kind == "truncated" {
				source.markdown.Truncated = true
			} else {
				source.markdown.UnknownBlockIDs = []string{"unknown"}
			}
			if kind == "without transcript" {
				source.markdown.Markdown = "Meeting context only."
			}
			hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.NoError(err)
			assert.Empty(hydrated.Transcript)
			assert.Contains(hydrated.Warnings, "page Markdown transcript was incomplete; transcript remains pending")
		})
	}
}

func TestHydratorRejectsRepeatedChildCursor(t *testing.T) {
	source := completeHydrationSource()
	source.children["notes-1"][""] = &BlockPage{HasMore: true, NextCursor: "same"}
	source.children["notes-1"]["same"] = &BlockPage{HasMore: true, NextCursor: "same"}

	result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(t, err)
	assert.False(t, result.NotesTree.Complete)
	assert.Contains(t, result.Warnings, "notes blocks were incomplete; retained available meeting content")
}

func TestHydratorDegradesWhenUserInformationIsUnavailable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := completeHydrationSource()
	source.usersErr = ErrUserInformation

	hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(err)
	assert.Empty(hydrated.Attendees)
	assert.True(hydrated.AttendeeResolutionDegraded)
	assert.Equal([]string{"user-1", "user-2"}, hydrated.UnresolvedAttendeeIDs)
	assert.Contains(hydrated.Warnings, "Notion User Information access unavailable; attendee emails were not resolved")

	snapshot, err := hydrated.ArchiveSnapshot(17, "work", "user@example.com")
	require.NoError(err)
	assert.Contains(snapshot.Body, "Attendees: user-1, user-2")
}

func TestHydratorDegradesOnInvalidUserPaginationAndKeepsFetchedUsers(t *testing.T) {
	tests := []struct {
		name  string
		pages map[string]*UserPage
		want  string
	}{
		{
			name: "missing cursor",
			pages: map[string]*UserPage{
				"": {Results: []User{{
					ID: "user-1", Name: "Test Attendee", Type: "person",
					Person: UserPerson{Email: "attendee@example.com", EmailVerified: true},
				}}, HasMore: true},
			},
			want: "has_more without next_cursor",
		},
		{
			name: "repeated cursor",
			pages: map[string]*UserPage{
				"": {Results: []User{{
					ID: "user-1", Name: "Test Attendee", Type: "person",
					Person: UserPerson{Email: "attendee@example.com", EmailVerified: true},
				}}, HasMore: true, NextCursor: "same"},
				"same": {HasMore: true, NextCursor: "same"},
			},
			want: "repeated cursor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			source := completeHydrationSource()
			source.users = tt.pages

			hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.NoError(err)
			assert.True(hydrated.AttendeeResolutionDegraded)
			require.Len(hydrated.Attendees, 1)
			assert.Equal("attendee@example.com", hydrated.Attendees[0].Email)
			assert.Equal([]string{"user-2"}, hydrated.UnresolvedAttendeeIDs)
			require.Len(hydrated.Warnings, 1)
			assert.Contains(hydrated.Warnings[0], "Notion User Information pagination was incomplete; attendee emails were not resolved")
		})
	}
}

func TestHydratorKeepsSystemicUserListingErrorsFatal(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "unauthorized", err: &APIError{Kind: ErrUnauthorized, Status: 401, Code: "unauthorized"}},
		{name: "context canceled", err: context.Canceled},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := completeHydrationSource()
			source.usersErr = tt.err

			hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.Error(t, err)
			assert.Nil(t, hydrated)
			assert.ErrorIs(t, err, tt.err)
		})
	}
}

func TestHydratorDegradesAndCachesTransientUserListingFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := completeHydrationSource()
	source.usersErr = errors.New("rate limit exceeded")
	hydrator := NewHydrator(source)

	first, err := hydrator.Hydrate(t.Context(), hydrationMeeting())
	require.NoError(err)
	assert.Empty(first.Attendees)
	assert.True(first.AttendeeResolutionDegraded)
	assert.Equal([]string{"user-1", "user-2"}, first.AttendeeLabels)
	assert.Contains(first.Warnings, "Notion User Information lookup failed; attendee emails were not resolved")

	second, err := hydrator.Hydrate(t.Context(), hydrationMeeting())
	require.NoError(err)
	assert.Empty(second.Attendees)
	assert.True(second.AttendeeResolutionDegraded)
	assert.Contains(second.Warnings, "Notion User Information lookup failed; attendee emails were not resolved")
	assert.Equal(1, source.usersCalls)
}

// The authoritative block can have any parent. Query fields may be stale and
// another meeting on the same page must never supply this meeting's transcript.
func TestHydrateBlockOwnedParents(t *testing.T) {
	for _, parent := range []string{`{"type":"page_id","page_id":"page-1"}`, `{"type":"data_source_id","data_source_id":"rows-1"}`} {
		t.Run(parent, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			source := completeHydrationSource()
			children := `{}`
			if strings.Contains(parent, "page_id") {
				children = `{"transcript_block_id":"fresh-transcript"}`
			}
			raw := []byte(`{"object":"block","id":"meeting-1","type":"meeting_notes","created_time":"2026-08-29T10:00:00Z","has_children":false,"parent":` + parent + `,"meeting_notes":{"status":"notes_ready","children":` + children + `}}`)
			var block Block
			require.NoError(json.Unmarshal(raw, &block))
			source.blocks["meeting-1"] = &block
			transcript := paragraph("fresh-transcript", "Own transcript.", false)
			source.blocks[transcript.ID] = &transcript
			source.markdown.Markdown = "## Transcript\nSibling transcript."
			hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.NoError(err)
			if hydrated.Discovery.Parent.Type == "page_id" {
				assert.Equal("page-1", hydrated.Discovery.Parent.PageID)
				assert.Equal("Own transcript.", hydrated.Transcript)
			} else {
				assert.Empty(hydrated.Transcript)
			}
			snapshot, err := hydrated.ArchiveSnapshot(1, "synthetic", "")
			require.NoError(err)
			var evidence map[string]json.RawMessage
			require.NoError(json.Unmarshal(snapshot.Raw, &evidence))
			var header map[string]json.RawMessage
			require.NoError(json.Unmarshal(evidence["meeting_block"], &header))
			assert.JSONEq(parent, string(header["parent"]))
			assert.Zero(source.markdownCalls)
		})
	}
}

func TestHydrateExcludesNestedMeetingEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := completeHydrationSource()
	marker := "SIBLING_PRIVATE_CONTENT"
	payload, err := json.Marshal(map[string]any{"object": "block", "id": "other-meeting", "type": "meeting_notes", "has_children": true, "meeting_notes": map[string]any{"title": []any{map[string]any{"plain_text": marker}}}})
	require.NoError(err)
	var nested Block
	require.NoError(json.Unmarshal(payload, &nested))
	privateChild := paragraph("private-child", marker, false)
	source.children["other-meeting"] = map[string]*BlockPage{"": {Results: []Block{privateChild}, Raw: payload}}
	child := paragraph("own-child", "Own notes.", false)
	source.children["notes-1"][""] = &BlockPage{Results: []Block{nested, child}, Raw: payload}
	hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(err)
	snapshot, err := hydrated.ArchiveSnapshot(1, "synthetic", "")
	require.NoError(err)
	assert.NotContains(string(snapshot.Raw), "SIBLING_PRIVATE_")
	assert.Contains(hydrated.Notes, "Own notes.")
}

func TestV2SnapshotMeetingContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	source := completeHydrationSource()
	var task Block
	require.NoError(json.Unmarshal([]byte(`{"object":"block","id":"action-1","type":"to_do","has_children":false,"to_do":{"rich_text":[{"plain_text":"Prepare report"}],"checked":false}}`), &task))
	source.children["notes-1"][""] = &BlockPage{Results: []Block{task}}
	hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(err)
	snapshot, err := hydrated.ArchiveSnapshot(1, "synthetic", "")
	require.NoError(err)
	content := meetingcontent.Decode(RawFormat, snapshot.Raw, nil)
	require.NotNil(content.DurationSeconds)
	assert.InDelta(float64(1800), *content.DurationSeconds, 0.001)
	require.Len(content.SourceParticipants, 2)
	assert.Equal("attendee@example.com", content.SourceParticipants[0].Email)

	source.children["notes-1"][""].HasMore = true
	source.children["notes-1"][""].NextCursor = "unfinished"
	source.children["notes-1"]["unfinished"] = &BlockPage{HasMore: true, NextCursor: "unfinished"}
	hydrated, err = NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(err)
	snapshot, err = hydrated.ArchiveSnapshot(1, "synthetic", "")
	require.NoError(err)
	content = meetingcontent.Decode(RawFormat, snapshot.Raw, nil)
	assert.Equal(meetingcontent.StateTruncated, content.Notes.State)
	assert.Equal(meetingcontent.CoveragePartial, content.ActionCoverage)
	require.Len(content.Actions, 1)
}

func TestHydratePaginatedCyclicTrees(t *testing.T) {
	source := completeHydrationSource()
	root := source.blocks["notes-1"]
	first := paragraph("first-child", "First note.", false)
	last := paragraph("last-child", "Last note.", false)
	source.children["notes-1"] = map[string]*BlockPage{
		"":      {Results: []Block{first, *root}, HasMore: true, NextCursor: "page2"},
		"page2": {Results: []Block{first, last}},
	}
	result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(t, err)
	assert.Equal(t, "Owner will prepare the rollout.\nFirst note.\nLast note.", result.Notes)
	assert.False(t, result.NotesTree.Complete)
}

func TestHydrateRejectsMalformedAuthoritativeHeader(t *testing.T) {
	deleteFields := []string{"meeting_notes", "has_children"}
	for _, field := range deleteFields {
		t.Run(field, func(t *testing.T) {
			source := completeHydrationSource()
			var raw map[string]any
			require.NoError(t, json.Unmarshal(source.blocks["meeting-1"].Raw, &raw))
			delete(raw, field)
			payload, err := json.Marshal(raw)
			require.NoError(t, err)
			block := *source.blocks["meeting-1"]
			block.Raw = payload
			source.blocks[block.ID] = &block
			_, err = NewHydrator(source).Hydrate(t.Context(), MeetingNote{ID: block.ID})
			require.ErrorIs(t, err, ErrMalformedResponse)
		})
	}
}

func TestHydratorRetainsContentWhenOptionalDirectoryBudgetExhausted(t *testing.T) {
	assert := assert.New(t)

	source := completeHydrationSource()
	pages := map[string]*BlockPage{}
	for i := range maxHydrationRequests - 4 {
		cursor := ""
		if i > 0 {
			cursor = strconv.Itoa(i)
		}
		pages[cursor] = &BlockPage{HasMore: i < maxHydrationRequests-5, NextCursor: strconv.Itoa(i + 1)}
	}
	source.children["notes-1"] = pages
	hydrator := NewHydrator(source)
	result, err := hydrator.Hydrate(t.Context(), hydrationMeeting())
	require.NoError(t, err)
	assert.Equal("Test Speaker: Ready to ship.", result.Transcript)
	assert.True(result.AttendeeResolutionDegraded)
	assert.Empty(result.Attendees)

	source.children["notes-1"] = map[string]*BlockPage{"": {}}
	next, err := hydrator.Hydrate(t.Context(), hydrationMeeting())
	require.NoError(t, err)
	assert.False(next.AttendeeResolutionDegraded)
	assert.NotEmpty(next.Attendees)
}

func TestHydrationErrorsDoNotExposeIdentifiers(t *testing.T) {
	assert := assert.New(t)

	source := completeHydrationSource()
	cause := errors.New("request https://example.test/PRIVATE_BLOCK_ID?cursor=PRIVATE_CURSOR failed")
	source.blockErrs = map[string]error{"meeting-1": cause}
	_, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.Error(t, err)
	require.ErrorIs(t, err, cause)
	assert.NotContains(err.Error(), "PRIVATE_BLOCK_ID")
	assert.NotContains(err.Error(), "PRIVATE_CURSOR")
}

func TestSnapshotFullPagePresence(t *testing.T) {
	for _, value := range []any{nil, false, true} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			source := completeHydrationSource()
			var header map[string]any
			require.NoError(json.Unmarshal(source.blocks["meeting-1"].Raw, &header))
			payload, ok := header["meeting_notes"].(map[string]any)
			require.True(ok)
			delete(payload, "is_full_page")
			if value != nil {
				payload["is_full_page"] = value
			}
			raw, err := json.Marshal(header)
			require.NoError(err)
			require.NoError(json.Unmarshal(raw, source.blocks["meeting-1"]))
			result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.NoError(err)
			snapshot, err := result.ArchiveSnapshot(1, "synthetic", "")
			require.NoError(err)
			var metadata map[string]any
			require.NoError(json.Unmarshal(snapshot.Metadata, &metadata))
			got, exists := metadata["is_full_page"]
			assert.Equal(value != nil, exists)
			assert.Equal(value, got)
		})
	}
}

func TestSnapshotRejectsEvidenceLargerThanReaderLimit(t *testing.T) {
	require := require.New(t)
	source := completeHydrationSource()
	text := strings.Repeat("a", 12<<20)
	for _, id := range []string{"summary-1", "notes-1"} {
		block := paragraph(id, text, false)
		source.blocks[id] = &block
	}
	result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(err)
	_, err = result.ArchiveSnapshot(1, "synthetic", "")
	require.ErrorContains(err, "evidence limit")
}

func TestHydrateRejectsDanglingContinuation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := completeHydrationSource()
	source.children["notes-1"][""].NextCursor = "dangling"
	result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(err)
	assert.False(result.NotesTree.Complete)
	snapshot, err := result.ArchiveSnapshot(1, "synthetic", "")
	require.NoError(err)
	assert.Equal(meetingcontent.StateTruncated, meetingcontent.Decode(RawFormat, snapshot.Raw, nil).Notes.State)
}

func TestSnapshotParentDiscriminator(t *testing.T) {
	for _, kind := range []string{"data_source_id", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			source := completeHydrationSource()
			var header map[string]any
			require.NoError(json.Unmarshal(source.blocks["meeting-1"].Raw, &header))
			parent := map[string]any{"type": kind, "page_id": "MISLEADING_PAGE", kind: "actual-parent"}
			header["parent"] = parent
			raw, err := json.Marshal(header)
			require.NoError(err)
			require.NoError(json.Unmarshal(raw, source.blocks["meeting-1"]))
			result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.NoError(err)
			snapshot, err := result.ArchiveSnapshot(1, "synthetic", "")
			require.NoError(err)
			var metadata map[string]any
			require.NoError(json.Unmarshal(snapshot.Metadata, &metadata))
			assert.Empty(metadata["page_id"])
			assert.Contains(string(snapshot.Raw), "MISLEADING_PAGE", "raw source shape remains evidence")
		})
	}
}

func TestHydratedV2TokenLedger(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		source := completeHydrationSource()
		root := paragraph("notes-1", "", true)
		source.blocks[root.ID] = &root
		count := 1 + int(hegel.Draw(ht, hegel.Integers(uint(0), ^uint(0)))%8)
		var blocks []Block
		var lines []string
		var expected []meetingcontent.Action
		for i := range count {
			kind := hegel.Draw(ht, hegel.SampledFrom([]string{"paragraph", "bulleted_list_item", "numbered_list_item", "quote", "to_do"}))
			text := strings.TrimSpace(fmt.Sprintf("OWNED_%d ", i) + hegel.Draw(ht, hegel.Text()))
			id := fmt.Sprintf("item-%d", i)
			runes := []rune(text)
			split := len(runes) / 2
			payload := map[string]any{"rich_text": []RichText{{PlainText: string(runes[:split])}, {PlainText: string(runes[split:])}}}
			if kind == "to_do" {
				checked := hegel.Draw(ht, hegel.Booleans())
				payload["checked"] = checked
				status, rawStatus := meetingcontent.StatusPending, "false"
				if checked {
					status, rawStatus = meetingcontent.StatusCompleted, "true"
				}
				expected = append(expected, meetingcontent.Action{Ordinal: len(expected), SourceID: id, Title: text, Status: status, SourceStatus: rawStatus, Origin: "checkbox", Locator: "block:" + id})
			}
			prefixes := map[string]string{"bulleted_list_item": "- ", "numbered_list_item": "1. ", "quote": "> "}
			lines = append(lines, prefixes[kind]+text)
			raw, err := json.Marshal(map[string]any{"object": "block", "id": id, "type": kind, "has_children": false, kind: payload})
			require.NoError(ht, err)
			var block Block
			require.NoError(ht, json.Unmarshal(raw, &block))
			blocks = append(blocks, block)
		}
		// Arbitrary page partitions, including an empty intermediate page, must not
		// alter the independent token/action ledger.
		split := int(hegel.Draw(ht, hegel.Integers(uint(0), ^uint(0))) % uint(count+1))
		source.children[root.ID] = map[string]*BlockPage{"": {Results: blocks[:split], HasMore: true, NextCursor: "empty"}, "empty": {HasMore: true, NextCursor: "tail"}, "tail": {Results: blocks[split:]}}
		hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
		require.NoError(ht, err)
		want := strings.Join(lines, "\n")
		assert.Equal(ht, want, hydrated.Notes)
		snapshot, err := hydrated.ArchiveSnapshot(1, "synthetic", "")
		require.NoError(ht, err)
		content := meetingcontent.Decode(RawFormat, snapshot.Raw, nil)
		assert.Equal(ht, want, content.Notes.Text)
		require.Len(ht, content.Actions, len(expected))
		for i, action := range expected {
			assert.Equal(ht, action, content.Actions[i])
		}
	}, hegel.WithTestCases(128), hegel.WithDatabase(""))
}

func TestHydrationDepthAndBlockBoundaries(t *testing.T) {
	for _, depth := range []int{maxHydrationDepth - 1, maxHydrationDepth, maxHydrationDepth + 1} {
		t.Run(fmt.Sprintf("depth-%d", depth), func(t *testing.T) {
			assert := assert.New(t)

			source := completeHydrationSource()
			header := hydrationMeeting()
			header.MeetingNotes.Children = MeetingChildren{NotesBlockID: "depth-0"}
			installMeetingHeader(t, source, header)
			for i := range depth + 1 {
				id := fmt.Sprintf("depth-%d", i)
				block := paragraph(id, fmt.Sprintf("TOKEN_%d", i), i < depth)
				source.blocks[id] = &block
				if i > 0 {
					source.children[fmt.Sprintf("depth-%d", i-1)] = map[string]*BlockPage{"": {Results: []Block{block}}}
				}
			}
			result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.NoError(t, err)
			assert.Equal(depth <= maxHydrationDepth, result.NotesTree.Complete)
			assert.LessOrEqual(len(result.NotesTree.Blocks), maxHydrationDepth+1)
			if depth > maxHydrationDepth {
				assert.NotContains(result.Notes, fmt.Sprintf("TOKEN_%d", depth))
			}
		})
	}
	for _, count := range []int{maxHydrationBlocks, maxHydrationBlocks + 1} {
		t.Run(fmt.Sprintf("blocks-%d", count), func(t *testing.T) {
			source := completeHydrationSource()
			header := hydrationMeeting()
			header.MeetingNotes.Children = MeetingChildren{NotesBlockID: "notes-1"}
			installMeetingHeader(t, source, header)
			root := paragraph("notes-1", "", true)
			source.blocks[root.ID] = &root
			blocks := make([]Block, 0, count-1)
			for i := range count - 1 {
				blocks = append(blocks, paragraph(fmt.Sprintf("child-%d", i), "OWNED", false))
			}
			pages := make(map[string]*BlockPage)
			for offset := 0; offset < len(blocks); offset += pageSize {
				cursor := ""
				if offset > 0 {
					cursor = strconv.Itoa(offset)
				}
				end := min(offset+pageSize, len(blocks))
				page := &BlockPage{Results: blocks[offset:end]}
				if end < len(blocks) {
					page.HasMore = true
					page.NextCursor = strconv.Itoa(end)
				}
				pages[cursor] = page
			}
			source.children[root.ID] = pages
			result, err := NewHydrator(source).Hydrate(t.Context(), MeetingNote{ID: "meeting-1"})
			require.NoError(t, err)
			assert.Equal(t, count <= maxHydrationBlocks, result.NotesTree.Complete)
			assert.Len(t, result.NotesTree.Blocks, maxHydrationBlocks)
		})
	}
}

func TestHydrationParentCancellationStopsSectionRequests(t *testing.T) {
	source := completeHydrationSource()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/blocks/notes-1/children" {
			cancel()
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/blocks/")
		if block := source.blocks[id]; block != nil {
			_, err := w.Write(block.Raw)
			assert.NoError(t, err)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	_, err := NewHydrator(NewClient(server.URL, "synthetic")).Hydrate(ctx, hydrationMeeting())
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, paths, "/v1/blocks/transcript-1")
	assert.NotContains(t, paths, "/v1/users")
}

func TestPageMarkdownRequiresCompleteSingleMeetingTree(t *testing.T) {
	for _, scenario := range []string{"nested sibling", "unreadable page", "missing continuation", "request limit"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			source := completeHydrationSource()
			meeting := hydrationMeeting()
			meeting.MeetingNotes.Children.TranscriptBlockID = ""
			installMeetingHeader(t, source, meeting)
			source.children["page-1"][""].Results = []Block{*source.blocks["meeting-1"]}
			switch scenario {
			case "nested sibling":
				parent := paragraph("parent", "", true)
				var sibling Block
				require.NoError(json.Unmarshal([]byte(`{"object":"block","id":"meeting-2","type":"meeting_notes","has_children":false,"meeting_notes":{}}`), &sibling))
				source.children["page-1"][""].Results = append(source.children["page-1"][""].Results, parent)
				source.children[parent.ID] = map[string]*BlockPage{"": {Results: []Block{sibling}}}
			case "unreadable page":
				source.childrenErrs = map[string]error{"page-1/": ErrRateLimited}
			case "missing continuation":
				source.children["page-1"][""].HasMore = true
			case "request limit":
				page := source.children["page-1"][""]
				for i := range maxHydrationRequests {
					next := strconv.Itoa(i)
					page.HasMore, page.NextCursor = true, next
					page = &BlockPage{}
					source.children["page-1"][next] = page
				}
			}
			hydrated, err := NewHydrator(source).Hydrate(t.Context(), meeting)
			require.NoError(err)
			assert.Empty(hydrated.Transcript)
			assert.Zero(source.markdownCalls)
		})
	}
}
