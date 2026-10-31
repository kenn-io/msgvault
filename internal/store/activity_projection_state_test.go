package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"testing"
	"time"
)

func TestActivityProjectionStateIsScoped(t *testing.T) {
	f := newTimelineActivityFixture(t)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f.add(t, f.ConvID, f.Source.ID, at, false, "")
	before, err := f.Store.ActivityProjectionStateContext(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, before.Queued)
	assert.NotNil(t, before.OldestQueuedAt)
	hidden, err := f.Store.GetOrCreateSource("gmail", "hidden@example.com")
	require.NoError(t, err)
	conv, err := f.Store.EnsureConversation(hidden.ID, "hidden", "Hidden")
	require.NoError(t, err)
	f.add(t, conv, hidden.ID, at, false, "")
	after, err := f.Store.ActivityProjectionStateContext(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	all, err := f.Store.ActivityProjectionStateContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, all.Queued)
}
