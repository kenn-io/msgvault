package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
)

// A sender who is also addressed retains the stronger sender
// role for saved and unsaved participants under the stored audience limit.
func TestColdActivityRetainsStrongestSubjectRoles(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved=%t", saved), func(t *testing.T) {
			requirements := require.New(t)
			f := newColdTimelineActivityFixture(t)
			coldTimelineRoom(t, f, f.Source.ID, "default-thread")
			subject := store.ActivitySubject{ParticipantIDs: []int64{f.peer}}
			alias := int64(0)
			if saved {
				alias = f.EnsureParticipant("alias@example.com", "Alias", "example.com")
				_, err := f.Store.LinkParticipants(f.peer, alias)
				requirements.NoError(err)
				p, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				requirements.NoError(err)
				subject = store.ActivitySubject{PersonID: p.ID}
			}
			id := addColdTimelineMessage(t, f, f.ConvID, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), false)
			requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{f.peer}, nil))
			guest := f.EnsureParticipant("guest@example.com", "Guest", "example.com")
			to := []int64{f.owner, f.peer, guest}
			if alias != 0 {
				to = append(to, alias)
			}
			requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", to, nil))
			for _, limit := range []int{25, 1} {
				t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					burstQuery := timelineBurstQuery()
					burstQuery.Subject = subject
					// The cold room stays unprojected; RunOnce stores the audience limit.
					projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 100, MaxDirectCounterparts: limit})
					requirements.NoError(err)
					_, err = projector.RunOnce(t.Context())
					requirements.NoError(err)
					burstQuery.Filters = store.ActivityFilters{IncludeNoise: true}
					page, err := f.Store.ListColdActivityBurstsContext(t.Context(), burstQuery)
					requirements.NoError(err)
					requirements.Len(page.Bursts, 1)
					wantInbound := 1
					if limit == 1 {
						wantInbound = 0
					}
					assertions.Equal(wantInbound, page.Bursts[0].Inbound)
					assertions.Zero(page.Bursts[0].Outbound)
					seen := map[int64]bool{}
					for _, cp := range page.Bursts[0].Counterparts {
						seen[cp.ParticipantID] = true
						if cp.ParticipantID == f.peer || cp.ParticipantID == alias {
							assertions.Equal(store.RoleSender, cp.Role)
							assertions.Equal(limit != 1, cp.Counted)
							assertions.Equal(store.EvidenceDirect, cp.Evidence)
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
