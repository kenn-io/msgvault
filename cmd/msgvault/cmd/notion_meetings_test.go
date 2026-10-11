package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	err := runNotionMeetingsProbe(context.Background(), &out, fakeNotionProbe{
		result: &notionmeetings.QueryResult{Results: []notionmeetings.MeetingNote{meeting}, HasMore: true},
		block:  &notionmeetings.Block{Parent: notionmeetings.Parent{PageID: "private-page-id"}},
	}, nil)
	require.NoError(t, err)
	assert.Contains(out.String(), "Returned meetings: 1")
	assert.Contains(out.String(), "Partial coverage: true")
	assert.NotContains(out.String(), "private-block-id")
	assert.NotContains(out.String(), "Private meeting title")
	assert.NotContains(out.String(), "secret-token")
	assert.NotContains(out.String(), "private transcript")
	assert.Contains(out.String(), "Read Content: available")
	assert.Contains(out.String(), "User Information: available")
}

func TestRunNotionMeetingsProbeResolvesParentFromMeetingBlock(t *testing.T) {
	var out bytes.Buffer
	err := runNotionMeetingsProbe(context.Background(), &out, fakeNotionProbe{
		result: &notionmeetings.QueryResult{Results: []notionmeetings.MeetingNote{{ID: "meeting-1"}}},
		block:  &notionmeetings.Block{Parent: notionmeetings.Parent{PageID: "page-1"}},
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Read Content: available")
}

func TestRunNotionMeetingsProbeChecksBlockWhenQueryHasParent(t *testing.T) {
	var out bytes.Buffer
	err := runNotionMeetingsProbe(context.Background(), &out, fakeNotionProbe{
		result: &notionmeetings.QueryResult{Results: []notionmeetings.MeetingNote{{
			ID: "meeting-1", Parent: notionmeetings.Parent{PageID: "page-1"},
		}}},
		blockErr: errors.New("block endpoint unavailable"),
	}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "block endpoint unavailable")
}

func TestRunNotionMeetingsProbeDegradesWithoutUserInformation(t *testing.T) {
	var out bytes.Buffer
	err := runNotionMeetingsProbe(context.Background(), &out, fakeNotionProbe{
		result: &notionmeetings.QueryResult{}, usersErr: notionmeetings.ErrUserInformation,
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Read Content: not tested")
	assert.Contains(t, out.String(), "User Information: unavailable")
}

func TestRunNotionMeetingsProbeDegradesOnTransientUserListingFailure(t *testing.T) {
	var out bytes.Buffer
	err := runNotionMeetingsProbe(context.Background(), &out, fakeNotionProbe{
		result: &notionmeetings.QueryResult{}, usersErr: &notionmeetings.APIError{
			Kind: notionmeetings.ErrRateLimited, Status: 503, Code: "service_unavailable",
		},
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "User Information: unavailable")
	assert.Contains(t, out.String(), "retry budget exhausted")
}

func TestRunNotionMeetingsProbeSurfacesSystemicUserListingFailures(t *testing.T) {
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
			err := runNotionMeetingsProbe(context.Background(), &out, fakeNotionProbe{
				result: &notionmeetings.QueryResult{}, usersErr: tt.err,
			}, nil)
			require.Error(t, err)
			require.ErrorIs(t, err, tt.err)
		})
	}
}

func TestRunConfiguredNotionMeetingsSyncRefusesRemovedSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	err := runConfiguredNotionMeetingsSync(context.Background(), st, config.NotionMeetingsSource{
		Identifier: "removed", Token: "secret-token",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add-notion-meetings removed")
}

type fakeNotionUsersProbe struct {
	user      *notionmeetings.User
	err       error
	retrieved int
}

func (f *fakeNotionUsersProbe) RetrieveUser(ctx context.Context, id string) (*notionmeetings.User, error) {
	f.retrieved++
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
			probe := fakeNotionProbe{result: &notionmeetings.QueryResult{Results: []notionmeetings.MeetingNote{{ID: "meeting-1", Parent: notionmeetings.Parent{PageID: "page-1"}, MeetingNotes: notionmeetings.MeetingNotesData{CalendarEvent: notionmeetings.MeetingCalendarEvent{Attendees: []string{"member"}}}}}}, block: &notionmeetings.Block{}, usersErr: notionmeetings.ErrUnauthorized}
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
			block:    &notionmeetings.Block{Object: "block", ID: "meeting-1", Type: "meeting_notes"},
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
