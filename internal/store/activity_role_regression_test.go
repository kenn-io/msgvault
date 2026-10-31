package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// A coauthor who is also addressed retains the projector's stronger sender
// role, including unsaved participants and explicit effective-limit overrides.
func TestActivityStrongestRoleMatchesProjector(t *testing.T) {
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
			for _, limit := range []int{0, 25, 1} {
				t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					q.Filters = store.ActivityFilters{IncludeNoise: true, MaxDirect: limit}
					aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), q)
					requirements.NoError(err)
					assertions.Zero(aggregate.Count)
					messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, Limit: 10, Filters: q.Filters})
					requirements.NoError(err)
					requirements.Len(messages, 1)
					seen := map[int64]bool{}
					for _, cp := range messages[0].Counterparts {
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
