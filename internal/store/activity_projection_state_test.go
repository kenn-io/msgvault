package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestActivityProjectionStateIsScoped(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f.add(t, f.ConvID, f.Source.ID, at, false, "")
	before, err := f.Store.ActivityProjectionStateContext(ctx)
	requirements.NoError(err)
	assertions.Equal(1, before.Queued)
	assertions.NotNil(before.OldestQueuedAt)
	hidden, err := f.Store.GetOrCreateSource("gmail", "hidden@example.com")
	requirements.NoError(err)
	conv, err := f.Store.EnsureConversation(hidden.ID, "hidden", "Hidden")
	requirements.NoError(err)
	f.add(t, conv, hidden.ID, at, false, "")
	after, err := f.Store.ActivityProjectionStateContext(ctx)
	requirements.NoError(err)
	assertions.Equal(before, after)
	all, err := f.Store.ActivityProjectionStateContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(2, all.Queued)
}
