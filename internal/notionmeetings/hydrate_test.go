package notionmeetings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeHydrationSource struct {
	blocks        map[string]*Block
	blockErrs     map[string]error
	childErrs     map[string]map[string]error
	children      map[string]map[string]*BlockPage
	markdown      *MarkdownPage
	users         map[string]*UserPage
	usersErr      error
	userErrs      map[string]error
	usersCalls    int
	markdownCalls int
	markdownID    string
	markdownErr   error
	blockCalls    []string
	onBlock       func(string)
}

func (f *fakeHydrationSource) RetrieveBlock(_ context.Context, id string) (*Block, error) {
	f.blockCalls = append(f.blockCalls, id)
	if f.onBlock != nil {
		f.onBlock(id)
	}
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
	if err := f.childErrs[id][cursor]; err != nil {
		return nil, err
	}
	page, ok := f.children[id][cursor]
	if !ok {
		return nil, errors.New("missing children page")
	}
	return page, nil
}

func (f *fakeHydrationSource) RetrievePageMarkdown(_ context.Context, id string, _ bool) (*MarkdownPage, error) {
	f.markdownCalls++
	f.markdownID = id
	return f.markdown, f.markdownErr
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
	meeting := MeetingNote{
		Object: "block", ID: "meeting-1", Type: "meeting_notes",
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
	raw, err := json.Marshal(meeting)
	if err != nil {
		panic(err)
	}
	meeting.Raw = raw
	return meeting
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

	source := completeHydrationSource()
	hydrated, err := NewHydrator(source).Hydrate(t.Context(), meeting)
	require.NoError(t, err)
	assert.Equal(t, "page-1", hydrated.Discovery.Parent.PageID)
	assert.Equal(t, "page-1", source.markdownID)
}

func TestHydratorDataSourceIgnoresRetrievedPageParent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	meeting := hydrationMeeting()
	meeting.Parent = Parent{Type: "data_source_id"}
	var err error
	meeting.Raw, err = json.Marshal(meeting)
	require.NoError(err)
	source := completeHydrationSource()
	source.markdown.Markdown = "# Transcript\nAnother meeting"
	result, err := NewHydrator(source).Hydrate(t.Context(), meeting)
	require.NoError(err)
	assert.Equal("Test Speaker: Ready to ship.", result.Transcript)
	assert.Zero(source.markdownCalls)
	assert.Equal("data_source_id", result.Discovery.Parent.Type)
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
	assert.Contains(string(snapshot.Raw), `"page_markdown":{"object":"page_markdown","id":"page-1"`)
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

	hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
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
		{name: "rate limit", err: ErrRateLimited},
		{name: "server", err: &APIError{Kind: ErrProvider, Status: 503}},
		{name: "reset", err: syscall.ECONNRESET},
		{name: "timeout", err: context.DeadlineExceeded},
		{name: "truncated", err: io.ErrUnexpectedEOF},
		{name: "oversized section", err: ErrResponseTooLarge},
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
			assert.True(hydrated.MarkdownTranscriptFallback)
			assert.Contains(hydrated.Incomplete, "transcript")
			assert.Contains(hydrated.Warnings, "structured transcript was empty; used page Markdown transcript")
			assert.Len(hydrated.AttendeeLabels, 2)
		})
	}
	t.Run("data source", func(t *testing.T) {
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				meeting := hydrationMeeting()
				meeting.Parent = Parent{Type: "data_source_id"}
				source := completeHydrationSource()
				source.blocks["meeting-1"].Parent = meeting.Parent
				source.blockErrs = map[string]error{"transcript-1": tt.err}
				hydrated, err := NewHydrator(source).Hydrate(t.Context(), meeting)
				require.NoError(t, err)
				assert.Empty(t, hydrated.Transcript)
				assert.Contains(t, hydrated.Incomplete, "transcript")
			})
		}
	})
}

func TestHydratorKeepsSystemicTranscriptErrorsFatal(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		cancel bool
	}{
		{name: "unauthorized", err: &APIError{Kind: ErrUnauthorized, Status: 401, Code: "unauthorized"}},
		{name: "read content forbidden", err: &APIError{Kind: ErrReadContent, Status: 403, Code: "restricted_resource"}},
		{name: "malformed response", err: fmt.Errorf("%w: invalid block", ErrMalformedResponse)},
		{name: "transient error after cancel", err: ErrRateLimited, cancel: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := completeHydrationSource()
			source.blockErrs = map[string]error{"transcript-1": tt.err}

			ctx := t.Context()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				source.onBlock = func(id string) {
					if id == "transcript-1" {
						cancel()
					}
				}
			}
			hydrated, err := NewHydrator(source).Hydrate(ctx, hydrationMeeting())
			require.Error(t, err)
			assert.Nil(t, hydrated)
			assert.ErrorIs(t, err, tt.err)
		})
	}
}

func TestHydratorRejectsNonMeetingRootBlock(t *testing.T) {
	for _, bad := range []struct{ id, kind string }{{"meeting-1", "paragraph"}, {"other-1", "meeting_notes"}} {
		source := completeHydrationSource()
		source.blocks["meeting-1"] = &Block{
			Object: "block", ID: bad.id, Type: bad.kind,
			Raw: json.RawMessage(`{"object":"block","id":"meeting-1","type":"paragraph","has_children":false,"paragraph":{}}`),
		}

		hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())

		require.ErrorIs(t, err, ErrMalformedResponse)
		assert.Nil(t, hydrated)
	}
}

func TestExtractMarkdownTranscript(t *testing.T) {
	for _, tc := range []struct{ name, markdown, want string }{
		{"heading", "# Notes\nSome notes\n## Transcript\nSpeaker: Hello\n## Actions\nNext", "Speaker: Hello"},
		{"deeper heading", "# Meeting\n## Transcript\nFirst\n### Detail\nSecond\n## Actions\nExclude", "First\n### Detail\nSecond"},
		{"CRLF and case", "# Meeting\r\n### tRaNsCrIpT\r\nKeep\r\n## Notes\r\nExclude", "Keep"},
		{"EOF", "# Meeting\n## Transcript\nKeep to EOF", "Keep to EOF"},
		{"bare marker", "# Meeting\nTranscript:\nKeep\n## Actions\nExclude", "Keep"},
		{"inline", "Transcript: Inline text\n## Notes\nExclude", "Inline text"},
		{"absent", "# Notes\nSome notes", ""},
		{"empty", "# Transcript\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, extractMarkdownTranscript(tc.markdown)) })
	}
}

func TestHydratorLeavesIncompleteMarkdownTranscriptPending(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*MarkdownPage)
	}{
		{"truncated", func(p *MarkdownPage) { p.Truncated = true }},
		{"unknown blocks", func(p *MarkdownPage) { p.UnknownBlockIDs = []string{"block-1"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			source := completeHydrationSource()
			empty := paragraph("transcript-1", "", false)
			source.blocks["transcript-1"] = &empty
			tc.mutate(source.markdown)
			result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.NoError(t, err)
			assert.Empty(result.Transcript)
			assert.Contains(result.Incomplete, "transcript")
			assert.Contains(result.Warnings, "transcript incomplete: markdown")
		})
	}
	t.Run("no transcript in incomplete page", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		source := completeHydrationSource()
		empty := paragraph("transcript-1", "", false)
		source.blocks["transcript-1"] = &empty
		source.markdown.Markdown = "# Notes\nMeeting context"
		source.markdown.UnknownBlockIDs = []string{"unrelated"}
		result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
		require.NoError(err)
		assert.Empty(result.Transcript)
		assert.Contains(result.Incomplete, "transcript")
		assert.True(result.markdownOnlyIncomplete)
	})
}

func TestHydratorStoresAndComparesPageMarkdown(t *testing.T) {
	for _, tc := range []struct {
		name, markdown     string
		differs, truncated bool
	}{
		{"same words", "# Transcript\nTest Speaker: Ready   to ship.", false, false},
		{"different", "# Transcript\nAnother transcript", true, false},
		{"incomplete different", "# Transcript\nPartial conflicting transcript", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			source := completeHydrationSource()
			source.markdown.Markdown = tc.markdown
			source.markdown.Truncated = tc.truncated
			result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.NoError(t, err)
			assert.Equal("Test Speaker: Ready to ship.", result.Transcript)
			assert.Same(source.markdown, result.PageMarkdown)
			if tc.differs {
				assert.Contains(result.Warnings, "structured transcript and page Markdown transcript differed")
			} else {
				assert.NotContains(result.Warnings, "structured transcript and page Markdown transcript differed")
			}
		})
	}
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
	assert.Contains(first.Warnings, "Notion User Information lookup failed: rate limit exceeded; attendee emails were not resolved")

	second, err := hydrator.Hydrate(t.Context(), hydrationMeeting())
	require.NoError(err)
	assert.Empty(second.Attendees)
	assert.True(second.AttendeeResolutionDegraded)
	assert.Contains(second.Warnings, "Notion User Information lookup failed: rate limit exceeded; attendee emails were not resolved")
	assert.Equal(1, source.usersCalls)
}

func TestHydratorSkipsNestedMeetingChildren(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := completeHydrationSource()
	var nested Block
	require.NoError(json.Unmarshal([]byte(`{"object":"block","id":"nested-meeting","type":"meeting_notes","has_children":true,"meeting_notes":{}}`), &nested))
	source.children["notes-1"][""] = &BlockPage{
		Results: []Block{nested},
		Raw:     json.RawMessage(`{"object":"list","results":[{"object":"block","id":"nested-meeting","type":"meeting_notes","has_children":true,"meeting_notes":{}}],"has_more":false,"next_cursor":null}`),
	}
	result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(err)
	assert.Empty(result.Incomplete)
}

func TestHydratorLimitsKeepSectionsIncomplete(t *testing.T) {
	for _, limit := range []string{"requests", "bytes", "blocks", "depth"} {
		t.Run(limit, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			source := completeHydrationSource()
			meeting := hydrationMeeting()
			if limit == "blocks" || limit == "depth" {
				meeting.Parent = Parent{Type: "data_source_id"}
				var err error
				meeting.Raw, err = json.Marshal(meeting)
				require.NoError(err)
				source.blocks["meeting-1"].Parent = meeting.Parent
			}
			root := source.blocks["summary-1"]
			switch limit {
			case "bytes":
				payload := strings.Repeat("x", 12<<20)
				source.blocks["meeting-1"].Raw = json.RawMessage(`{"object":"block","id":"meeting-1","type":"meeting_notes","padding":"` + payload + `"}`)
				source.blocks["notes-1"].Raw = json.RawMessage(`{"object":"block","id":"notes-1","type":"paragraph","padding":"` + payload + `"}`)
				source.markdown.Raw = json.RawMessage(`{"object":"page_markdown","id":"page-1","markdown":"` + payload + `"}`)
				source.blockErrs = map[string]error{"summary-1": ErrRateLimited}
				empty := paragraph("transcript-1", "", false)
				source.blocks["transcript-1"] = &empty
			case "requests":
				root.HasChildren = true
				pages := map[string]*BlockPage{}
				for i := range maxHydrationRequests - 2 {
					cursor := ""
					if i > 0 {
						cursor = strconv.Itoa(i)
					}
					pages[cursor] = &BlockPage{HasMore: true, NextCursor: strconv.Itoa(i + 1)}
				}
				source.children["summary-1"] = pages
			case "blocks":
				root.HasChildren = true
				blocks := make([]Block, maxHydrationBlocks)
				for i := range blocks {
					blocks[i] = Block{ID: fmt.Sprintf("block-%d", i), Type: "paragraph"}
				}
				source.children["summary-1"] = map[string]*BlockPage{"": {Results: blocks}}
			case "depth":
				root.HasChildren = true
				id := "summary-1"
				for i := 0; i <= maxHydrationDepth; i++ {
					next := fmt.Sprintf("depth-%d", i)
					source.children[id] = map[string]*BlockPage{"": {Results: []Block{{ID: next, Type: "paragraph", HasChildren: true}}}}
					id = next
				}
			}
			result, err := NewHydrator(source).Hydrate(t.Context(), meeting)
			require.NoError(err)
			if limit == "bytes" {
				assert.Equal(1, source.markdownCalls)
				assert.Contains(result.Incomplete, "transcript")
				return
			}
			assert.Contains(result.Incomplete, "summary")
			assert.Empty(result.SummaryTree.Blocks)
			if limit == "requests" {
				assert.Zero(source.markdownCalls)
			}
			if limit == "depth" {
				assert.NotContains(result.Incomplete, "notes")
				assert.NotEmpty(result.Notes)
			} else {
				assert.Contains(result.Incomplete, "notes")
				assert.Contains(result.Incomplete, "transcript")
				assert.NotContains(source.blockCalls, "notes-1")
				assert.NotContains(source.blockCalls, "transcript-1")
			}
		})
	}
}

func TestHydratorAttendeeLimitRequiresIncompleteSection(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprintf("incomplete=%t", incomplete), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			source := completeHydrationSource()
			if incomplete {
				source.blockErrs = map[string]error{"summary-1": ErrRateLimited}
			}
			source.users = map[string]*UserPage{}
			for i := range maxHydrationRequests {
				cursor := ""
				if i > 0 {
					cursor = strconv.Itoa(i)
				}
				source.users[cursor] = &UserPage{HasMore: true, NextCursor: strconv.Itoa(i + 1)}
			}
			result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			if incomplete {
				require.NoError(err)
				assert.Contains(result.Incomplete, "summary")
				assert.True(result.AttendeeResolutionDegraded)
			} else {
				require.Error(err)
				assert.Nil(result)
			}
		})
	}
}

func TestHydratorPaginationFailureDiscardsPartialSection(t *testing.T) {
	for _, mode := range []string{"missing cursor", "repeated cursor", "second page failure", "first page malformed"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			source := completeHydrationSource()
			child := paragraph("partial-1", "Partial text", false)
			source.children["notes-1"] = map[string]*BlockPage{"": {Results: []Block{child}, HasMore: true, NextCursor: "next"}}
			switch mode {
			case "missing cursor":
				source.children["notes-1"][""].NextCursor = ""
			case "repeated cursor":
				source.children["notes-1"]["next"] = &BlockPage{HasMore: true, NextCursor: "next"}
			case "second page failure":
				source.childErrs = map[string]map[string]error{"notes-1": {"next": errors.New("incomplete page")}}
			case "first page malformed":
				source.childErrs = map[string]map[string]error{"notes-1": {"": ErrMalformedResponse}}
			}
			result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.NoError(t, err)
			assert.Contains(result.Incomplete, "notes")
			assert.Empty(result.NotesTree.Blocks)
			assert.NotContains(result.Notes, "Partial text")
			assert.NotEmpty(result.Summary)
			assert.NotEmpty(result.Transcript)
		})
	}
}

func TestHydratorSurfacesMarkdownFailures(t *testing.T) {
	source := completeHydrationSource()
	source.markdownErr = &APIError{Kind: ErrProvider, Status: 503}
	meeting, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.ErrorIs(t, err, source.markdownErr)
	assert.Nil(t, meeting)
	assert.Equal(t, "page-1", source.markdownID)
}
