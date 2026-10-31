package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonTiersScopeSignalsAndOwnerBindings(t *testing.T) {
	for _, ownerBinding := range []bool{false, true} {
		t.Run(map[bool]string{false: "hidden contacts", true: "hidden owner binding"}[ownerBinding], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			owner := f.EnsureParticipant(storetest.OwnerAddress, "Owner", "example.com")
			peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
			person, _, err := f.Store.CreatePersonFromParticipant(peer)
			require.NoError(err)
			require.NoError(f.Store.AddAccountIdentity(f.Source.ID, storetest.OwnerAddress, "manual"))
			hidden, err := f.Store.GetOrCreateSource("gmail", "hidden@example.test")
			require.NoError(err)
			require.NoError(f.Store.AddAccountIdentity(hidden.ID, storetest.OwnerAddress, "manual"))
			seed := func(sourceID int64, first, count int) {
				conv, err := f.Store.EnsureConversation(sourceID, "tier-scope", "Tier scope")
				require.NoError(err)
				for i := first; i < first+count; i++ {
					sender, recipient := peer, owner
					out := i%2 == 0
					if out {
						sender, recipient = owner, peer
					}
					m := f.NewMessage().WithSourceMessageID(fmt.Sprintf("tier-scope-%d-%d", sourceID, i)).WithSentAt(tierAsOf.AddDate(0, 0, -i-1)).Build()
					m.SourceID, m.ConversationID, m.IsFromMe = sourceID, conv, out
					m.SenderID = sql.NullInt64{Int64: sender, Valid: true}
					id, err := f.Store.UpsertMessage(m)
					require.NoError(err)
					require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
					require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{recipient}, nil))
				}
			}
			count, wantScoped, wantGlobal := 3, 2, 1
			if ownerBinding {
				count, wantScoped, wantGlobal = 6, 1, 3
			}
			seed(f.Source.ID, 0, count)
			projectTierSignals(t, f, "UTC", 25)
			ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
			before, err := f.Store.PersonTiersContext(ctx, []int64{person.ID}, tierOptions())
			require.NoError(err)
			require.Equal(wantScoped, before[person.ID].Tier)
			if ownerBinding {
				require.NoError(f.Store.AddAccountIdentity(hidden.ID, "peer@example.test", "manual"))
			} else {
				seed(hidden.ID, 3, 3)
				projectTierSignals(t, f, "UTC", 25)
			}
			global, err := f.Store.PersonTiersContext(t.Context(), []int64{person.ID}, tierOptions())
			require.NoError(err)
			require.Equal(wantGlobal, global[person.ID].Tier, "the hidden mutation must cross a real tier rule")
			after, err := f.Store.PersonTiersContext(ctx, []int64{person.ID}, tierOptions())
			require.NoError(err)
			assert.Equal(before, after, "unauthorized sources cannot change tier rules")
			empty, err := f.Store.PersonTiersContext(sourcescope.WithSourceIDs(t.Context(), []int64{}), []int64{person.ID}, tierOptions())
			require.NoError(err)
			assert.Equal(3, empty[person.ID].Tier)
		})
	}
}
