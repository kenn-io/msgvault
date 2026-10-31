package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestActivityMessagesConversationKeyset(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	ids := []int64{}
	for i := range 501 {
		ids = append(ids, f.add(t, f.ConvID, f.Source.ID, at.Add(time.Duration(i/2)*time.Second), i%2 == 1, ""))
	}
	projectTimelineActivity(t, f, "UTC")
	for _, desc := range []bool{false, true} {
		q := store.MessageQuery{ConversationID: f.ConvID, Descending: desc, Limit: 500}
		got, err := f.Store.ListActivityMessagesContext(t.Context(), q)
		requirements.NoError(err)
		requirements.Len(got, 500)
		last := got[len(got)-1]
		q.AfterKey = &store.ActivityKey{OccurredAt: last.OccurredAt, MessageID: last.MessageID}
		next, err := f.Store.ListActivityMessagesContext(t.Context(), q)
		requirements.NoError(err)
		requirements.Len(next, 1)
		if desc {
			assertions.Equal(ids[0], next[0].MessageID)
		} else {
			assertions.Equal(ids[500], next[0].MessageID)
		}
	}
}
func TestLatestEligibleMessageSkipsDrafts(t *testing.T) {
	f := newTimelineActivityFixture(t)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	want := f.add(t, f.ConvID, f.Source.ID, at, false, "")
	draft := f.add(t, f.ConvID, f.Source.ID, at.Add(time.Hour), true, "")
	projectTimelineActivity(t, f, "UTC")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET is_sent=FALSE WHERE id=?`), draft)
	require.NoError(t, err)
	got, err := f.Store.LatestEligibleMessageContext(t.Context(), f.ConvID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
