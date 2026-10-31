package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

// A coauthor who is also addressed retains the projector's stronger sender
// role, including unsaved participants and explicit effective-limit overrides.
func TestColdActivityStrongestRoleMatchesProjector(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved=%t", saved), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newTimelineActivityFixture(t)
			q := timelineContactQuery(f)
			alias := int64(0)
			if saved {
				alias = f.EnsureParticipant("alias@example.com", "Alias", "example.com")
				_, err := f.Store.LinkParticipants(f.peer, alias)
				requirements.NoError(err)
				p, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				requirements.NoError(err)
				q.Subject = store.ActivitySubject{PersonID: p.ID}
			}
			id := f.add(t, f.ConvID, f.Source.ID, q.Now, true, "")
			requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{f.owner, f.peer}, nil))
			guest := f.EnsureParticipant("guest@example.com", "Guest", "example.com")
			to := []int64{f.peer, guest}
			if alias != 0 {
				to = append(to, alias)
			}
			requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", to, nil))
			projectTimelineActivity(t, f, "UTC")
			cs, err := f.Store.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
			requirements.NoError(err)
			requirements.Len(cs, 1)
			if saved {
				classified := store.ClassifyActivityCandidate(cs[0], 25)
				requirements.Len(classified.Persons, 1)
				assertions.Equal(store.RoleSender, classified.Persons[0].Role)
			}
			setTimelineConversationTier(t, f, f.Source.ID, "default-thread", conversationpolicy.Cold)
			projectColdTimeline(t, f, "UTC")
			retractTimelineActivity(t, f, id)
			for _, limit := range []int{0, 25, 1} {
				t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					q.Filters = store.ActivityFilters{IncludeNoise: true, MaxDirect: limit}
					burstQuery := timelineBurstQuery()
					burstQuery.Subject = q.Subject
					burstQuery.Filters = q.Filters
					page, err := f.Store.ListColdActivityBurstsContext(t.Context(), burstQuery)
					requirements.NoError(err)
					requirements.Len(page.Bursts, 1)
					assertions.Zero(page.Bursts[0].Outbound)
					seen := map[int64]bool{}
					for _, cp := range page.Bursts[0].Counterparts {
						seen[cp.ParticipantID] = true
						if cp.ParticipantID == f.peer || cp.ParticipantID == alias {
							assertions.Equal(store.RoleSender, cp.Role)
							assertions.False(cp.Counted)
							wantEvidence := store.EvidenceDirect
							if limit == 1 {
								wantEvidence = store.EvidenceCoPresence
							}
							assertions.Equal(wantEvidence, cp.Evidence)
						}
						if cp.ParticipantID == guest {
							assertions.Equal(limit != 1, cp.Counted)
						}
					}
					assertions.True(seen[f.peer])
					assertions.True(seen[guest])
					if alias != 0 {
						assertions.True(seen[alias])
					}
				})
			}
		})
	}
}

// Both copies have contact, but only the sent copy counts for the requested
// addressee. The lower source is a received copy in another owned account.
func TestColdActivityEmailCopiesPreferRequestedSubject(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved=%t", saved), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newTimelineActivityFixture(t)
			contact := timelineContactQuery(f)
			if saved {
				p, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				requirements.NoError(err)
				contact.Subject = store.ActivitySubject{PersonID: p.ID}
			}
			owner2 := f.EnsureParticipant("second@example.com", "Second owner", "example.com")
			source, err := f.Store.GetOrCreateSource("gmail", "second@example.com")
			requirements.NoError(err)
			requirements.NoError(f.Store.AddAccountIdentity(source.ID, "second@example.com", "manual"))
			room, err := f.Store.EnsureConversationWithType(source.ID, "sent-copy", "email_thread", "Sent copy")
			requirements.NoError(err)
			received := f.add(t, f.ConvID, f.Source.ID, contact.Now.Add(-time.Hour), false, "<subject-copy@example.com>")
			requirements.NoError(f.Store.ReplaceMessageRecipients(received, "from", []int64{owner2}, nil))
			requirements.NoError(f.Store.ReplaceMessageRecipients(received, "to", []int64{f.owner, f.peer}, nil))
			sent := f.add(t, room, source.ID, contact.Now, true, "<subject-copy@example.com>")
			requirements.NoError(f.Store.ReplaceMessageRecipients(sent, "from", []int64{owner2}, nil))
			projectTimelineActivity(t, f, "UTC")
			setTimelineConversationTier(t, f, f.Source.ID, "default-thread", conversationpolicy.Cold)
			setTimelineConversationTier(t, f, source.ID, "sent-copy", conversationpolicy.Cold)
			projectColdTimeline(t, f, "UTC")
			retractTimelineActivity(t, f, received)
			retractTimelineActivity(t, f, sent)
			fullQuery := timelineBurstQuery()
			fullQuery.Filters.IncludeNoise = true
			full, err := f.Store.ListColdActivityBurstsContext(t.Context(), fullQuery)
			requirements.NoError(err)
			requirements.Len(full.Bursts, 1)
			assertions.Equal(received, full.Bursts[0].AnchorMessageID)
			for _, scope := range []struct {
				sources       []int64
				count, copies int
				id            int64
			}{{nil, 1, 2, sent}, {[]int64{source.ID}, 1, 1, sent}, {[]int64{f.Source.ID}, 0, 1, received}} {
				ctx := t.Context()
				if scope.sources != nil {
					ctx = sourcescope.WithSourceIDs(ctx, scope.sources)
				}
				q := timelineBurstQuery()
				q.Subject = contact.Subject
				q.Filters.IncludeNoise = true
				page, err := f.Store.ListColdActivityBurstsContext(ctx, q)
				requirements.NoError(err)
				requirements.Len(page.Bursts, 1)
				assertions.Equal(scope.id, page.Bursts[0].AnchorMessageID)
				assertions.Equal(scope.count, page.Bursts[0].Outbound)
				assertions.Zero(page.Bursts[0].Inbound)
				assertions.Equal(scope.copies, page.Bursts[0].CopyCounts[scope.id])
			}
		})
	}
}
