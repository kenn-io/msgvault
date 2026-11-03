package store_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestActivityReadersUseInstantForOffsetBounds(t *testing.T) {
	for _, offset := range []int{-8, 8} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineActivityFixture(t)
			at := timelineContactQuery(f).Now
			id := f.add(t, f.ConvID, f.Source.ID, at, false, "")
			projectTimelineActivity(t, f, "UTC")
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
				projectTimelineActivity(t, f, "UTC")
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

func TestLegacyNullMessageTypeRemainsVisibleToActivityContactAndAnchorReaders(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, sourceID, conversationID := newLegacyNullableMessageTypeStore(t)
	peer, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	requirements.NoError(err)
	requirements.NoError(st.AddAccountIdentity(sourceID, storetest.OwnerAddress, "manual"))
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	id := persistLegacyLoopEmail(t, st, sourceID, conversationID, "legacy-activity-null", "alice@example.test", storetest.OwnerAddress, "A synthetic note", at)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET message_type=NULL WHERE id=?`), id)
	requirements.NoError(err)
	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", BatchSize: 10000})
	requirements.NoError(err)
	_, err = projector.RunOnce(t.Context())
	requirements.NoError(err)

	messages, err := st.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: conversationID, Limit: 10})
	requirements.NoError(err)
	requirements.Len(messages, 1)
	assertions.Equal(id, messages[0].MessageID)
	aggregate, err := st.MessageContactAggregateContext(t.Context(), store.ContactQuery{Subject: store.ActivitySubject{ParticipantIDs: []int64{peer}}, Now: at, Location: time.UTC})
	requirements.NoError(err)
	assertions.Equal(1, aggregate.Count)
	anchor, err := st.LatestEligibleMessageContext(t.Context(), conversationID)
	requirements.NoError(err)
	assertions.Equal(id, anchor)
	display, err := st.MessageDisplayContext(t.Context(), []int64{anchor})
	requirements.NoError(err)
	requirements.Contains(display, id)
	assertions.Empty(display[id].MessageType)
}
