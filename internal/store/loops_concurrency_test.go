package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopConcurrentDoneAndApply(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL conversation row lock interleaving")
	}
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := f.CreateMessage("concurrent-done")
	open := loopOpen(t, f, message, message, now)
	applyLoop(t, f, open, now)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		results <- f.Store.ApplyLoopVerdictContext(ctx, store.LoopApply{ConversationID: f.ConvID, Now: now, Verdict: store.LoopVerdict{Open: &open}})
	}()
	go func() {
		<-start
		_, err := f.Store.CloseLoopContext(ctx, live[0].ID, "handled concurrently", now)
		results <- err
	}()
	close(start)
	requirements.NoError(<-results)
	requirements.NoError(<-results)
	got, err := f.Store.GetOpenLoopContext(t.Context(), live[0].ID)
	requirements.NoError(err)
	assertions.Equal("closed", got.Status)
	requirements.NotNil(got.ClosedReason)
	assertions.Equal("done", *got.ClosedReason)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	assertions.Empty(rows)
}
