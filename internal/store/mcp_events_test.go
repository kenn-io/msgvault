package store_test

import (
	"context"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPEventsSchemaStartsDisabled(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	var head, floor, epoch int64
	var enabled bool
	err := f.Store.DB().QueryRow(`SELECT head_seq, pruned_through_seq, capture_epoch, enabled FROM mcp_event_clock WHERE singleton = 1`).Scan(&head, &floor, &epoch, &enabled)
	require.NoError(err)
	assert.Equal(int64(0), head)
	assert.Equal(int64(0), floor)
	assert.Equal(int64(0), epoch)
	assert.False(enabled)
	for _, table := range []string{"mcp_event_log", "mcp_event_subscriptions", "mcp_event_dead_letters", "mcp_event_message_refs"} {
		var count int
		require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count))
		assert.Zero(count)
	}
}

// An ingest view only tags writes with their provider phase. It must share
// the root Store's in-process guards, or a view and the root could run the
// same person's CardDAV operation at once.
func TestMCPIngestViewSharesRootOperationGuards(t *testing.T) {
	f := storetest.New(t)
	release, err := f.Store.AcquireCardDAVPersonOperation(t.Context(), 1)
	Require.NoError(t, err)
	defer release()
	for name, view := range map[string]*store.Store{
		"root view":   f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive}),
		"nested view": f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive}).WithIngestContext(store.IngestContext{Mode: store.IngestBackfill}),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			_, err := view.AcquireCardDAVPersonOperation(ctx, 1)
			Require.ErrorIs(t, err, context.DeadlineExceeded, "the view must wait for the root's operation")
		})
	}
}
