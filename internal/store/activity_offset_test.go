package store_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestActivityReadersUseInstantForOffsetBounds(t *testing.T) {
	for _, offset := range []int{-8, 8} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineActivityFixture(t)
			at := timelineContactQuery(f).Now
			id := f.add(t, f.ConvID, f.Source.ID, at, false, "")
			projectTierSignals(t, f.Fixture, "UTC", 0)
			zone := time.FixedZone("offset", offset*3600)
			after, before := at.Add(-time.Hour).In(zone), at.Add(time.Hour).In(zone)
			messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, After: &after, Before: &before, Limit: 10})
			requirements.NoError(err)
			assertions.Len(messages, 1)
			page, err := f.Store.ListActivityBurstsContext(t.Context(), store.BurstQuery{After: &after, Before: &before, Limit: 10})
			requirements.NoError(err)
			assertions.Len(page.Bursts, 1)
			if len(messages) > 0 {
				assertions.Equal(id, messages[0].MessageID)
			}
		})
	}
}

func TestContactReadersUseInstantForOffsetClock(t *testing.T) {
	for _, transcript := range []bool{false, true} {
		for _, hours := range []int{-1, 1} {
			t.Run(strconv.Itoa(hours)+"/transcript="+strconv.FormatBool(transcript), func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				f := newTimelineActivityFixture(t)
				q := timelineContactQuery(f)
				id := f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
				if transcript {
					_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='meeting_transcript' WHERE id=?`), id)
					requirements.NoError(err)
				}
				projectTierSignals(t, f.Fixture, "UTC", 0)
				// Earlier clock has a later wall time, and later clock an earlier one.
				q.Now = q.Now.Add(time.Duration(hours) * time.Hour).In(time.FixedZone("offset", -hours*8*3600))
				want := 0
				if hours > 0 {
					want = 1
				}
				if transcript {
					ids, _, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 10)
					requirements.NoError(err)
					assertions.Len(ids, want)
				} else {
					got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
					requirements.NoError(err)
					assertions.Equal(want, got.Count)
				}
			})
		}
	}
}
