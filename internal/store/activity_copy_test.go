package store_test

import (
	"fmt"
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
			countedID := f.add(t, countedRoom, countedSource.ID, contact.Now, false, "shared@example.com")
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

// Both copies have contact, but only the sent copy counts for the requested
// addressee. The lower source is a received copy in another owned account.
func TestActivityEmailCopiesPreferRequestedSubject(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved=%t", saved), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newTimelineActivityFixture(t)
			contact := timelineContactQuery(f)
			if saved {
				p, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				require.NoError(err)
				contact.Subject = store.ActivitySubject{PersonID: p.ID}
			}
			owner2 := f.EnsureParticipant("second@example.com", "Second owner", "example.com")
			sender := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
			source, err := f.Store.GetOrCreateSource("gmail", "second@example.com")
			require.NoError(err)
			require.NoError(f.Store.AddAccountIdentity(source.ID, "second@example.com", "manual"))
			room, err := f.Store.EnsureConversationWithType(source.ID, "sent-copy", "email_thread", "Sent copy")
			require.NoError(err)
			received := f.add(t, f.ConvID, f.Source.ID, contact.Now.Add(-time.Hour), false, "<subject-copy@example.com>")
			require.NoError(f.Store.ReplaceMessageRecipients(received, "from", []int64{sender}, nil))
			require.NoError(f.Store.ReplaceMessageRecipients(received, "to", []int64{f.owner, f.peer}, nil))
			sent := f.add(t, room, source.ID, contact.Now, true, "subject-copy@example.com")
			require.NoError(f.Store.ReplaceMessageRecipients(sent, "from", []int64{owner2}, nil))
			projectTimelineActivity(t, f, "UTC")
			fullQuery := timelineBurstQuery()
			fullQuery.Filters.IncludeNoise = true
			full, err := f.Store.ListActivityBurstsContext(t.Context(), fullQuery)
			require.NoError(err)
			require.Len(full.Bursts, 1)
			assert.Equal(received, full.Bursts[0].AnchorMessageID)
			for _, scope := range []struct {
				sources       []int64
				count, copies int
				id            int64
			}{{nil, 1, 2, sent}, {[]int64{source.ID}, 1, 1, sent}, {[]int64{f.Source.ID}, 0, 1, received}} {
				ctx := t.Context()
				if scope.sources != nil {
					ctx = sourcescope.WithSourceIDs(ctx, scope.sources)
				}
				aggregate, err := f.Store.MessageContactAggregateContext(ctx, contact)
				require.NoError(err)
				assert.Equal(scope.count, aggregate.Count)
				if scope.count == 1 {
					require.NotNil(aggregate.LastAt)
					assert.True(contact.Now.Equal(*aggregate.LastAt))
				}
				q := timelineBurstQuery()
				q.Subject = contact.Subject
				q.Filters.IncludeNoise = true
				page, err := f.Store.ListActivityBurstsContext(ctx, q)
				require.NoError(err)
				require.Len(page.Bursts, 1)
				assert.Equal(scope.id, page.Bursts[0].AnchorMessageID)
				assert.Equal(scope.copies, page.Bursts[0].CopyCounts[scope.id])
			}
		})
	}
}

func TestActivityEmailCopiesCanonicalMessageIDs(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
		count               int
	}{
		{"clean brackets", "<copy@example.com>", "copy@example.com", 1},
		{"distinct IDs", "first@example.com", "second@example.com", 2},
		{"missing IDs", "", "", 2},
		{"empty brackets", "<>", "", 2},
		{"inner whitespace", "< copy@example.com >", "copy@example.com", 2},
		{"outer whitespace", " <copy@example.com> ", "copy@example.com", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineActivityFixture(t)
			contact := timelineContactQuery(f)
			source, err := f.Store.GetOrCreateSource("gmail", "second@example.com")
			requirements.NoError(err)
			requirements.NoError(f.Store.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
			room, err := f.Store.EnsureConversationWithType(source.ID, "canonical-copy", "email_thread", "Copy")
			requirements.NoError(err)
			f.add(t, f.ConvID, f.Source.ID, contact.Now, false, tc.first)
			f.add(t, room, source.ID, contact.Now, false, tc.second)
			projectTimelineActivity(t, f, "UTC")
			aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), contact)
			requirements.NoError(err)
			assertions.Equal(tc.count, aggregate.Count)
			page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
			requirements.NoError(err)
			assertions.Len(page.Bursts, tc.count)
			for _, burst := range page.Bursts {
				assertions.Equal(1, burst.Inbound)
				assertions.Equal(3-tc.count, burst.CopyCounts[burst.AnchorMessageID])
			}
			for _, sourceID := range []int64{f.Source.ID, source.ID} {
				ctx := sourcescope.WithSourceIDs(t.Context(), []int64{sourceID})
				aggregate, err := f.Store.MessageContactAggregateContext(ctx, contact)
				requirements.NoError(err)
				assertions.Equal(1, aggregate.Count)
				page, err := f.Store.ListActivityBurstsContext(ctx, timelineBurstQuery())
				requirements.NoError(err)
				requirements.Len(page.Bursts, 1)
				assertions.Equal(1, page.Bursts[0].CopyCounts[page.Bursts[0].AnchorMessageID])
			}
		})
	}
}
