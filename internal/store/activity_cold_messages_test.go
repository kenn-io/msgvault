package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

// Individual cold history must not be reconstructed from the latest-five burst
// preview. Its keyset keeps equal-time messages and applies scope before reads.
func TestColdActivityMessagesKeysetScopeAndGroupNoise(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "cold-messages")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='email_thread' WHERE id=?`), room)
	requirements.NoError(err)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	ids := []int64{}
	for i := range 8 {
		copyID := ""
		if i == 0 {
			copyID = "<cold-copy@example.com>"
		}
		id := f.add(t, room, f.Source.ID, at.Add(time.Duration(i/2)*time.Second), false, copyID)
		ids = append(ids, id)
		if i < 2 {
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='updates.example.com' WHERE id=?`), id)
			requirements.NoError(err)
		}
	}
	countedCopyID := f.add(t, room, f.Source.ID, at, false, "<cold-copy@example.com>")
	for i := range 2 {
		id := f.add(t, room, f.Source.ID, at.Add(24*time.Hour+time.Duration(i)*time.Second), false, "")
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='updates.example.com' WHERE id=?`), id)
		requirements.NoError(err)
	}
	for _, change := range []string{"is_sent=FALSE", "message_type='call_record'"} {
		id := f.add(t, room, f.Source.ID, at.Add(10*time.Second), false, "")
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET `+change+` WHERE id=?`), id)
		requirements.NoError(err)
	}
	hidden, err := f.Store.GetOrCreateSource("gmail", "hidden-cold@example.com")
	requirements.NoError(err)
	requirements.NoError(f.Store.AddAccountIdentity(hidden.ID, "owner@example.com", "manual"))
	hiddenRoom := coldTimelineRoom(t, f, hidden.ID, "hidden-messages")
	f.add(t, hiddenRoom, hidden.ID, at, false, "")
	before := at.Add(48 * time.Hour)
	base := store.MessageQuery{ConversationID: room, After: &at, Before: &before, Limit: 2}
	for _, descending := range []bool{false, true} {
		q := base
		q.Descending = descending
		gotIDs := []int64{}
		seen := map[int64]bool{}
		for pageNumber := range 10 {
			page, err := f.Store.ListColdActivityMessagesContext(t.Context(), q)
			requirements.NoError(err)
			requirements.LessOrEqual(len(page), 2)
			if len(page) == 0 {
				break
			}
			for _, m := range page {
				assertions.False(seen[m.MessageID], "duplicate message %d", m.MessageID)
				seen[m.MessageID] = true
				gotIDs = append(gotIDs, m.MessageID)
			}
			last := page[len(page)-1]
			q.AfterKey = &store.ActivityKey{OccurredAt: last.OccurredAt, MessageID: last.MessageID}
			requirements.Less(pageNumber, 9, "keyset failed to finish")
		}
		want := slices.Clone(ids)
		want[0] = countedCopyID // The non-list copy is the counted representative.
		if descending {
			slices.Reverse(want)
		}
		assertions.Equal(want, gotIDs)
	}
	q := base
	q.Limit = 500
	q.Filters.IncludeNoise = true
	all, err := f.Store.ListColdActivityMessagesContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Len(all, 10, "noise applies to the whole conversation-day, not each listed message")
	for _, allowed := range [][]int64{{}, {hidden.ID}} {
		scoped, err := f.Store.ListColdActivityMessagesContext(sourcescope.WithSourceIDs(t.Context(), allowed), q)
		requirements.NoError(err)
		assertions.Empty(scoped)
	}
	q.ConversationID = hiddenRoom
	hiddenRows, err := f.Store.ListColdActivityMessagesContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(hiddenRows, 1)
	hiddenRows, err = f.Store.ListColdActivityMessagesContext(sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID}), q)
	requirements.NoError(err)
	assertions.Empty(hiddenRows)
	for _, invalid := range []store.MessageQuery{{ConversationID: room, Limit: 0}, {ConversationID: room, Limit: 501}, {Limit: 1}} {
		_, err = f.Store.ListColdActivityMessagesContext(t.Context(), invalid)
		assertions.ErrorIs(err, store.ErrInvalidActivityRequest)
	}
}
