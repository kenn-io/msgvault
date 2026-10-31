package store_test

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
	"time"
)

func TestCounterpartPopulationMatchesProjector(t *testing.T) {
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	require.NoError(t, f.Store.EnsureConversationParticipant(f.ConvID, owner, "member"))
	require.NoError(t, f.Store.EnsureConversationParticipant(f.ConvID, peer, "member"))
	for _, out := range []bool{false, true} {
		sender := peer
		if out {
			sender = owner
		}
		m := f.NewMessage().WithSentAt(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)).WithIsFromMe(out).Build()
		m.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(m)
		require.NoError(t, err)
		cs, err := f.Store.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
		require.NoError(t, err)
		require.Len(t, cs, 1)
		assert.Len(t, cs[0].Counterparts, 3)
		want := store.DirectionInbound
		if out {
			want = store.DirectionOutbound
		}
		assert.Equal(t, want, store.ClassifyActivityCandidate(cs[0], 25).Direction)
	}
}
