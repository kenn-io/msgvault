package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
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

	t.Run("call-only source during timezone transition", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		f := newLogicalCallFixture(t)
		f.put(t, "call", nil)
		ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.source})
		_, err := f.st.ClaimActivityTimezoneTransitionContext(t.Context(), "America/New_York")
		requirements.NoError(err)
		state, err := f.st.ActivityProjectionStateContext(ctx)
		requirements.NoError(err)
		assertions.Zero(state.Queued)
		assertions.Nil(state.OldestQueuedAt)
		assertions.False(state.Reconciling)
		source, err := f.st.GetOrCreateSource("gmail", "owner@example.com")
		requirements.NoError(err)
		conv, err := f.st.EnsureConversation(source.ID, "ordinary", "Ordinary")
		requirements.NoError(err)
		_, err = f.st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "ordinary", MessageType: "email"})
		requirements.NoError(err)
		state, err = f.st.ActivityProjectionStateContext(sourcescope.WithSourceIDs(t.Context(), []int64{source.ID}))
		requirements.NoError(err)
		assertions.Equal(1, state.Queued)
		assertions.NotNil(state.OldestQueuedAt)
		assertions.True(state.Reconciling)
	})
}

func TestActivityProjectionStateIncludesConversationMarkers(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineActivityFixture(t)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f.add(t, f.ConvID, f.Source.ID, at, false, "")
	projectTimelineActivity(t, f, "UTC")
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
	before, err := f.Store.ActivityProjectionStateContext(ctx)
	requirements.NoError(err)
	assertions.Zero(before.Queued)
	member := f.EnsureParticipant("member@example.com", "Member", "example.com")
	requirements.NoError(f.Store.EnsureConversationParticipant(f.ConvID, member, "member"))
	pending, err := f.Store.ActivityProjectionStateContext(ctx)
	requirements.NoError(err)
	assertions.Equal(1, pending.Queued)
	assertions.True(pending.Reconciling)
	hidden := sourcescope.WithSourceIDs(t.Context(), []int64{})
	invisible, err := f.Store.ActivityProjectionStateContext(hidden)
	requirements.NoError(err)
	assertions.Zero(invisible.Queued)
	assertions.False(invisible.Reconciling)
	expandActivityConversations(t, f.Store)
	projectTimelineActivity(t, f, "UTC")
	// An empty conversation leaves a marker but has no activity to reconcile.
	empty, err := f.Store.EnsureConversation(f.Source.ID, "empty", "Empty")
	requirements.NoError(err)
	requirements.NoError(f.Store.EnsureConversationParticipant(empty, member, "member"))
	clean, err := f.Store.ActivityProjectionStateContext(ctx)
	requirements.NoError(err)
	assertions.Zero(clean.Queued)
	assertions.False(clean.Reconciling)
}
