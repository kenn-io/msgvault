package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestActivityProjectionStateIsScoped(t *testing.T) {
	for _, trigger := range []string{"identity", "message", "roster"} {
		t.Run(trigger, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newTimelineActivityFixture(t)
			q := timelineContactQuery(f)
			ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
			f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
			if trigger != "message" {
				projectTierSignals(t, f.Fixture, "UTC", 0)
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
			if trigger != "message" {
				projectTierSignals(t, f.Fixture, "UTC", 0)
				if trigger == "identity" {
					requirements.NoError(f.Store.AddAccountIdentity(hidden.ID, "alice@example.com", "manual"))
				} else {
					requirements.NoError(f.Store.EnsureConversationParticipant(conv, f.peer, "member"))
				}
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
			if trigger == "identity" {
				requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
			} else {
				requirements.NoError(f.Store.EnsureConversationParticipant(f.ConvID, f.peer, "member"))
			}
			after, err = f.Store.ActivityProjectionStateContext(ctx)
			requirements.NoError(err)
			if trigger == "roster" {
				assertions.Equal(1, after.Queued)
			}
			assertions.True(after.Reconciling)
			empty, err := f.Store.ActivityProjectionStateContext(sourcescope.WithSourceIDs(t.Context(), []int64{}))
			requirements.NoError(err)
			assertions.False(empty.Reconciling)
			projectTierSignals(t, f.Fixture, "UTC", 0)
			drained, err := f.Store.ActivityProjectionStateContext(ctx)
			requirements.NoError(err)
			assertions.Zero(drained.Queued)
			assertions.False(drained.Reconciling)
		})
	}
}
