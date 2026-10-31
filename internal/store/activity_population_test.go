package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCounterpartPopulationMatchesProjector(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	requirements.NoError(f.Store.EnsureConversationParticipant(f.ConvID, owner, "member"))
	requirements.NoError(f.Store.EnsureConversationParticipant(f.ConvID, peer, "member"))
	for _, out := range []bool{false, true} {
		sender := peer
		if out {
			sender = owner
		}
		m := f.NewMessage().WithSentAt(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)).WithIsFromMe(out).Build()
		m.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(m)
		requirements.NoError(err)
		cs, err := f.Store.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
		requirements.NoError(err)
		requirements.Len(cs, 1)
		assertions.Len(cs[0].Counterparts, 3)
		want := store.DirectionInbound
		if out {
			want = store.DirectionOutbound
		}
		assertions.Equal(want, store.ClassifyActivityCandidate(cs[0], 25).Direction)
	}
}
