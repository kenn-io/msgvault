package gcal

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMockAPI_FullPaginationDeliversSyncTokenOnFinalPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := NewMockAPI()
	m.Calendars = []Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]Event{
		{{ID: "a"}, {ID: "b"}},
		{{ID: "c"}},
	}
	m.FullSyncToken["primary"] = "TOK1"

	ctx := context.Background()
	p0, err := m.ListEvents(ctx, "primary", EventsListParams{})
	require.NoError(err)
	assert.Len(p0.Items, 2)
	assert.Equal("1", p0.NextPageToken)
	assert.Empty(p0.NextSyncToken, "no sync token before the final page")

	p1, err := m.ListEvents(ctx, "primary", EventsListParams{PageToken: p0.NextPageToken})
	require.NoError(err)
	assert.Len(p1.Items, 1)
	assert.Empty(p1.NextPageToken)
	assert.Equal("TOK1", p1.NextSyncToken)

	// Re-running the full sync (PageToken="") restarts pagination — needed for
	// idempotency tests.
	again, err := m.ListEvents(ctx, "primary", EventsListParams{})
	require.NoError(err)
	assert.Len(again.Items, 2)
	assert.Equal(3, m.ListEventsCalls())
}

func TestMockAPI_IncrementalAndGone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := NewMockAPI()
	m.IncEvents["TOK1"] = [][]Event{{{ID: "x", Status: StatusCancelled, RecurringEventID: "r"}}}
	m.IncNextToken["TOK1"] = "TOK2"
	m.GoneTokens["STALE"] = true

	ctx := context.Background()
	page, err := m.ListEvents(ctx, "primary", EventsListParams{SyncToken: "TOK1"})
	require.NoError(err)
	require.Len(page.Items, 1)
	assert.True(page.Items[0].IsCancelled())
	assert.Equal("TOK2", page.NextSyncToken)

	_, err = m.ListEvents(ctx, "primary", EventsListParams{SyncToken: "STALE"})
	var gone *GoneError
	assert.ErrorAs(err, &gone, "stale token should yield *GoneError")
}

func TestMockAPI_GetEventAndErrorInjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := NewMockAPI()
	m.EventsByID["primary"] = map[string]Event{"e1": {ID: "e1", Summary: "Found"}}

	ev, err := m.GetEvent(context.Background(), "primary", "e1")
	require.NoError(err)
	assert.Equal("Found", ev.Summary)

	_, err = m.GetEvent(context.Background(), "primary", "nope")
	var nf *NotFoundError
	require.ErrorAs(err, &nf)

	m.ListEventsErr = errors.New("boom")
	_, err = m.ListEvents(context.Background(), "primary", EventsListParams{})
	assert.ErrorContains(err, "boom")
}
