package store_test

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestResolvePersonReferenceHiddenBridgeDoesNotChangeCluster(t *testing.T) {
	f := storetest.New(t)
	visible := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
	hidden := f.EnsureParticipant("hidden@example.com", "Hidden", "example.com")
	for _, participant := range []int64{visible, other} {
		m := f.NewMessage().WithSentAt(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)).Build()
		m.SenderID = sql.NullInt64{Int64: participant, Valid: true}
		_, err := f.Store.UpsertMessage(m)
		require.NoError(t, err)
	}
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
	before, err := f.Store.ResolvePersonReferenceContext(ctx, "participant", strconv.FormatInt(visible, 10), nil, 10)
	require.NoError(t, err)
	require.Len(t, before, 1)
	assert.Equal(t, []int64{visible}, before[0].ParticipantIDs)
	_, err = f.Store.LinkParticipants(visible, hidden)
	require.NoError(t, err)
	_, err = f.Store.LinkParticipants(hidden, other)
	require.NoError(t, err)
	after, err := f.Store.ResolvePersonReferenceContext(ctx, "participant", strconv.FormatInt(visible, 10), nil, 10)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}
