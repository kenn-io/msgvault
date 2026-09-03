package cmd

import (
	"bytes"
	"context"
	"errors"
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

func TestResolveNotionMeetingsSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	cfg = &config.Config{NotionMeetings: []config.NotionMeetingsSource{
		{Identifier: "personal", Token: "secret-1"},
		{Identifier: "work", Token: "secret-2"},
	}}

	_, err := resolveNotionMeetingsSource(nil)
	require.Error(err)
	assert.Contains(err.Error(), "multiple [[notion_meetings]]")

	source, err := resolveNotionMeetingsSource([]string{"work"})
	require.NoError(err)
	assert.Equal("work", source.Identifier)
}

func TestResolveNotionMeetingsSourcesRequiresProbeIdentifierForMultipleSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	cfg = &config.Config{NotionMeetings: []config.NotionMeetingsSource{
		{Identifier: "personal", Token: "secret-1"},
		{Identifier: "work", Token: "secret-2"},
	}}

	_, err := resolveNotionMeetingsSources(nil, true)
	require.Error(err)
	assert.Contains(err.Error(), "multiple [[notion_meetings]]")

	sources, err := resolveNotionMeetingsSources([]string{"work"}, true)
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
	})
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
	})
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
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "block endpoint unavailable")
}

func TestRunNotionMeetingsProbeDegradesWithoutUserInformation(t *testing.T) {
	var out bytes.Buffer
	err := runNotionMeetingsProbe(context.Background(), &out, fakeNotionProbe{
		result: &notionmeetings.QueryResult{}, usersErr: notionmeetings.ErrUserInformation,
	})
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
	})
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
			})
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
