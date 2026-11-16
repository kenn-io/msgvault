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
	markdownIDs   []string
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
	f.markdownIDs = append(f.markdownIDs, id)
	if f.markdownErr != nil {
		return nil, f.markdownErr
	}
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
		Object      string        `json:"object"`
		ID          string        `json:"id"`
		Type        string        `json:"type"`
		HasChildren bool          `json:"has_children"`
		Paragraph   RichTextBlock `json:"paragraph"`
	}{Object: "block", ID: id, Type: "paragraph", HasChildren: children, Paragraph: RichTextBlock{RichText: []RichText{{PlainText: text}}}})
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
		Raw:    hydrationMeeting().Raw,
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

func TestHydratorBuildsCanonicalSnapshotWithoutInventingOrganizer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := completeHydrationSource()
	source.markdown.Markdown = "# Transcript\nDifferent page transcript"
	hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(err)
	assert.Zero(source.markdownCalls)
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
	var evidence map[string]json.RawMessage
	require.NoError(json.Unmarshal(snapshot.Raw, &evidence))
	assert.JSONEq(`1`, string(evidence["schema_version"]))
	assert.Contains(string(snapshot.Raw), `"discovery"`)
	assert.JSONEq(`null`, string(evidence["page_markdown"]))
	assert.Contains(string(snapshot.Raw), `"attendee_labels":["Test Attendee","Unresolved Attendee"]`)
	assert.NotContains(string(snapshot.Raw), "unverified@example.com")
	assert.NotContains(string(snapshot.Raw), "user@example.com", "configured identity must not enter provider evidence")
	assert.Contains(string(snapshot.Metadata), `"creator_user_id":"creator-1"`)
	assert.Contains(string(snapshot.Metadata), `"unresolved_attendee_ids":["user-2"]`)
}

func TestHydratorResolvesParentForMarkdown(t *testing.T) {
	for _, tc := range []struct {
		name     string
		parent   Parent
		wantPage bool
	}{
		{name: "parent from block", wantPage: true},
		{name: "data source parent", parent: Parent{Type: "data_source_id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := completeHydrationSource()
			meeting := hydrationMeeting()
			meeting.Parent = tc.parent
			if tc.parent.Type != "" {
				source.blocks["meeting-1"].Parent = tc.parent
				var raw map[string]any
				var err error
				require.NoError(t, json.Unmarshal(source.blocks["meeting-1"].Raw, &raw))
				raw["parent"] = map[string]string{"type": "data_source_id", "data_source_id": "source-1"}
				source.blocks["meeting-1"].Raw, err = json.Marshal(raw)
				require.NoError(t, err)
			}
			empty := paragraph("transcript-1", "", false)
			source.blocks["transcript-1"] = &empty
			source.markdown.Markdown = "# Transcript\nTest Speaker: Late transcript."
			hydrated, err := NewHydrator(source).Hydrate(t.Context(), meeting)
			require.NoError(t, err)
			if tc.wantPage {
				assert.Equal(t, []string{"page-1"}, source.markdownIDs)
				require.NotNil(t, hydrated.PageMarkdown)
				assert.Equal(t, "page-1", hydrated.PageMarkdown.ID)
				assert.Equal(t, "Test Speaker: Late transcript.", hydrated.Transcript)
				assert.True(t, hydrated.MarkdownTranscriptFallback)
				assert.Contains(t, hydrated.Warnings, "used page Markdown transcript")
			} else {
				assert.Zero(t, source.markdownCalls)
				assert.Empty(t, hydrated.Transcript)
			}
		})
	}
}

func TestHydratorDegradesWhenTranscriptBlockIsNotReady(t *testing.T) {
	tests := []struct {
		name                      string
		err                       error
		fatal, cancel, secondPage bool
	}{
		{
			name: "provider reports transcript block not found",
			err:  &APIError{Kind: ErrProvider, Status: 404, Code: "object_not_found"},
		},
		{name: "server retry exhausted", err: &APIError{Kind: ErrRateLimited, Status: 503, Code: "service_unavailable"}},
		{name: "reset", err: syscall.ECONNRESET},
		{name: "truncated", err: io.ErrUnexpectedEOF},
		{name: "malformed section root", err: fmt.Errorf("%w: invalid block", ErrMalformedResponse)},
		{name: "unauthorized", err: &APIError{Kind: ErrUnauthorized, Status: 401, Code: "unauthorized"}, fatal: true},
		{name: "forbidden second page", err: &APIError{Kind: ErrReadContent, Status: 403, Code: "forbidden"}, secondPage: true, fatal: true},
		{name: "transient error after cancel", err: &APIError{Kind: ErrRateLimited, Status: 429, Code: "rate_limited"}, cancel: true, fatal: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := completeHydrationSource()
			if tt.secondPage {
				source.children["notes-1"][""].HasMore = true
				source.children["notes-1"][""].NextCursor = "next"
				child := source.children["notes-1"][""].Results[0]
				source.children["notes-1"][""].Raw = json.RawMessage(`{"object":"list","results":[` + string(child.Raw) + `],"has_more":true,"next_cursor":"next"}`)
				source.childErrs = map[string]map[string]error{"notes-1": {"next": tt.err}}
			} else {
				source.blockErrs = map[string]error{"transcript-1": tt.err}
			}

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
			source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Published in Markdown.\n\n## Action items\nFollow up."
			hydrated, err := NewHydrator(source).Hydrate(ctx, hydrationMeeting())
			if tt.fatal {
				require.Error(t, err)
				assert.Nil(t, hydrated)
				assert.ErrorIs(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "Decide the release scope.", hydrated.Summary)
			assert.Contains(t, hydrated.Notes, "Nested action item.")
			assert.Equal(t, "Test Speaker: Published in Markdown.", hydrated.Transcript)
			assert.Equal(t, 1, source.markdownCalls)
			assert.True(t, hydrated.MarkdownTranscriptFallback)
			assert.Contains(t, hydrated.Incomplete, "transcript")
			assert.Len(t, hydrated.AttendeeLabels, 2)
		})
	}
}

func TestHydratorRejectsNonMeetingRootBlock(t *testing.T) {
	for _, bad := range []struct{ id, kind string }{{"meeting-1", "paragraph"}, {"other-1", "meeting_notes"}} {
		t.Run(bad.kind+"-"+bad.id, func(t *testing.T) {
			source := completeHydrationSource()
			source.blocks["meeting-1"] = &Block{
				Object: "block", ID: bad.id, Type: bad.kind,
			}
			var err error
			source.blocks["meeting-1"].Raw, err = json.Marshal(source.blocks["meeting-1"])
			require.NoError(t, err)

			hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())

			require.ErrorIs(t, err, ErrMalformedResponse)
			assert.Nil(t, hydrated)
		})
	}
}

func TestExtractMarkdownTranscript(t *testing.T) {
	for _, tc := range []struct {
		name, markdown, want string
	}{
		{"deeper heading", "# Meeting\n## Transcript\nFirst\n### Detail\nSecond\n## Actions\nExclude", "First\n### Detail\nSecond"},
		{"CRLF and case", "# Meeting\r\n### tRaNsCrIpT\r\nKeep\r\n## Notes\r\nExclude", "Keep"},
		{"EOF", "# Meeting\n## Transcript\nKeep to EOF", "Keep to EOF"},
		{"bare marker", "# Meeting\nTranscript:\nKeep\n## Actions\nExclude", "Keep"},
		{"inline", "Transcript: Inline text\n## Notes\nExclude", "Inline text"},
		{"absent", "# Notes\nSome notes", ""},
		{"empty", "# Transcript\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := extractMarkdownTranscript(tc.markdown)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestHydratorLeavesIncompleteMarkdownTranscriptPending(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*MarkdownPage)
		err    error
	}{
		{"truncated", func(p *MarkdownPage) { p.Truncated = true }, nil},
		{"unknown blocks", func(p *MarkdownPage) { p.UnknownBlockIDs = []string{"block-1"} }, nil},
		{"unavailable", nil, &APIError{Kind: ErrRateLimited, Status: 503, Code: "service_unavailable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := completeHydrationSource()
			empty := paragraph("transcript-1", "", false)
			source.blocks["transcript-1"] = &empty
			source.markdown.Markdown = "# Notes\nNo transcript here"
			if tc.mutate != nil {
				tc.mutate(source.markdown)
			}
			source.markdownErr = tc.err
			result, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
			require.NoError(t, err)
			assert.Empty(t, result.Transcript)
			assert.Contains(t, result.Incomplete, "transcript")
			assert.Contains(t, result.Warnings, "transcript incomplete: markdown")
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

func TestHydratorLimitsKeepSectionsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		limit                      string
		markdownCalls              int
		incomplete                 []string
		skipped                    []string
		checkSummary, notesPresent bool
	}{
		{"requests", 0, []string{"summary", "notes", "transcript"}, []string{"notes-1", "transcript-1"}, true, false},
		{"bytes", 1, []string{"transcript"}, nil, false, false},
		{"blocks", 1, []string{"summary", "notes", "transcript"}, []string{"notes-1", "transcript-1"}, true, false},
		{"depth", 0, []string{"summary"}, nil, true, true},
	} {
		t.Run(tc.limit, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			source := completeHydrationSource()
			meeting := hydrationMeeting()
			root := source.blocks["summary-1"]
			switch tc.limit {
			case "bytes":
				payload := strings.Repeat("x", 12<<20)
				source.blocks["meeting-1"].Raw = json.RawMessage(`{"object":"block","id":"meeting-1","type":"meeting_notes","padding":"` + payload + `"}`)
				source.blocks["notes-1"].Raw = json.RawMessage(`{"object":"block","id":"notes-1","type":"paragraph","padding":"` + payload + `"}`)
				source.markdown.Raw = json.RawMessage(`{"object":"page_markdown","id":"page-1","markdown":"` + payload + `"}`)
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
				pages := map[string]*BlockPage{}
				for i := range 100 {
					cursor := ""
					if i > 0 {
						cursor = strconv.Itoa(i)
					}
					blocks := make([]Block, 100)
					for j := range blocks {
						blocks[j] = paragraph(fmt.Sprintf("block-%d", i*100+j), "", false)
					}
					pages[cursor] = &BlockPage{Results: blocks, HasMore: i < 99, NextCursor: strconv.Itoa(i + 1)}
				}
				source.children["summary-1"] = pages
			case "depth":
				root.HasChildren = true
				id := "summary-1"
				for i := 0; i <= maxHydrationDepth; i++ {
					next := fmt.Sprintf("depth-%d", i)
					child := paragraph(next, "", true)
					source.children[id] = map[string]*BlockPage{"": {Results: []Block{child}}}
					id = next
				}
			}
			for _, pages := range source.children {
				for _, page := range pages {
					var err error
					page.Raw, err = json.Marshal(page)
					require.NoError(err)
				}
			}
			result, err := NewHydrator(source).Hydrate(t.Context(), meeting)
			require.NoError(err)
			assert.Equal(tc.markdownCalls, source.markdownCalls)
			for _, section := range tc.incomplete {
				assert.Contains(result.Incomplete, section)
			}
			for _, id := range tc.skipped {
				assert.NotContains(source.blockCalls, id)
			}
			if tc.checkSummary {
				assert.Empty(result.SummaryTree.Blocks)
			}
			if tc.notesPresent {
				assert.NotContains(result.Incomplete, "notes")
				assert.NotEmpty(result.Notes)
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
				source.blockErrs = map[string]error{"summary-1": &APIError{Kind: ErrRateLimited, Status: 429, Code: "rate_limited"}}
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
			for _, page := range source.children["notes-1"] {
				var err error
				page.Raw, err = json.Marshal(page)
				require.NoError(t, err)
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

func TestHydratorSkipsNestedMeetingAndReadsSibling(t *testing.T) {
	source := completeHydrationSource()
	var nested Block
	require.NoError(t, json.Unmarshal([]byte(`{"object":"block","id":"other-meeting","type":"meeting_notes","has_children":true,"meeting_notes":{"title":[{"plain_text":"Another meeting"}]}}`), &nested))
	sibling := paragraph("sibling", "Sibling notes", true)
	action := paragraph("action", "Sibling action", false)
	source.childErrs = map[string]map[string]error{"other-meeting": {"": errors.New("unreadable nested meeting")}}
	source.children["notes-1"][""] = &BlockPage{Results: []Block{nested, sibling}}
	source.children["sibling"] = map[string]*BlockPage{"": {Results: []Block{action}}}
	source.children["notes-1"][""].Raw = json.RawMessage(`{"object":"list","results":[` + string(nested.Raw) + `,` + string(sibling.Raw) + `],"has_more":false}`)
	source.children["sibling"][""].Raw = json.RawMessage(`{"object":"list","results":[` + string(action.Raw) + `],"has_more":false}`)
	hydrated, err := NewHydrator(source).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(t, err)
	assert.NotContains(t, hydrated.Notes, "Another meeting")
	assert.Contains(t, hydrated.Notes, "Sibling action")
}
