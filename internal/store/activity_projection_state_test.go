package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func TestActivityProjectionStateScopesIdentityChanges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	id := f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
	hidden, err := f.Store.GetOrCreateSource("gmail", "hidden@example.com")
	require.NoError(err)
	conv, err := f.Store.EnsureConversation(hidden.ID, "hidden", "Hidden")
	require.NoError(err)
	f.add(t, conv, hidden.ID, q.Now, false, "")
	projectTimelineActivity(t, f, "UTC")
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
	before, err := f.Store.ActivityProjectionStateContext(ctx)
	require.NoError(err)
	require.False(before.Reconciling)
	require.NoError(f.Store.AddAccountIdentity(hidden.ID, "alice@example.com", "manual"))
	afterHidden, err := f.Store.ActivityProjectionStateContext(ctx)
	require.NoError(err)
	assert.Equal(before, afterHidden)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	messages, err := f.Store.ListActivityMessagesContext(ctx, store.MessageQuery{ConversationID: f.ConvID, Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}})
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal(id, messages[0].MessageID)
	assert.Equal(store.DirectionInbound, messages[0].Direction)
	pending, err := f.Store.ActivityProjectionStateContext(ctx)
	require.NoError(err)
	assert.True(pending.Reconciling)
	empty, err := f.Store.ActivityProjectionStateContext(sourcescope.WithSourceIDs(t.Context(), []int64{}))
	require.NoError(err)
	assert.False(empty.Reconciling)
	projectTimelineActivity(t, f, "UTC")
	drained, err := f.Store.ActivityProjectionStateContext(ctx)
	require.NoError(err)
	assert.False(drained.Reconciling)
	messages, err = f.Store.ListActivityMessagesContext(ctx, store.MessageQuery{ConversationID: f.ConvID, Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}})
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal(store.DirectionOutbound, messages[0].Direction)
}

func TestActivityProjectionStateIsScoped(t *testing.T) {
	for _, trigger := range []string{"message", "roster"} {
		t.Run(trigger, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newTimelineActivityFixture(t)
			q := timelineContactQuery(f)
			ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
			f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
			if trigger == "roster" {
				projectTimelineActivity(t, f, "UTC")
			}
			before, err := f.Store.ActivityProjectionStateContext(ctx)
			requirements.NoError(err)
			if trigger == "message" {
				assertions.Equal(1, before.Queued)
				assertions.NotNil(before.OldestQueuedAt)
			} else {
				requirements.Zero(before.Queued)
				requirements.False(before.Reconciling)
			}
			hidden, err := f.Store.GetOrCreateSource("gmail", "hidden@example.com")
			requirements.NoError(err)
			conv, err := f.Store.EnsureConversation(hidden.ID, "hidden", "Hidden")
			requirements.NoError(err)
			f.add(t, conv, hidden.ID, q.Now, false, "")
			if trigger == "roster" {
				projectTimelineActivity(t, f, "UTC")
				requirements.NoError(f.Store.EnsureConversationParticipant(conv, f.peer, "member"))
			}
			after, err := f.Store.ActivityProjectionStateContext(ctx)
			requirements.NoError(err)
			assertions.Equal(before, after)
			if trigger == "message" {
				all, err := f.Store.ActivityProjectionStateContext(t.Context())
				requirements.NoError(err)
				assertions.Equal(2, all.Queued)
				return
			}
			requirements.NoError(f.Store.EnsureConversationParticipant(f.ConvID, f.peer, "member"))
			after, err = f.Store.ActivityProjectionStateContext(ctx)
			requirements.NoError(err)
			assertions.Equal(1, after.Queued)
			assertions.True(after.Reconciling)
			projectTimelineActivity(t, f, "UTC")
			drained, err := f.Store.ActivityProjectionStateContext(ctx)
			requirements.NoError(err)
			assertions.Zero(drained.Queued)
			assertions.False(drained.Reconciling)
		})
	}
}
