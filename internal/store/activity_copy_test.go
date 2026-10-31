package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

// A lower-ID observed copy must not suppress the owned copy's real contact.
func TestActivityEmailCopiesPreferCountedContact(t *testing.T) {
	for _, person := range []struct {
		name  string
		saved bool
	}{{"unsaved", false}, {"saved", true}} {
		t.Run(person.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineActivityFixture(t)
			contact := timelineContactQuery(f)
			if person.saved {
				profile, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				requirements.NoError(err)
				contact.Subject = store.ActivitySubject{PersonID: profile.ID}
			}
			observedSource, err := f.Store.GetOrCreateSource("mbox", "archive@example.com")
			requirements.NoError(err)
			countedSource, err := f.Store.GetOrCreateSource("gmail", "owned@example.com")
			requirements.NoError(err)
			requirements.NoError(f.Store.AddAccountIdentity(countedSource.ID, "owner@example.com", "manual"))
			requirements.Less(observedSource.ID, countedSource.ID)
			observedRoom, err := f.Store.EnsureConversationWithType(observedSource.ID, "observed-copy", "email_thread", "Observed copy")
			requirements.NoError(err)
			countedRoom, err := f.Store.EnsureConversationWithType(countedSource.ID, "counted-copy", "email_thread", "Counted copy")
			requirements.NoError(err)
			observedAt := contact.Now.Add(-time.Hour)
			observedID := f.add(t, observedRoom, observedSource.ID, observedAt, false, "<shared@example.com>")
			countedID := f.add(t, countedRoom, countedSource.ID, contact.Now, false, "<shared@example.com>")
			requirements.Less(observedID, countedID)
			projectTimelineActivity(t, f, "UTC")
			for _, precondition := range []struct {
				room      int64
				direction store.ActivityDirection
			}{{observedRoom, store.DirectionObserved}, {countedRoom, store.DirectionInbound}} {
				messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: precondition.room, Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}})
				requirements.NoError(err)
				requirements.Len(messages, 1)
				assertions.Equal(precondition.direction, messages[0].Direction)
			}
			for _, scope := range []struct {
				name            string
				sources         []int64
				message, source int64
				count, copies   int
				at              time.Time
			}{
				{"all sources", nil, countedID, countedSource.ID, 1, 2, contact.Now},
				{"owned only", []int64{countedSource.ID}, countedID, countedSource.ID, 1, 1, contact.Now},
				{"observed only", []int64{observedSource.ID}, observedID, observedSource.ID, 0, 1, observedAt},
				{"none", []int64{}, 0, 0, 0, 0, time.Time{}},
			} {
				t.Run(scope.name, func(t *testing.T) {
					assertions, requirements := assert.New(t), require.New(t)
					ctx := t.Context()
					if scope.sources != nil {
						ctx = sourcescope.WithSourceIDs(ctx, scope.sources)
					}
					q := timelineBurstQuery()
					q.Subject = contact.Subject
					q.Filters.IncludeNoise = true
					page, err := f.Store.ListActivityBurstsContext(ctx, q)
					requirements.NoError(err)
					aggregate, err := f.Store.MessageContactAggregateContext(ctx, contact)
					requirements.NoError(err)
					if scope.count == 0 {
						assertions.Zero(aggregate.Count)
						assertions.Nil(aggregate.FirstAt)
						assertions.Nil(aggregate.LastAt)
						assertions.Nil(aggregate.LastInboundAt)
					} else {
						want := store.ContactAggregate{Count: 1, FirstAt: &contact.Now, LastAt: &contact.Now, LastInboundAt: &contact.Now,
							Channels: []store.ActivityChannel{store.ChannelEmail}, Monthly: [12]int{11: 1}, InitiatedByContact: 1}
						assertions.Equal(want, aggregate)
					}
					if scope.message == 0 {
						assertions.Empty(page.Bursts)
						return
					}
					requirements.Len(page.Bursts, 1)
					burst := page.Bursts[0]
					assertions.Equal(scope.message, burst.AnchorMessageID)
					assertions.Equal(scope.source, burst.SourceID)
					assertions.Equal([]int64{scope.message}, burst.LatestMessageIDs)
					assertions.Equal(scope.copies, burst.CopyCounts[scope.message])
					assertions.Equal(scope.count, burst.Inbound)
					assertions.Zero(burst.Outbound)
					assertions.True(scope.at.Equal(burst.FirstAt))
					assertions.True(scope.at.Equal(burst.LastAt))
					foundPeer := false
					for _, counterpart := range burst.Counterparts {
						if counterpart.ParticipantID == f.peer {
							foundPeer = true
							assertions.Equal(scope.count == 1, counterpart.Counted)
						}
					}
					assertions.True(foundPeer)
					q.Filters.IncludeNoise = false
					quiet, err := f.Store.ListActivityBurstsContext(ctx, q)
					requirements.NoError(err)
					assertions.Len(quiet.Bursts, scope.count)
				})
			}
		})
	}
}
