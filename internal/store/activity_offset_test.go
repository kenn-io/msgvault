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
	for _, transcript := range []bool{false, true} {
		for _, tc := range []struct{ offset, hours, want int }{{-8, 1, 1}, {8, -1, 0}} {
			t.Run(strconv.Itoa(tc.offset)+"/transcript="+strconv.FormatBool(transcript), func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				f := newTimelineActivityFixture(t)
				at := timelineContactQuery(f).Now
				id := f.add(t, f.ConvID, f.Source.ID, at, false, "")
				if transcript {
					_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='meeting_transcript' WHERE id=?`), id)
					requirements.NoError(err)
				}
				projectTierSignals(t, f.Fixture, "UTC", 0)
				zone := time.FixedZone("offset", tc.offset*3600)
				after, before := at.Add(-time.Hour).In(zone), at.Add(time.Hour).In(zone)
				messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, After: &after, Before: &before, Limit: 10})
				requirements.NoError(err)
				requirements.Len(messages, 1)
				page, err := f.Store.ListActivityBurstsContext(t.Context(), store.BurstQuery{After: &after, Before: &before, Limit: 10})
				requirements.NoError(err)
				assertions.Len(page.Bursts, 1)
				assertions.Equal(id, messages[0].MessageID)
				q := timelineContactQuery(f)
				// Earlier clock has a later wall time, and later clock an earlier one.
				q.Now = q.Now.Add(time.Duration(tc.hours) * time.Hour).In(time.FixedZone("offset", -tc.hours*8*3600))
				if !transcript {
					got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
					requirements.NoError(err)
					assertions.Equal(tc.want, got.Count)
				}
			})
		}
	}
}
