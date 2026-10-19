package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopKataDetailRefreshPreservesBridgeProgress(t *testing.T) {
	f := storetest.New(t)
	s := f.Store
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	id := insertKataSchemaLoop(t, f, "detail-refresh", "reply_owed", "turn_state")
	stale := insertKataTestLink(t, f, id, now)
	observed, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	require.NoError(t, s.ReconcileLoopKataContext(ctx, observed, true, now, time.Hour))
	durable, err := s.GetLoopKataLinkContext(ctx, id)
	require.NoError(t, err)
	next := now.Add(2 * time.Hour)
	durable.Attempts = 4
	durable.NextAttemptAt = &next
	durable.ClosedNotifiedAt = &now
	durable.LastError = "retry needed"
	require.NoError(t, s.UpdateLoopKataLinkContext(ctx, durable))
	checked := now.Add(time.Minute)
	stale.IssueTitle = "Refreshed detail"
	stale.IssueStatus = "closed"
	stale.CheckedAt = &checked
	stale.LastError = ""
	stale.UpdatedAt = checked
	require.NoError(t, s.RefreshLoopKataLinkContext(ctx, stale))
	got, err := s.GetLoopKataLinkContext(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "Refreshed detail", got.IssueTitle)
	assert.Equal(t, "closed", got.IssueStatus)
	assert.Empty(t, got.LastError)
	require.NotNil(t, got.CheckedAt)
	assert.True(t, checked.Equal(*got.CheckedAt))
	assert.Equal(t, 4, got.Attempts)
	require.NotNil(t, got.NextAttemptAt)
	assert.True(t, next.Equal(*got.NextAttemptAt))
	require.NotNil(t, got.ClosedNotifiedAt)
	assert.True(t, now.Equal(*got.ClosedNotifiedAt))
	require.NotNil(t, got.BridgeClosedLoopAt)
	assert.True(t, now.Equal(*got.BridgeClosedLoopAt))
	require.NoError(t, s.DeleteLoopKataLinkContext(ctx, id))
	replacement := insertKataTestLink(t, f, id, now)
	require.NoError(t, s.RefreshLoopKataLinkContext(ctx, stale))
	got, err = s.GetLoopKataLinkContext(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, replacement.LinkToken, got.LinkToken)
	assert.Equal(t, "Reply to peer", got.IssueTitle)
}
