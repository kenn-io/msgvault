package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/store"
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

func TestResolveNotionMeetingsSource(t *testing.T) {
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

	_, err := resolveNotionMeetingsSource(nil, cfg)
	require.Error(err)
	assert.Contains(err.Error(), "multiple [[notion_meetings]]")

	source, err := resolveNotionMeetingsSource([]string{"work"}, cfg)
	require.NoError(err)
	assert.Equal("work", source.Identifier)
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

func TestFinishNotionMeetingsImportRefreshesCommittedWritesOnFailure(t *testing.T) {
	refreshed := 0
	err := finishNotionMeetingsImport("work", &notionmeetings.ImportSummary{MeetingsAdded: 1},
		errors.New("hydrate failed"), func() error { refreshed++; return nil })
	require.Error(t, err)
	assert.Equal(t, 1, refreshed)
	assert.Contains(t, err.Error(), "notion meetings sync work failed")
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
			users := &fakeNotionUsersProbe{users: tc.users, listErr: tc.err}
			var out bytes.Buffer
			err := runNotionMeetingsProbe(t.Context(), &out, fakeNotionProbe{result: &notionmeetings.QueryResult{}, usersErr: notionmeetings.ErrUserInformation}, users)
			require.NoError(t, err)
			assert.Contains(t, out.String(), "unless a users token is configured")
			assert.Contains(t, out.String(), tc.want)
			assert.NotContains(t, out.String(), "member@example.com")
			assert.Equal(t, 1, users.listed)
		})
	}
}

func TestConfiguredNotionClientsKeepCredentialsSeparate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	meetingFactory, usersFactory := newNotionMeetingsClient, newNotionUsersClient
	t.Cleanup(func() { newNotionMeetingsClient, newNotionUsersClient = meetingFactory, usersFactory })
	t.Setenv("MSGVAULT_TEST_NOTION_USERS_TOKEN", "internal-example")
	var meetingToken, usersToken string
	newNotionMeetingsClient = func(_ string, token string) notionmeetings.Source { meetingToken = token; return nil }
	newNotionUsersClient = func(_ string, token string) notionmeetings.UserSource {
		usersToken = token
		return &fakeNotionUsersProbe{}
	}
	_, users, err := configuredNotionClients(config.NotionMeetingsSource{Token: "pat-example", UsersTokenEnv: "MSGVAULT_TEST_NOTION_USERS_TOKEN"})
	require.NoError(err)
	require.NotNil(users)
	assert.Equal("pat-example", meetingToken)
	assert.Equal("internal-example", usersToken)
	_, users, err = configuredNotionClients(config.NotionMeetingsSource{Token: "pat-example"})
	require.NoError(err)
	assert.Nil(users)
}

type countingNotionSyncSource struct {
	notionmeetings.Source

	queries int
}

func (s *countingNotionSyncSource) QueryMeetingNotes(context.Context, int) (*notionmeetings.QueryResult, error) {
	s.queries++
	return &notionmeetings.QueryResult{}, nil
}

func TestNotionSyncValidatesAllUserCredentialsBeforeRequests(t *testing.T) {
	for _, credential := range []string{"missing env", "missing file", "both references"} {
		t.Run(credential, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			home := t.TempDir()
			cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, NotionMeetings: []config.NotionMeetingsSource{
				{Identifier: "first", Token: "example-meeting-token", AccountEmail: "user@example.com"},
				{Identifier: "later", Token: "example-meeting-token", AccountEmail: "user@example.com"},
			}}
			t.Setenv("MSGVAULT_TEST_NOTION_MISSING_USERS_TOKEN", "")
			switch credential {
			case "missing env":
				cfg.NotionMeetings[1].UsersTokenEnv = "MSGVAULT_TEST_NOTION_MISSING_USERS_TOKEN"
			case "missing file":
				cfg.NotionMeetings[1].UsersTokenFile = filepath.Join(home, "missing-users-token")
			case "both references":
				cfg.NotionMeetings[1].UsersTokenEnv = "MSGVAULT_TEST_NOTION_MISSING_USERS_TOKEN"
				cfg.NotionMeetings[1].UsersTokenFile = filepath.Join(home, "missing-users-token")
			}
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(err)
			t.Cleanup(func() { _ = st.Close() })
			require.NoError(st.InitSchema())
			_, err = st.GetOrCreateSource(notionmeetings.SourceType, "first")
			require.NoError(err)

			t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			factory, rebuild := newNotionMeetingsClient, rebuildNotionMeetingsCacheAfterWrite
			probe, full, limit, after := syncNotionMeetingsProbe, syncNotionMeetingsFull, syncNotionMeetingsLimit, syncNotionMeetingsAfter
			t.Cleanup(func() {
				newNotionMeetingsClient, rebuildNotionMeetingsCacheAfterWrite = factory, rebuild
				syncNotionMeetingsProbe, syncNotionMeetingsFull, syncNotionMeetingsLimit, syncNotionMeetingsAfter = probe, full, limit, after
			})
			syncNotionMeetingsProbe, syncNotionMeetingsFull, syncNotionMeetingsLimit, syncNotionMeetingsAfter = false, false, 0, ""
			client := &countingNotionSyncSource{}
			newNotionMeetingsClient = func(string, string) notionmeetings.Source { return client }
			var out bytes.Buffer
			cmd := &cobra.Command{Use: syncNotionMeetingsCmd.Use, RunE: syncNotionMeetingsCmd.RunE}
			cmd.SetOut(&out)
			cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			err = cmd.RunE(cmd, nil)
			require.Error(err)
			assert.Contains(err.Error(), "users_token_")
			assert.Zero(client.queries, "a later invalid credential must prevent all Notion requests")
			assert.Empty(out.String(), "no source should start syncing before credential validation completes")
		})
	}
}
