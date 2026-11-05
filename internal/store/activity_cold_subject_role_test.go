package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
)

// A coauthor who is also addressed retains the stronger sender
// role, including unsaved participants and explicit effective-limit overrides.
func TestColdActivityRetainsStrongestSubjectRoles(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved=%t", saved), func(t *testing.T) {
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
			setTimelineConversationTier(t, f, f.Source.ID, "default-thread", conversationpolicy.Cold)
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
