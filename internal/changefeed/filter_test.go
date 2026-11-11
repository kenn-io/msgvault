package changefeed

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveFilterTracksNewPersonLinks(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	first, err := st.EnsureParticipant("first@example.com", "First", "example.com")
	require.NoError(err)
	second, err := st.EnsureParticipant("second@example.com", "Second", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(first)
	require.NoError(err)
	filter, err := ResolveFilter(t.Context(), st, Filter{PersonID: person.ID})
	require.NoError(err)
	assert.True(filter.Matches(store.ChangedMessage{SenderID: first}))
	assert.False(filter.Matches(store.ChangedMessage{SenderID: second}))
	_, err = st.LinkParticipants(first, second)
	require.NoError(err)
	filter, err = ResolveFilter(t.Context(), st, Filter{PersonID: person.ID})
	require.NoError(err)
	assert.True(filter.Matches(store.ChangedMessage{SenderID: second}))
	_, err = ResolveFilter(t.Context(), st, Filter{PersonID: 99999999})
	require.Error(err)
}
func TestScanStopsAtBudgetAndResumes(t *testing.T) {
	assert := Assert.New(t)

	at := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	page := store.ChangedMessagePage{ServerTime: at.Add(time.Hour), CompleteThrough: at.Add(time.Minute)}
	for i := range 2001 {
		page.Messages = append(page.Messages, store.ChangedMessage{ID: int64(i), ContentChangedAt: at, SourceID: 1})
	}
	rows, next, more := Scan(page, store.ChangedMessagesFrom(at), Filter{SourceIDs: []int64{2}}, 2000)
	assert.Empty(rows)
	assert.True(more)
	id, ok := next.AfterID()
	Require.True(t, ok)
	assert.Equal(int64(1999), id)
}
