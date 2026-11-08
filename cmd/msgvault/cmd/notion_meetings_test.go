package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/testutil"
)

type fakeNotionProbe struct {
	result   *notionmeetings.QueryResult
	block    *notionmeetings.Block
	err      error
	blockErr error
	usersErr error
}

func (f fakeNotionProbe) QueryMeetingNotes(context.Context, int) (*notionmeetings.QueryResult, error) {
	return f.result, f.err
}

func (f fakeNotionProbe) RetrieveBlock(context.Context, string) (*notionmeetings.Block, error) {
	return f.block, f.blockErr
}

func (f fakeNotionProbe) RetrievePageMarkdown(context.Context, string, bool) (*notionmeetings.MarkdownPage, error) {
	return &notionmeetings.MarkdownPage{Markdown: "private transcript"}, nil
}

func (f fakeNotionProbe) RetrieveBlockChildren(context.Context, string, string) (*notionmeetings.BlockPage, error) {
	return &notionmeetings.BlockPage{}, nil
}

func (f fakeNotionProbe) ListUsers(context.Context, string) (*notionmeetings.UserPage, error) {
	return &notionmeetings.UserPage{}, f.usersErr
}

func notionProbeBlock(t *testing.T, meeting notionmeetings.MeetingNote) *notionmeetings.Block {
	t.Helper()
	meeting.Object, meeting.Type = "block", "meeting_notes"
	raw, err := json.Marshal(meeting)
	require.NoError(t, err)
	var block notionmeetings.Block
	require.NoError(t, json.Unmarshal(raw, &block))
	return &block
}

func TestResolveNotionMeetingsSourcesRequiresProbeIdentifierForMultipleSources(t *testing.T) {
	cfg := testConfigValue()

	assert := assert.New(t)
	require := require.New(t)
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	cfg = &config.Config{NotionMeetings: []config.NotionMeetingsSource{
		{Identifier: "personal", Token: "secret-1"},
		{Identifier: "work", Token: "secret-2"},
	}}
	testCtx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	_ = testCtx

	_, err := resolveNotionMeetingsSources(nil, true, cfg)
	require.Error(err)
	assert.Contains(err.Error(), "multiple [[notion_meetings]]")

	sources, err := resolveNotionMeetingsSources([]string{"work"}, true, cfg)
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal("work", sources[0].Identifier)
}

func TestRunNotionMeetingsProbeIsContentAndTokenSafe(t *testing.T) {
	assert := assert.New(t)
	meeting := notionmeetings.MeetingNote{
		ID: "private-block-id", MeetingNotes: notionmeetings.MeetingNotesData{
			Title: []notionmeetings.RichText{{PlainText: "Private meeting title"}},
		},
		Parent: notionmeetings.Parent{PageID: "private-page-id"},
	}
	var out bytes.Buffer
	err := runNotionMeetingsProbe(t.Context(), &out, fakeNotionProbe{
		result: &notionmeetings.QueryResult{Results: []notionmeetings.MeetingNote{meeting}, HasMore: true},
		block:  notionProbeBlock(t, meeting),
	}, nil)
	require.NoError(t, err)
	assert.Contains(out.String(), "Returned meetings: 1")
	assert.Contains(out.String(), "Coverage: sample only; sync coverage was not measured")
	assert.NotContains(out.String(), "Partial coverage: true")
	assert.NotContains(out.String(), "private-block-id")
	assert.NotContains(out.String(), "Private meeting title")
	assert.NotContains(out.String(), "secret-token")
	assert.NotContains(out.String(), "private transcript")
	assert.Contains(out.String(), "Read Content: available")
	assert.Contains(out.String(), "User Information: available")
}

func TestRunNotionMeetingsProbeHydratesWithoutPageParent(t *testing.T) {
	var out bytes.Buffer
	err := runNotionMeetingsProbe(t.Context(), &out, fakeNotionProbe{
		result: &notionmeetings.QueryResult{Results: []notionmeetings.MeetingNote{{ID: "meeting-1"}}},
		block:  notionProbeBlock(t, notionmeetings.MeetingNote{ID: "meeting-1", Parent: notionmeetings.Parent{Type: "data_source_id", DataSourceID: "rows-1"}}),
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Read Content: available")
}

func TestRunNotionMeetingsProbeChecksBlockWhenQueryHasParent(t *testing.T) {
	var out bytes.Buffer
	err := runNotionMeetingsProbe(t.Context(), &out, fakeNotionProbe{
		result: &notionmeetings.QueryResult{Results: []notionmeetings.MeetingNote{{
			ID: "meeting-1", Parent: notionmeetings.Parent{PageID: "page-1"},
		}}},
		blockErr: errors.New("block endpoint unavailable"),
	}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "sample content")
}

func TestRunNotionMeetingsProbeDegradesWithoutUserInformation(t *testing.T) {
	var out bytes.Buffer
	err := runNotionMeetingsProbe(t.Context(), &out, fakeNotionProbe{
		result: &notionmeetings.QueryResult{}, usersErr: notionmeetings.ErrUserInformation,
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Read Content: not tested")
	assert.Contains(t, out.String(), "User Information: unavailable")
}

func TestRunNotionMeetingsProbeDegradesOnTransientUserListingFailure(t *testing.T) {
	var out bytes.Buffer
	err := runNotionMeetingsProbe(t.Context(), &out, fakeNotionProbe{
		result: &notionmeetings.QueryResult{}, usersErr: &notionmeetings.APIError{
			Kind: notionmeetings.ErrRateLimited, Status: 503, Code: "service_unavailable",
		},
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "User Information: unavailable")
	assert.NotContains(t, out.String(), "status 503")
}

func TestRunNotionMeetingsProbeOptionalUserListingFailuresRemainNonfatal(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "unauthorized", err: &notionmeetings.APIError{
			Kind: notionmeetings.ErrUnauthorized, Status: 401, Code: "unauthorized",
		}},
		{name: "context canceled", err: context.Canceled},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
		{name: "malformed response", err: notionmeetings.ErrMalformedResponse},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runNotionMeetingsProbe(t.Context(), &out, fakeNotionProbe{
				result: &notionmeetings.QueryResult{}, usersErr: tt.err,
			}, nil)
			require.NoError(t, err)
			assert.Contains(t, out.String(), "User Information: unavailable")
		})
	}
}

func TestRunConfiguredNotionMeetingsSyncRefusesRemovedSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	err := runConfiguredNotionMeetingsSync(t.Context(), st, config.NotionMeetingsSource{
		Identifier: "removed", Token: "secret-token",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add-notion-meetings removed")
}

type fakeNotionUsersProbe struct {
	user      *notionmeetings.User
	err       error
	retrieved int
	ids       []string
}

func (f *fakeNotionUsersProbe) RetrieveUser(ctx context.Context, id string) (*notionmeetings.User, error) {
	f.retrieved++
	f.ids = append(f.ids, id)
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing optional lookup deadline")
	}
	return f.user, f.err
}

func TestNotionProbeUsersToken(t *testing.T) {
	verified := notionmeetings.User{Object: "user", ID: "member", Person: notionmeetings.UserPerson{Email: "member@example.com", EmailVerified: true}}
	for _, tc := range []struct {
		name        string
		user        *notionmeetings.User
		err         error
		want        string
		noAttendees bool
	}{
		{"available", &verified, nil, "Users token: verified email available for sampled attendee", false},
		{"no emails", &notionmeetings.User{Object: "user", ID: "member"}, nil, "Users token: no verified email for sampled attendee", false},
		{"missing capability", nil, notionmeetings.ErrUserInformation, "Users token: unavailable", false},
		{"request timeout", nil, fmt.Errorf("perform Notion request: %w", context.DeadlineExceeded), "Users token: unavailable", false},
		{"no attendees", nil, nil, "Users token: untested", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			users := &fakeNotionUsersProbe{user: tc.user, err: tc.err}
			var out bytes.Buffer
			probe := fakeNotionProbe{result: &notionmeetings.QueryResult{Results: []notionmeetings.MeetingNote{{ID: "meeting-1", Parent: notionmeetings.Parent{PageID: "page-1"}, MeetingNotes: notionmeetings.MeetingNotesData{CalendarEvent: notionmeetings.MeetingCalendarEvent{Attendees: []string{"member"}}}}}}, block: notionProbeBlock(t, notionmeetings.MeetingNote{ID: "meeting-1", MeetingNotes: notionmeetings.MeetingNotesData{CalendarEvent: notionmeetings.MeetingCalendarEvent{Attendees: []string{"member"}}}}), usersErr: notionmeetings.ErrUnauthorized}
			expectedRequests := 1
			if tc.noAttendees {
				probe.result.Results = nil
				expectedRequests = 0
			}
			err := runNotionMeetingsProbe(t.Context(), &out, probe, users)
			require.NoError(t, err)
			assert.NotContains(out.String(), "unless a users token is configured")
			assert.Contains(out.String(), tc.want)
			assert.NotContains(out.String(), "member@example.com")
			assert.Equal(expectedRequests, users.retrieved)
		})
	}
}

func TestScheduledNotionSyncUsesUsersToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(notionmeetings.SourceType, "work")
	require.NoError(err)
	meetingFactory, usersFactory, rebuild := newNotionMeetingsClient, newNotionUsersClient, rebuildNotionMeetingsCacheAfterScheduledSync
	t.Cleanup(func() {
		newNotionMeetingsClient, newNotionUsersClient, rebuildNotionMeetingsCacheAfterScheduledSync = meetingFactory, usersFactory, rebuild
	})
	newNotionMeetingsClient = func(_ string, token string) notionmeetings.Source {
		assert.Equal("pat-example", token)
		return fakeNotionProbe{
			result:   &notionmeetings.QueryResult{Results: []notionmeetings.MeetingNote{{Object: "block", ID: "meeting-1", Type: "meeting_notes", Parent: notionmeetings.Parent{PageID: "page-1"}, CreatedTime: "2026-08-29T10:00:00Z", MeetingNotes: notionmeetings.MeetingNotesData{Status: "notes_ready", CalendarEvent: notionmeetings.MeetingCalendarEvent{Attendees: []string{"member"}}}}}},
			block:    notionProbeBlock(t, notionmeetings.MeetingNote{ID: "meeting-1", CreatedTime: "2026-08-29T10:00:00Z", MeetingNotes: notionmeetings.MeetingNotesData{Status: "notes_ready", CalendarEvent: notionmeetings.MeetingCalendarEvent{Attendees: []string{"member"}}}}),
			usersErr: notionmeetings.ErrUnauthorized,
		}
	}
	users := &fakeNotionUsersProbe{user: &notionmeetings.User{ID: "member", Person: notionmeetings.UserPerson{Email: "member@example.com", EmailVerified: true}}}
	newNotionUsersClient = func(_ string, token string) notionmeetings.UserSource {
		assert.Equal("ntn-example", token)
		return users
	}
	rebuildNotionMeetingsCacheAfterScheduledSync = func(context.Context, string) error { return nil }
	err = runConfiguredNotionMeetingsSync(t.Context(), st, config.NotionMeetingsSource{Identifier: "work", AccountEmail: "owner@example.com", Token: "pat-example", UsersToken: "ntn-example"})
	require.NoError(err)
	assert.Equal(1, users.retrieved)
	_, absent := configuredNotionClients(config.NotionMeetingsSource{Token: "pat-example"})
	assert.Nil(absent)
}

func TestNotionProbeHTTPBlockOwnedHydration(t *testing.T) {
	assert := assert.New(t)

	var markdownCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/blocks/meeting_notes/query":
			_, err := io.WriteString(w, `{"results":[{"object":"block","id":"private-meeting","type":"meeting_notes","has_children":false,"meeting_notes":{"children":{"transcript_block_id":"stale-transcript"}}}],"has_more":true}`)
			assert.NoError(err)
		case "/v1/blocks/private-meeting":
			_, err := io.WriteString(w, `{"object":"block","id":"private-meeting","type":"meeting_notes","has_children":false,"parent":{"type":"data_source_id","data_source_id":"private-rows"},"meeting_notes":{"title":[{"plain_text":"PRIVATE_TITLE"}],"children":{"transcript_block_id":"own-transcript"}}}`)
			assert.NoError(err)
		case "/v1/blocks/own-transcript":
			_, err := io.WriteString(w, `{"object":"block","id":"own-transcript","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"PRIVATE_TRANSCRIPT"}]}}`)
			assert.NoError(err)
		case "/v1/users":
			w.WriteHeader(http.StatusForbidden)
			_, err := io.WriteString(w, `{"object":"error","code":"restricted_resource"}`)
			assert.NoError(err)
		default:
			markdownCalls++
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	var out bytes.Buffer
	err := runNotionMeetingsProbe(t.Context(), &out, notionmeetings.NewClient(server.URL, "synthetic-token"), nil)
	require.NoError(t, err)
	assert.Zero(markdownCalls)
	assert.Contains(out.String(), "Credentials: query verified")
	assert.Contains(out.String(), "Meeting query: available")
	assert.Contains(out.String(), "Read Content: available")
	assert.Contains(out.String(), "User Information: unavailable")
	assert.NotContains(out.String(), "Partial coverage: true")
	for _, private := range []string{"PRIVATE_TITLE", "PRIVATE_TRANSCRIPT", "private-meeting", "private-rows", "synthetic-token", "stale-transcript"} {
		assert.NotContains(out.String(), private)
	}
}

func TestNotionProbeErrorsArePrivate(t *testing.T) {
	assert := assert.New(t)

	cause := errors.New("GET https://example.test/PRIVATE_ID?cursor=PRIVATE_CURSOR failed")
	var out bytes.Buffer
	err := runNotionMeetingsProbe(t.Context(), &out, fakeNotionProbe{err: cause}, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, cause)
	assert.NotContains(err.Error(), "PRIVATE_ID")
	assert.NotContains(err.Error(), "PRIVATE_CURSOR")
}

func TestNotionProbeSamplesOnlyRefreshedAttendee(t *testing.T) {
	assert := assert.New(t)

	probe := fakeNotionProbe{result: &notionmeetings.QueryResult{Results: []notionmeetings.MeetingNote{{ID: "meeting-1", MeetingNotes: notionmeetings.MeetingNotesData{CalendarEvent: notionmeetings.MeetingCalendarEvent{Attendees: []string{"old-attendee"}}}}}, HasMore: true}, block: notionProbeBlock(t, notionmeetings.MeetingNote{ID: "meeting-1", MeetingNotes: notionmeetings.MeetingNotesData{CalendarEvent: notionmeetings.MeetingCalendarEvent{Attendees: []string{"new-attendee", "another-attendee"}}}})}
	users := &fakeNotionUsersProbe{user: &notionmeetings.User{ID: "new-attendee"}}
	var out bytes.Buffer
	require.NoError(t, runNotionMeetingsProbe(t.Context(), &out, probe, users))
	assert.Equal([]string{"new-attendee"}, users.ids)
	assert.NotContains(out.String(), "old-attendee")
	assert.NotContains(out.String(), "new-attendee")
}

func TestNotionMeetingsSummaryReportsActualCoverage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		legacy  bool
		hasMore bool
	}{
		{name: "retained legacy without query cap", legacy: true},
		{name: "query cap", hasMore: true},
		{name: "complete query and content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			transcript := "Meeting-owned transcript."
			meeting := notionmeetings.MeetingNote{
				Object: "block", Type: "meeting_notes", ID: "meeting-1", CreatedTime: "2026-11-07T10:00:00Z",
				MeetingNotes: notionmeetings.MeetingNotesData{Status: "notes_ready", Children: notionmeetings.MeetingChildren{TranscriptBlockID: "transcript-1"}},
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/blocks/meeting_notes/query":
					assert.NoError(json.NewEncoder(w).Encode(map[string]any{"results": []notionmeetings.MeetingNote{meeting}, "has_more": tc.hasMore}))
				case "/v1/blocks/meeting-1":
					assert.NoError(json.NewEncoder(w).Encode(meeting))
				case "/v1/blocks/transcript-1":
					assert.NoError(json.NewEncoder(w).Encode(map[string]any{
						"object": "block", "id": "transcript-1", "type": "paragraph", "has_children": false,
						"paragraph": map[string]any{"rich_text": []notionmeetings.RichText{{PlainText: transcript}}},
					}))
				case "/v1/users":
					_, err := io.WriteString(w, `{"object":"list","results":[],"has_more":false}`)
					assert.NoError(err)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			st := testutil.NewTestStore(t)
			_, err := st.GetOrCreateSource(notionmeetings.SourceType, "synthetic")
			require.NoError(err)
			importer := notionmeetings.NewImporter(st, notionmeetings.NewClient(server.URL, "synthetic-token"))
			summary, err := importer.Import(t.Context(), notionmeetings.ImportOptions{Identifier: "synthetic"})
			require.NoError(err)
			if tc.legacy {
				var messageID int64
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
				raw, err := st.GetMessageRaw(messageID)
				require.NoError(err)
				var legacy map[string]any
				require.NoError(json.Unmarshal(raw, &legacy))
				legacy["schema_version"] = 1
				legacy["page_markdown"] = map[string]any{"object": "page_markdown", "id": "page-1", "markdown": "Legacy parent evidence"}
				raw, err = json.Marshal(legacy)
				require.NoError(err)
				require.NoError(st.UpsertMessageRawWithFormat(messageID, raw, notionmeetings.RawFormat))
				transcript = ""
				summary, err = importer.Import(t.Context(), notionmeetings.ImportOptions{Identifier: "synthetic"})
				require.NoError(err)
				assert.Zero(summary.MeetingsUpdated, "the real import retained the unscoped legacy evidence")
			}
			wantPartial := tc.legacy || tc.hasMore
			assert.Equal(wantPartial, summary.PartialCoverage)
			var out bytes.Buffer
			writeNotionMeetingsSummary(&out, summary)
			if wantPartial {
				assert.Contains(out.String(), "Coverage: partial (some meetings or sections remain incomplete)")
			} else {
				assert.NotContains(out.String(), "Coverage: partial")
			}
			assert.NotContains(out.String(), "Notion reports more than")
		})
	}
}
