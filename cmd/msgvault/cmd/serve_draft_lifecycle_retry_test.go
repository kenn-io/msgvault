package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
)

func TestDraftDeleteCancelledBeforeStoreCanRetry(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	created := createReviewDraft(t, fixture, adapter, "keep until retry")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	previous := slog.Default()
	slog.SetDefault(slog.New(reviewDraftCommitHandler{
		Handler: slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}),
		onCommit: func() {
			draft, err := fixture.store.GetIMAPDraftContext(context.Background(), created.DraftID)
			if err == nil && draft.Pending != nil {
				cancel()
			}
		},
	}))
	t.Cleanup(func() { slog.SetDefault(previous) })
	args := []string{api.CLIRunDraftDeleteCommand, created.DraftID, "--revision", "1", "--json"}
	var events []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(ctx, api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.ErrorContains(err, "cancelled")
	requirements.ErrorIs(ctx.Err(), context.Canceled)
	draft, err := fixture.store.GetIMAPDraftContext(t.Context(), created.DraftID)
	requirements.NoError(err)
	requirements.Nil(draft.Pending)
	assertions.Equal(int64(1), draft.Revision)
	assertions.Nil(draft.DiscardedAt)
	requirements.Len(events, 1)
	var output draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal("active", output.Status)
	assertions.False(output.ManualReconciliation)
	slog.SetDefault(previous)
	_, err = runReviewLifecycle(t, adapter, args...)
	requirements.NoError(err)
	draft, err = fixture.store.GetIMAPDraftContext(t.Context(), created.DraftID)
	requirements.NoError(err)
	assertions.NotNil(draft.DiscardedAt)
}
