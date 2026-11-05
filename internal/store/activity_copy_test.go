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

func TestActivityEmailCopiesKeepCountedExchangeInOneMailbox(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTimelineActivityFixture(t)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	workOwner := f.EnsureParticipant("work@example.com", "Work owner", "example.com")
	work, err := f.Store.GetOrCreateSource("gmail", "work@example.com")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(work.ID, "work@example.com", "manual"))
	require.Less(f.Source.ID, work.ID)
	workThread, err := f.Store.EnsureConversationWithType(work.ID, "exchange", "email_thread", "Exchange")
	require.NoError(err)
	for _, mailbox := range []struct {
		source, conversation int64
		outbound             bool
	}{{f.Source.ID, f.ConvID, false}, {work.ID, workThread, true}} {
		sent := f.add(t, mailbox.conversation, mailbox.source, at, mailbox.outbound, "<exchange-start@example.com>")
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sender_id=? WHERE id=?`), workOwner, sent)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(sent, "from", []int64{workOwner}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(sent, "to", []int64{f.peer}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(sent, "cc", []int64{f.owner}, nil))
		reply := f.add(t, mailbox.conversation, mailbox.source, at.Add(time.Hour), false, "<exchange-reply@example.com>")
		require.NoError(f.Store.ReplaceMessageRecipients(reply, "to", []int64{workOwner, f.owner}, nil))
	}
	projectTierSignals(t, f.Fixture, "UTC", 0)
	page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	require.NoError(err)
	require.Len(page.Bursts, 1)
	assert.Equal(work.ID, page.Bursts[0].SourceID)
	assert.Equal(workThread, page.Bursts[0].ConversationID)
	assert.Equal(1, page.Bursts[0].Inbound)
	assert.Equal(1, page.Bursts[0].Outbound)
}

func TestActivityEmailCopyWinnerStableWhenEndDateWidens(t *testing.T) {
	requirements := require.New(t)
	f := newTimelineActivityFixture(t)
	day1 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	workOwner := f.EnsureParticipant("work@example.com", "Work owner", "example.com")
	work, err := f.Store.GetOrCreateSource("gmail", "work@example.com")
	requirements.NoError(err)
	requirements.NoError(f.Store.AddAccountIdentity(work.ID, "work@example.com", "manual"))
	workThread, err := f.Store.EnsureConversationWithType(work.ID, "exchange", "email_thread", "Exchange")
	requirements.NoError(err)
	for _, mailbox := range []struct {
		source, conversation int64
		work                 bool
	}{{f.Source.ID, f.ConvID, false}, {work.ID, workThread, true}} {
		sent := f.add(t, mailbox.conversation, mailbox.source, day1, mailbox.work, "<exchange-start@example.com>")
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sender_id=? WHERE id=?`), workOwner, sent)
		requirements.NoError(err)
		requirements.NoError(f.Store.ReplaceMessageRecipients(sent, "from", []int64{workOwner}, nil))
		requirements.NoError(f.Store.ReplaceMessageRecipients(sent, "to", []int64{f.peer}, nil))
		requirements.NoError(f.Store.ReplaceMessageRecipients(sent, "cc", []int64{f.owner}, nil))
		reply := f.add(t, mailbox.conversation, mailbox.source, day1.Add(time.Hour), false, "<exchange-reply@example.com>")
		requirements.NoError(f.Store.ReplaceMessageRecipients(reply, "to", []int64{workOwner, f.owner}, nil))
		// Both threads have four messages, but only the personal mailbox counts
		// these later sent messages. They must not change the day-one winner.
		for i := range 2 {
			id := f.add(t, mailbox.conversation, mailbox.source, day1.AddDate(0, 0, 2).Add(time.Duration(i)*time.Hour), !mailbox.work, fmt.Sprintf("<personal-sent-%d@example.com>", i))
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sender_id=? WHERE id=?`), f.owner, id)
			requirements.NoError(err)
			requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{f.owner}, nil))
			requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer}, nil))
			requirements.NoError(f.Store.ReplaceMessageRecipients(id, "cc", []int64{workOwner}, nil))
		}
	}
	projectTierSignals(t, f.Fixture, "UTC", 0)
	for _, to := range []string{"2026-10-22", "2026-10-24"} {
		t.Run(to, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			page, err := f.Store.ListActivityBurstsContext(t.Context(), store.BurstQuery{FromDate: "2026-10-22", ToDate: to, Limit: 10})
			require.NoError(err)
			var day1Sources []int64
			for _, burst := range page.Bursts {
				if burst.LocalDate == "2026-10-22" {
					day1Sources = append(day1Sources, burst.SourceID)
					assert.Equal(1, burst.Inbound)
					assert.Equal(1, burst.Outbound)
				}
			}
			assert.Equal([]int64{work.ID}, day1Sources)
		})
	}
}

// A lower-ID observed copy must not suppress the owned copy's real contact.
func TestActivityEmailCopiesPreferCountedContact(t *testing.T) {
	for _, tc := range []struct {
		invitation, largeAudience bool
	}{{false, false}, {true, false}, {true, true}} {
		t.Run(fmt.Sprintf("counted tie/invitation=%t/largeAudience=%t", tc.invitation, tc.largeAudience), func(t *testing.T) {
			invitation := tc.invitation
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineActivityFixture(t)
			at := timelineContactQuery(f).Now
			var ids []int64
			var rooms []int64
			for i := range 2 {
				source, err := f.Store.GetOrCreateSource("gmail", fmt.Sprintf("archive-%d@example.com", i))
				requirements.NoError(err)
				requirements.NoError(f.Store.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
				room, err := f.Store.EnsureConversationWithType(source.ID, "copy", "email_thread", "Copy")
				requirements.NoError(err)
				id := f.add(t, room, source.ID, at, !invitation, "<display-copy@example.com>")
				ids = append(ids, id)
				rooms = append(rooms, room)
				if tc.largeAudience {
					recipients := []int64{f.owner}
					for j := range 26 {
						recipients = append(recipients, f.EnsureParticipant(fmt.Sprintf("guest-%d@example.com", j), "Guest", "example.com"))
					}
					requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", recipients, nil))
				} else if invitation {
					_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='list.example.com' WHERE id=?`), id)
					requirements.NoError(err)
				} else if i == 0 {
					sender := f.EnsureParticipant("noreply@example.com", "Automated sender", "example.com")
					requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{f.owner, sender}, nil))
					_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_is_from_me=FALSE WHERE id=?`), id)
					requirements.NoError(err)
				}
			}
			if invitation {
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO attachments(message_id,filename,mime_type,storage_path) VALUES(?,'invite.ics','application/octet-stream','invite.ics')`), ids[1])
				requirements.NoError(err)
			}
			projectTierSignals(t, f.Fixture, "UTC", 0)
			for _, room := range rooms {
				messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: room, Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}})
				requirements.NoError(err)
				requirements.Len(messages, 1)
				counted := false
				for _, counterpart := range messages[0].Counterparts {
					counted = counted || counterpart.Counted
				}
				assertions.Equal(!invitation, counted)
			}
			page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
			requirements.NoError(err)
			requirements.Len(page.Bursts, 1)
			assertions.Equal(ids[1], page.Bursts[0].AnchorMessageID)
			assertions.Equal(2, page.Bursts[0].CopyCounts[ids[1]])
			assertions.Zero(page.Bursts[0].Inbound)
			if invitation {
				assertions.Zero(page.Bursts[0].Outbound)
			} else {
				assertions.Equal(1, page.Bursts[0].Outbound)
			}
			assertions.Zero(page.NoiseOmitted)
		})
	}
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
			projectTierSignals(t, f.Fixture, "UTC", 0)
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
					assertions.Equal(1-scope.count, quiet.NoiseOmitted)
				})
			}
		})
	}
}

// The same sender is received in one account and owns the sending account.
// Only the sent copy counts for the requested addressee.
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
			source, err := f.Store.GetOrCreateSource("gmail", "second@example.com")
			require.NoError(err)
			require.NoError(f.Store.AddAccountIdentity(source.ID, "second@example.com", "manual"))
			room, err := f.Store.EnsureConversationWithType(source.ID, "sent-copy", "email_thread", "Sent copy")
			require.NoError(err)
			received := f.add(t, f.ConvID, f.Source.ID, contact.Now.Add(-time.Hour), false, "<subject-copy@example.com>")
			require.NoError(f.Store.ReplaceMessageRecipients(received, "from", []int64{owner2}, nil))
			require.NoError(f.Store.ReplaceMessageRecipients(received, "to", []int64{f.owner, f.peer}, nil))
			sent := f.add(t, room, source.ID, contact.Now, true, "subject-copy@example.com")
			require.NoError(f.Store.ReplaceMessageRecipients(sent, "from", []int64{owner2}, nil))
			require.NoError(f.Store.ReplaceMessageRecipients(sent, "to", []int64{f.owner, f.peer}, nil))
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sender_id=? WHERE id IN (?,?)`), owner2, received, sent)
			require.NoError(err)
			projectTierSignals(t, f.Fixture, "UTC", 0)
			fullQuery := timelineBurstQuery()
			fullQuery.Filters.IncludeNoise = true
			full, err := f.Store.ListActivityBurstsContext(t.Context(), fullQuery)
			require.NoError(err)
			require.Len(full.Bursts, 1)
			assert.Equal(sent, full.Bursts[0].AnchorMessageID)
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
		{"missing IDs", "", "", 2},
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
			firstID := f.add(t, f.ConvID, f.Source.ID, contact.Now, false, tc.first)
			f.add(t, room, source.ID, contact.Now, false, tc.second)
			projectTierSignals(t, f.Fixture, "UTC", 0)
			aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), contact)
			requirements.NoError(err)
			assertions.Equal(tc.count, aggregate.Count)
			page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
			requirements.NoError(err)
			assertions.Len(page.Bursts, tc.count)
			for _, burst := range page.Bursts {
				if tc.count == 1 {
					assertions.Equal(firstID, burst.AnchorMessageID)
				}
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
				assertions.Equal(sourceID, page.Bursts[0].SourceID)
				assertions.Equal(1, page.Bursts[0].CopyCounts[page.Bursts[0].AnchorMessageID])
			}
		})
	}
}
