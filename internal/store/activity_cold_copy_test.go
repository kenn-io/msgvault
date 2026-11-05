package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func TestColdEmailCopiesPreferNonNoiseWithoutCountedContact(t *testing.T) {
	for _, noise := range []string{"list", "excluded-sender"} {
		t.Run(noise, func(t *testing.T) {
			requirements := require.New(t)
			f := newTimelineActivityFixture(t)
			noisySource, err := f.Store.GetOrCreateSource("mbox", "noisy@example.com")
			requirements.NoError(err)
			cleanSource, err := f.Store.GetOrCreateSource("mbox", "clean@example.com")
			requirements.NoError(err)
			requirements.Less(noisySource.ID, cleanSource.ID)
			noisyRoom, err := f.Store.EnsureConversationWithType(noisySource.ID, "noisy-copy", "email_thread", "Noisy copy")
			requirements.NoError(err)
			cleanRoom, err := f.Store.EnsureConversationWithType(cleanSource.ID, "clean-copy", "email_thread", "Clean copy")
			requirements.NoError(err)
			at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			noisyID := f.add(t, noisyRoom, noisySource.ID, at, false, "<quiet-copy@example.com>")
			cleanID := f.add(t, cleanRoom, cleanSource.ID, at, false, "<quiet-copy@example.com>")
			if noise == "list" {
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='list.example.com' WHERE id=?`), noisyID)
				requirements.NoError(err)
			} else {
				sender := f.EnsureParticipant("no-reply@example.com", "Automated sender", "example.com")
				requirements.NoError(f.Store.ReplaceMessageRecipients(noisyID, "from", []int64{sender}, nil))
			}
			setTimelineConversationTier(t, f, noisySource.ID, "noisy-copy", conversationpolicy.Cold)
			setTimelineConversationTier(t, f, cleanSource.ID, "clean-copy", conversationpolicy.Cold)
			projectColdTimeline(t, f, "UTC")
			retractTimelineActivity(t, f, noisyID)
			retractTimelineActivity(t, f, cleanID)
			for _, scope := range []struct {
				name    string
				sources []int64
				id      int64
				copies  int
				omitted int
			}{
				{"clean only", []int64{cleanSource.ID}, cleanID, 1, 0},
				{"noisy only", []int64{noisySource.ID}, noisyID, 1, 1},
				{"both", []int64{noisySource.ID, cleanSource.ID}, cleanID, 2, 0},
			} {
				t.Run(scope.name, func(t *testing.T) {
					assertions, requirements := assert.New(t), require.New(t)
					ctx := sourcescope.WithSourceIDs(t.Context(), scope.sources)
					q := timelineBurstQuery()
					q.Filters.Exclusions = persontiers.DefaultConfig()
					q.Filters.IncludeNoise = true
					full, err := f.Store.ListColdActivityBurstsContext(ctx, q)
					requirements.NoError(err)
					requirements.Len(full.Bursts, 1)
					burst := full.Bursts[0]
					assertions.Equal(scope.id, burst.AnchorMessageID)
					assertions.Equal(scope.copies, burst.CopyCounts[scope.id])
					assertions.Zero(burst.Inbound)
					assertions.Zero(burst.Outbound)
					requirements.NotEmpty(burst.Counterparts)
					for _, cp := range burst.Counterparts {
						assertions.False(cp.Counted)
					}
					q.Filters.IncludeNoise = false
					filtered, err := f.Store.ListColdActivityBurstsContext(ctx, q)
					requirements.NoError(err)
					assertions.Len(filtered.Bursts, 1-scope.omitted)
					assertions.Equal(scope.omitted, filtered.NoiseOmitted)
					count, err := f.Store.CountColdActivityMessagesContext(ctx, q)
					requirements.NoError(err)
					assertions.Equal(1-scope.omitted, count)
				})
			}
		})
	}
}

// Collapsing copies before classification and hydration loses owned contact.
func TestColdEmailCopiesPreferCountedContact(t *testing.T) {
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
			projectColdTimeline(t, f, "UTC")
			for _, precondition := range []struct {
				room      int64
				direction store.ActivityDirection
			}{{observedRoom, store.DirectionObserved}, {countedRoom, store.DirectionInbound}} {
				messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: precondition.room, Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}})
				requirements.NoError(err)
				requirements.Len(messages, 1)
				assertions.Equal(precondition.direction, messages[0].Direction)
			}
			setTimelineConversationTier(t, f, observedSource.ID, "observed-copy", conversationpolicy.Cold)
			setTimelineConversationTier(t, f, countedSource.ID, "counted-copy", conversationpolicy.Cold)
			projectColdTimeline(t, f, "UTC")
			retractTimelineActivity(t, f, observedID)
			retractTimelineActivity(t, f, countedID)
			spine, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
			requirements.NoError(err)
			assertions.Empty(spine.Bursts)
			for _, maxDirect := range []int{0, 1} {
				t.Run(fmt.Sprintf("limit=%d", maxDirect), func(t *testing.T) {
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
							q.Filters = store.ActivityFilters{IncludeNoise: true, MaxDirect: maxDirect}
							page, err := f.Store.ListColdActivityBurstsContext(ctx, q)
							requirements.NoError(err)
							count, err := f.Store.CountColdActivityMessagesContext(ctx, q)
							requirements.NoError(err)
							if scope.message == 0 {
								assertions.Empty(page.Bursts)
								assertions.Zero(count)
								return
							}
							assertions.Equal(1, count)
							requirements.Len(page.Bursts, 1)
							burst := page.Bursts[0]
							assertions.True(burst.Cold)
							assertions.Equal(store.ChannelEmail, burst.Channel)
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
							quiet, err := f.Store.ListColdActivityBurstsContext(ctx, q)
							requirements.NoError(err)
							assertions.Equal(page.Bursts, quiet.Bursts)
							assertions.Zero(quiet.NoiseOmitted)
							count, err = f.Store.CountColdActivityMessagesContext(ctx, q)
							requirements.NoError(err)
							assertions.Equal(1, count)
						})
					}
				})
			}
		})
	}
}
