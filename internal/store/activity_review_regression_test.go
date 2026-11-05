package store_test

import (
	"fmt"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCleartextMarkerIsOrdinaryMessage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	src, err := f.Store.GetOrCreateSource("matrix", "@owner:example.com")
	requirements.NoError(err)
	requirements.NoError(f.Store.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
	conv, err := f.Store.EnsureConversation(src.ID, "!room:example.com", "Room")
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='direct_chat' WHERE id=?`), conv)
	requirements.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	var ids []int64
	for i := range 3 {
		id := f.add(t, conv, src.ID, tierAsOf.AddDate(0, 0, i-3), i == 1, "")
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='matrix',snippet=? WHERE id=?`), store.MatrixEncryptedPlaceholder, id)
		requirements.NoError(err)
		ids = append(ids, id)
	}
	projectTimelineActivity(t, f, "UTC")
	explanation, err := f.Store.ExplainPersonTierContext(t.Context(), person.ID, tierOptions())
	requirements.NoError(err)
	assertions.Equal(2, explanation.Tier)
	assertions.Equal(3, explanation.Signals.ActiveDaysLong)
	display, err := f.Store.MessageDisplayContext(t.Context(), ids)
	requirements.NoError(err)
	for _, id := range ids {
		assertions.False(display[id].Encrypted)
		assertions.Equal(store.MatrixEncryptedPlaceholder, display[id].Snippet)
	}
	q := timelineContactQuery(f)
	q.Now = tierAsOf
	aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(3, aggregate.Count)
	bursts, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	requirements.NoError(err)
	assertions.Len(bursts.Bursts, 3)
}

func TestMissingIDCannotCollideWithRealID(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	id := f.add(t, f.ConvID, f.Source.ID, q.Now.Add(-time.Hour), false, "")
	f.add(t, f.ConvID, f.Source.ID, q.Now, true, fmt.Sprintf("id:%d", id))
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(2, got.Count)
	bursts, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	requirements.NoError(err)
	requirements.Len(bursts.Bursts, 1)
	assertions.Equal(1, bursts.Bursts[0].Inbound)
	assertions.Equal(1, bursts.Bursts[0].Outbound)
}

func TestPromotionPreservesLinkedContactCounts(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	alias := f.EnsureParticipant("noreply@example.com", "Alias", "example.com")
	_, err := f.Store.LinkParticipants(f.peer, alias)
	requirements.NoError(err)
	q := timelineContactQuery(f)
	q.Subject.ParticipantIDs = []int64{f.peer, alias}
	f.peer = alias
	f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
	projectTimelineActivity(t, f, "UTC")
	before, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(1, before.Count)
	person, _, err := f.Store.CreatePersonFromParticipant(alias)
	requirements.NoError(err)
	projectTimelineActivity(t, f, "UTC")
	q.Subject = store.ActivitySubject{PersonID: person.ID}
	after, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(before, after)
	bursts, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	requirements.NoError(err)
	requirements.Len(bursts.Bursts, 1)
	assertions.Equal(1, bursts.Bursts[0].Inbound)
}

func TestMonthlyCountsSkippedMidnight(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	loc, err := time.LoadLocation("America/Asuncion")
	requirements.NoError(err)
	q := timelineContactQuery(f)
	q.Location = loc
	q.Now = time.Date(2023, 10, 22, 12, 0, 0, 0, loc)
	// October starts at 01:00 after a midnight clock jump. September's
	// final hour and October's first hour belong to separate buckets.
	f.add(t, f.ConvID, f.Source.ID, time.Date(2023, 10, 1, 3, 30, 0, 0, time.UTC), false, "")
	f.add(t, f.ConvID, f.Source.ID, time.Date(2023, 10, 1, 4, 30, 0, 0, time.UTC), true, "")
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal([12]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1}, got.Monthly)
}
