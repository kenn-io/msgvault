package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOverlappingMailboxThreadsShareBurstAndInitiation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTimelineActivityFixture(t)
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	f.add(t, f.ConvID, f.Source.ID, at, false, "<a@example.com>")
	f.add(t, f.ConvID, f.Source.ID, at.Add(time.Hour), true, "<b@example.com>")
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(other.ID, "owner@example.com", "manual"))
	conv, err := f.Store.EnsureConversation(other.ID, "overlap", "Overlapping thread")
	require.NoError(err)
	f.add(t, conv, other.ID, at.Add(time.Hour), true, "<b@example.com>")
	f.add(t, conv, other.ID, at.Add(2*time.Hour), false, "<c@example.com>")
	projectTimelineActivity(t, f, "UTC")
	page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	require.NoError(err)
	assert.Len(page.Bursts, 1)
	contact := timelineContactQuery(f)
	aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), contact)
	require.NoError(err)
	assert.Equal(3, aggregate.Count)
	assert.Equal(1, aggregate.InitiatedByContact)
	assert.Zero(aggregate.InitiatedByOwner)
}
