package notionmeetings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeHydrationSource struct {
	blocks     map[string]*Block
	blockErrs  map[string]error
	children   map[string]map[string]*BlockPage
	markdown   *MarkdownPage
	users      map[string]*UserPage
	usersErr   error
	userErrs   map[string]error
	usersCalls int
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
	page, ok := f.children[id][cursor]
	if !ok {
		return nil, errors.New("missing children page")
	}
	return page, nil
}

func (f *fakeHydrationSource) RetrievePageMarkdown(_ context.Context, _ string, _ bool) (*MarkdownPage, error) {
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
	raw, err := json.Marshal(struct {
		Object string `json:"object"`
		ID     string `json:"id"`
		Type   string `json:"type"`
		Text   string `json:"text"`
	}{Object: "block", ID: id, Type: "paragraph", Text: text})
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
	meetingBlock := Block{
		Object: "block", ID: "meeting-1", Type: "meeting_notes",
		Parent: Parent{Type: "page_id", PageID: "page-1"},
		Raw:    json.RawMessage(`{"object":"block","id":"meeting-1","type":"meeting_notes"}`),
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

func TestHydratorResolvesParentPageFromRetrievedMeetingBlock(t *testing.T) {
	meeting := hydrationMeeting()
	meeting.Parent = Parent{}

	hydrated, err := NewHydrator(completeHydrationSource()).Hydrate(context.Background(), meeting)
	require.NoError(t, err)
	assert.Equal(t, "page-1", hydrated.Discovery.Parent.PageID)
	assert.Equal(t, "page-1", hydrated.PageMarkdown.ID)
}

func TestHydratorBuildsCanonicalSnapshotWithoutInventingOrganizer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hydrated, err := NewHydrator(completeHydrationSource()).Hydrate(context.Background(), hydrationMeeting())
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
	assert.Contains(string(snapshot.Raw), `"page_markdown"`)
	assert.Contains(string(snapshot.Raw), `"attendee_labels":["Test Attendee","Unresolved Attendee"]`)
	assert.NotContains(string(snapshot.Raw), "unverified@example.com")
	assert.NotContains(string(snapshot.Raw), "user@example.com", "configured identity must not enter provider evidence")
	assert.Contains(string(snapshot.Metadata), `"creator_user_id":"creator-1"`)
	assert.Contains(string(snapshot.Metadata), `"unresolved_attendee_ids":["user-2"]`)
}

func TestHydratorUsesMarkdownTranscriptFallback(t *testing.T) {
	assert := assert.New(t)
	source := completeHydrationSource()
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Late transcript."

	hydrated, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())
	require.NoError(t, err)
	assert.Equal("Test Speaker: Late transcript.", hydrated.Transcript)
	assert.True(hydrated.MarkdownTranscriptFallback)
	assert.Contains(hydrated.Warnings, "structured transcript was empty; used page Markdown transcript")
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

			hydrated, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())
			require.NoError(t, err)
			assert.Equal("Decide the release scope.", hydrated.Summary)
			assert.Contains(hydrated.Notes, "Nested action item.")
			assert.Equal("Test Speaker: Published in Markdown.", hydrated.Transcript)
			assert.True(hydrated.MarkdownTranscriptFallback)
			assert.Contains(hydrated.Warnings, "structured transcript was temporarily unavailable; continued with page Markdown")
			assert.Contains(hydrated.Warnings, "structured transcript was empty; used page Markdown transcript")
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
		{name: "local traversal limit", err: errors.New("notion block traversal exceeded block limit")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := completeHydrationSource()
			source.blockErrs = map[string]error{"transcript-1": tt.err}

			hydrated, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())
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

	hydrated, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())

	require.ErrorIs(t, err, ErrMalformedResponse)
	assert.Nil(t, hydrated)
}

func TestExtractMarkdownTranscriptStopsAtSameOrHigherHeading(t *testing.T) {
	tests := []struct {
		name     string
		markdown string
		want     string
	}{
		{
			name: "same-level heading",
			markdown: "# Meeting\n## Transcript\nTest Speaker: First.\n" +
				"### Transcript detail\nTest Speaker: Second.\n## Action items\nDo not include.",
			want: "Test Speaker: First.\n### Transcript detail\nTest Speaker: Second.",
		},
		{
			name:     "higher-level heading with CRLF",
			markdown: "# Meeting\r\n### tRaNsCrIpT\r\nTest Speaker: Keep.\r\n## Notes\r\nExclude.",
			want:     "Test Speaker: Keep.",
		},
		{
			name:     "transcript reaches EOF",
			markdown: "# Meeting\n## Transcript\nTest Speaker: Keep to EOF.",
			want:     "Test Speaker: Keep to EOF.",
		},
		{
			name:     "bare marker stops at next heading",
			markdown: "# Meeting\nTranscript:\nTest Speaker: Keep.\n## Action items\nExclude.",
			want:     "Test Speaker: Keep.",
		},
		{
			name:     "inline transcript",
			markdown: "Transcript: Test Speaker: Inline text.\n## Notes\nExclude.",
			want:     "Test Speaker: Inline text.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractMarkdownTranscript(tt.markdown))
		})
	}
}

func TestHydratorMarkdownFallbackExcludesLaterSections(t *testing.T) {
	assert := assert.New(t)
	source := completeHydrationSource()
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Late transcript.\n\n## Action items\nDo not archive as transcript."

	hydrated, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())
	require.NoError(t, err)
	assert.Equal("Test Speaker: Late transcript.", hydrated.Transcript)
	assert.NotContains(hydrated.Transcript, "Action items")
	assert.NotContains(hydrated.Transcript, "Do not archive")
}

func TestHydratorDoesNotWarnWhenOnlyLaterMarkdownSectionDiffers(t *testing.T) {
	source := completeHydrationSource()
	source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Ready to ship.\n\n## Action items\nDifferent content."

	hydrated, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())
	require.NoError(t, err)
	assert.NotContains(t, hydrated.Warnings, "structured transcript and page Markdown transcript differed")
}

func TestHydratorLeavesIncompleteMarkdownTranscriptPending(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*MarkdownPage)
	}{
		{
			name: "truncated",
			mutate: func(page *MarkdownPage) {
				page.Truncated = true
			},
		},
		{
			name: "unknown blocks",
			mutate: func(page *MarkdownPage) {
				page.UnknownBlockIDs = []string{"transcript-child-1"}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			source := completeHydrationSource()
			empty := paragraph("transcript-1", "", false)
			source.blocks["transcript-1"] = &empty
			source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Partial transcript."
			tt.mutate(source.markdown)

			hydrated, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())
			require.NoError(t, err)
			assert.Empty(hydrated.Transcript)
			assert.False(hydrated.MarkdownTranscriptFallback)
			assert.Contains(hydrated.Warnings, "page Markdown transcript was incomplete; transcript remains pending")
		})
	}
}

func TestHydratorDoesNotCompareIncompleteMarkdownTranscript(t *testing.T) {
	source := completeHydrationSource()
	source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Partial conflicting transcript."
	source.markdown.Truncated = true

	hydrated, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())
	require.NoError(t, err)
	assert.NotContains(t, hydrated.Warnings, "structured transcript and page Markdown transcript differed")
}

func TestHydratorDoesNotWarnAboutIncompleteMarkdownWithoutTranscript(t *testing.T) {
	source := completeHydrationSource()
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning\n\nMeeting context only."
	source.markdown.UnknownBlockIDs = []string{"unrelated-block-1"}

	hydrated, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())
	require.NoError(t, err)
	assert.Empty(t, hydrated.Transcript)
	assert.NotContains(t, hydrated.Warnings, "page Markdown transcript was incomplete; transcript remains pending")
}

func TestHydratorRejectsRepeatedChildCursor(t *testing.T) {
	source := completeHydrationSource()
	source.children["notes-1"][""] = &BlockPage{HasMore: true, NextCursor: "same"}
	source.children["notes-1"]["same"] = &BlockPage{HasMore: true, NextCursor: "same"}

	_, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repeated child cursor")
}

func TestHydratorDegradesWhenUserInformationIsUnavailable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := completeHydrationSource()
	source.usersErr = ErrUserInformation

	hydrated, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())
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

			hydrated, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())
			require.NoError(err)
			assert.True(hydrated.AttendeeResolutionDegraded)
			require.Len(hydrated.Attendees, 1)
			assert.Equal("attendee@example.com", hydrated.Attendees[0].Email)
			assert.Equal([]string{"user-2"}, hydrated.UnresolvedAttendeeIDs)
			require.Len(hydrated.Warnings, 1)
			assert.Contains(hydrated.Warnings[0], "Notion User Information pagination was incomplete: "+tt.want)
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

			hydrated, err := NewHydrator(source).Hydrate(context.Background(), hydrationMeeting())
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

	first, err := hydrator.Hydrate(context.Background(), hydrationMeeting())
	require.NoError(err)
	assert.Empty(first.Attendees)
	assert.True(first.AttendeeResolutionDegraded)
	assert.Equal([]string{"user-1", "user-2"}, first.AttendeeLabels)
	assert.Contains(first.Warnings, "Notion User Information lookup failed: rate limit exceeded; attendee emails were not resolved")

	second, err := hydrator.Hydrate(context.Background(), hydrationMeeting())
	require.NoError(err)
	assert.Empty(second.Attendees)
	assert.True(second.AttendeeResolutionDegraded)
	assert.Contains(second.Warnings, "Notion User Information lookup failed: rate limit exceeded; attendee emails were not resolved")
	assert.Equal(1, source.usersCalls)
}
