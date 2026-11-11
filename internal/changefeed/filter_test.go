package changefeed

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveFilterTracksNewPersonLinks(t *testing.T) {
	st := testutil.NewTestStore(t)
	first, err := st.EnsureParticipant("first@example.com", "First", "example.com")
	require.NoError(t, err)
	second, err := st.EnsureParticipant("second@example.com", "Second", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(first)
	require.NoError(t, err)
	filter, err := ResolveFilter(t.Context(), st, Filter{PersonID: person.ID})
	require.NoError(t, err)
	assert.True(t, filter.Matches(store.ChangedMessage{SenderID: first}))
	assert.False(t, filter.Matches(store.ChangedMessage{SenderID: second}))
	_, err = st.LinkParticipants(first, second)
	require.NoError(t, err)
	filter, err = ResolveFilter(t.Context(), st, Filter{PersonID: person.ID})
	require.NoError(t, err)
	assert.True(t, filter.Matches(store.ChangedMessage{SenderID: second}))
	_, err = ResolveFilter(t.Context(), st, Filter{PersonID: 99999999})
	require.Error(t, err)
}
func TestScanStopsAtBudgetAndResumes(t *testing.T) {
	at := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	page := store.ChangedMessagePage{ServerTime: at.Add(time.Hour), CompleteThrough: at.Add(time.Minute)}
	for i := range 2001 {
		page.Messages = append(page.Messages, store.ChangedMessage{ID: int64(i), ContentChangedAt: at, SourceID: 1})
	}
	rows, next, more := Scan(page, store.ChangedMessagesFrom(at), Filter{SourceIDs: []int64{2}}, 2000)
	assert.Empty(t, rows)
	assert.True(t, more)
	id, ok := next.AfterID()
	require.True(t, ok)
	assert.Equal(t, int64(1999), id)
}
