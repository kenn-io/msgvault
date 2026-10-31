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
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	visible := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
	hidden := f.EnsureParticipant("hidden@example.com", "Hidden", "example.com")
	for _, participant := range []int64{visible, other} {
		m := f.NewMessage().WithSentAt(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)).Build()
		m.SenderID = sql.NullInt64{Int64: participant, Valid: true}
		_, err := f.Store.UpsertMessage(m)
		requirements.NoError(err)
	}
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
	before, err := f.Store.ResolvePersonReferenceContext(ctx, "participant", strconv.FormatInt(visible, 10), nil, 10)
	requirements.NoError(err)
	requirements.Len(before, 1)
	assertions.Equal([]int64{visible}, before[0].ParticipantIDs)
	_, err = f.Store.LinkParticipants(visible, hidden)
	requirements.NoError(err)
	_, err = f.Store.LinkParticipants(hidden, other)
	requirements.NoError(err)
	after, err := f.Store.ResolvePersonReferenceContext(ctx, "participant", strconv.FormatInt(visible, 10), nil, 10)
	requirements.NoError(err)
	assertions.Equal(before, after)
}
