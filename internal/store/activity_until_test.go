package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
)

func TestBurstsUntilBoundsMessagesBeforeGrouping(t *testing.T) {
	for _, cold := range []bool{false, true} {
		name := "warm"
		if cold {
			name = "cold"
		}
		t.Run(name, func(t *testing.T) {
			f := newColdTimelineActivityFixture(t)
			room := coldTimelineRoom(t, f, f.Source.ID, "history")
			if !cold {
				setTimelineConversationTier(t, f, f.Source.ID, "history", conversationpolicy.Full)
			}
			now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			addColdTimelineMessage(t, f, room, f.Source.ID, now.Add(-time.Hour), false)
			anchor := addColdTimelineMessage(t, f, room, f.Source.ID, now, true)
			addColdTimelineMessage(t, f, room, f.Source.ID, now.Add(time.Minute), false)
			projectColdTimeline(t, f, "UTC")
			q := store.BurstQuery{After: new(time.Time), Until: &now, Limit: 10}
			read := f.Store.ListActivityBurstsContext
			if cold {
				read = f.Store.ListColdActivityBurstsContext
			}
			got, err := read(t.Context(), q)
			require.NoError(t, err)
			require.Len(t, got.Bursts, 1)
			assert.WithinDuration(t, now, got.Bursts[0].LastAt, time.Microsecond)
			assert.Equal(t, 1, got.Bursts[0].Inbound)
			assert.Equal(t, 1, got.Bursts[0].Outbound)
			assert.Equal(t, anchor, got.Bursts[0].AnchorMessageID)
		})
	}
}
