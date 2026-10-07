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
	users   []notionmeetings.User
	listErr error
	listed  int
}

func (f *fakeNotionUsersProbe) ListUsers(context.Context, string) (*notionmeetings.UserPage, error) {
	f.listed++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &notionmeetings.UserPage{Results: f.users}, nil
}

func (f *fakeNotionUsersProbe) RetrieveUser(context.Context, string) (*notionmeetings.User, error) {
	return nil, notionmeetings.ErrUserInformation
}

func TestNotionProbeUsersToken(t *testing.T) {
	verified := notionmeetings.User{Object: "user", ID: "member", Person: notionmeetings.UserPerson{Email: "member@example.com", EmailVerified: true}}
	for _, tc := range []struct {
		name  string
		users []notionmeetings.User
		err   error
		want  string
	}{
		{"available", []notionmeetings.User{verified}, nil, "Users token: available"},
		{"no emails", []notionmeetings.User{{Object: "user", ID: "member"}}, nil, "Users token: no verified emails"},
		{"missing capability", nil, notionmeetings.ErrUserInformation, "Users token: unavailable"},
		{"request timeout", nil, fmt.Errorf("perform Notion request: %w", context.DeadlineExceeded), "Users token: unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			users := &fakeNotionUsersProbe{users: tc.users, listErr: tc.err}
			var out bytes.Buffer
			err := runNotionMeetingsProbe(t.Context(), &out, fakeNotionProbe{result: &notionmeetings.QueryResult{}, usersErr: notionmeetings.ErrUnauthorized}, users)
			require.NoError(t, err)
			assert.NotContains(out.String(), "unless a users token is configured")
			assert.Contains(out.String(), tc.want)
			assert.NotContains(out.String(), "member@example.com")
			assert.Equal(1, users.listed)
		})
	}
}

func TestConfiguredNotionClientsKeepCredentialsSeparate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	meetingFactory, usersFactory := newNotionMeetingsClient, newNotionUsersClient
	t.Cleanup(func() { newNotionMeetingsClient, newNotionUsersClient = meetingFactory, usersFactory })
	var meetingToken, usersToken string
	newNotionMeetingsClient = func(_ string, token string) notionmeetings.Source { meetingToken = token; return nil }
	newNotionUsersClient = func(_ string, token string) notionmeetings.UserSource {
		usersToken = token
		return &fakeNotionUsersProbe{}
	}
	_, users := configuredNotionClients(config.NotionMeetingsSource{Token: "pat-example", UsersToken: "internal-example"})
	require.NotNil(users)
	assert.Equal("pat-example", meetingToken)
	assert.Equal("internal-example", usersToken)
	_, users = configuredNotionClients(config.NotionMeetingsSource{Token: "pat-example"})
	assert.Nil(users)
}

type scheduledNotionSource struct {
	notionmeetings.Source
}

func (s scheduledNotionSource) QueryMeetingNotes(context.Context, int) (*notionmeetings.QueryResult, error) {
	return &notionmeetings.QueryResult{Results: []notionmeetings.MeetingNote{{
		Object: "block", ID: "meeting-1", Type: "meeting_notes", Parent: notionmeetings.Parent{PageID: "page-1"},
		CreatedTime:  "2026-08-29T10:00:00Z",
		MeetingNotes: notionmeetings.MeetingNotesData{Status: "notes_ready", CalendarEvent: notionmeetings.MeetingCalendarEvent{Attendees: []string{"member"}}},
	}}}, nil
}

func (s scheduledNotionSource) RetrieveBlock(context.Context, string) (*notionmeetings.Block, error) {
	return &notionmeetings.Block{Object: "block", ID: "meeting-1", Type: "meeting_notes"}, nil
}

func (s scheduledNotionSource) RetrievePageMarkdown(context.Context, string, bool) (*notionmeetings.MarkdownPage, error) {
	return &notionmeetings.MarkdownPage{Markdown: "# Transcript\nSpeaker: Scheduled meeting."}, nil
}

func (s scheduledNotionSource) ListUsers(context.Context, string) (*notionmeetings.UserPage, error) {
	return nil, notionmeetings.ErrUnauthorized
}

func TestScheduledNotionSyncUsesUsersToken(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(notionmeetings.SourceType, "work")
	require.NoError(t, err)
	meetingFactory, usersFactory, rebuild := newNotionMeetingsClient, newNotionUsersClient, rebuildNotionMeetingsCacheAfterScheduledSync
	t.Cleanup(func() {
		newNotionMeetingsClient, newNotionUsersClient, rebuildNotionMeetingsCacheAfterScheduledSync = meetingFactory, usersFactory, rebuild
	})
	newNotionMeetingsClient = func(_ string, token string) notionmeetings.Source {
		assert.Equal(t, "pat-example", token)
		return scheduledNotionSource{}
	}
	users := &fakeNotionUsersProbe{users: []notionmeetings.User{{ID: "member", Person: notionmeetings.UserPerson{Email: "member@example.com", EmailVerified: true}}}}
	newNotionUsersClient = func(_ string, token string) notionmeetings.UserSource {
		assert.Equal(t, "ntn-example", token)
		return users
	}
	var refreshes int
	rebuildNotionMeetingsCacheAfterScheduledSync = func(context.Context, string) error { refreshes++; return nil }
	err = runConfiguredNotionMeetingsSync(t.Context(), st, config.NotionMeetingsSource{Identifier: "work", AccountEmail: "owner@example.com", Token: "pat-example", UsersToken: "ntn-example"})
	require.NoError(t, err)
	assert.Equal(t, 1, users.listed)
	assert.Equal(t, 1, refreshes)
	var messageID int64
	require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'meeting-1'`).Scan(&messageID))
	recipients, err := st.GetMessageRecipientsContext(t.Context(), messageID, "to")
	require.NoError(t, err)
	require.Len(t, recipients, 1)
	assert.Equal(t, "member@example.com", recipients[0].EmailAddress)
	body, err := st.GetMessageBodyText(messageID)
	require.NoError(t, err)
	assert.Contains(t, body, "Speaker: Scheduled meeting.")
}
