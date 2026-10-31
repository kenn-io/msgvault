package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestLiveEligibilityExcludesChangesAfterProjection(t *testing.T) {
	for _, change := range []string{"is_sent=FALSE", "deleted_at=CURRENT_TIMESTAMP", "deleted_from_source_at=CURRENT_TIMESTAMP", `message_type='calendar_event',metadata='{"status":"cancelled"}'`} {
		t.Run(change, func(t *testing.T) {
			f := newTimelineActivityFixture(t)
			now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			id := f.add(t, f.ConvID, f.Source.ID, now, false, "")
			transcript := f.add(t, f.ConvID, f.Source.ID, now.Add(-time.Minute), false, "")
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='meeting_transcript' WHERE id=?`), transcript)
			require.NoError(t, err)
			projectTimelineActivity(t, f, "UTC")
			q := timelineContactQuery(f)
			attended, _, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 10)
			require.NoError(t, err)
			assert.Equal(t, []int64{transcript}, attended)
			summary, err := f.Store.MessageContactAggregateContext(t.Context(), q)
			require.NoError(t, err)
			assert.Equal(t, 1, summary.Count)
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET `+change+` WHERE id IN (?,?)`), id, transcript)
			require.NoError(t, err)
			bursts, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
			require.NoError(t, err)
			assert.Empty(t, bursts.Bursts)
			messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}})
			require.NoError(t, err)
			assert.Empty(t, messages)
			summary, err = f.Store.MessageContactAggregateContext(t.Context(), q)
			require.NoError(t, err)
			assert.Zero(t, summary.Count)
			attended, _, err = f.Store.AttendedTranscriptIDsContext(t.Context(), q, 10)
			require.NoError(t, err)
			assert.Empty(t, attended)
			latest, err := f.Store.LatestEligibleMessageContext(t.Context(), f.ConvID)
			require.NoError(t, err)
			assert.Zero(t, latest)
			display, err := f.Store.MessageDisplayContext(t.Context(), []int64{id, transcript})
			require.NoError(t, err)
			assert.Empty(t, display)
		})
	}
}
