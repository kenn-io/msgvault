package mcpevents

import (
	"path/filepath"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRetentionCannotExceedSevenDayHardBound(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	opts := Options{Enabled: true, Sources: []string{"gmail"}, OwnerKey: "synthetic-owner", KeyPath: filepath.Join(t.TempDir(), "key"), Retention: 169 * time.Hour}
	_, err := New(t.Context(), f.Store, opts)
	require.Error(err)
	var eventErr *Error
	require.ErrorAs(err, &eventErr)
	assert.Equal("invalid_retention", eventErr.Reason)
	opts.Retention = -time.Hour
	_, err = New(t.Context(), f.Store, opts)
	require.Error(err)
	for _, retention := range []time.Duration{0, time.Hour, 168 * time.Hour} {
		opts.Retention = retention
		s, err := New(t.Context(), f.Store, opts)
		require.NoError(err)
		if retention == 0 {
			assert.Equal(168*time.Hour, s.opts.Retention)
		} else {
			assert.Equal(retention, s.opts.Retention)
		}
	}
}

func TestStartupRetentionPruneAdvancesFloorBeforeReplay(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventServiceWithRetention(t, time.Minute)
	first, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	appendReceipt(t, s, f, 1)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_log SET recorded_at=? WHERE seq=1`), time.Now().UTC().Add(-2*time.Minute).Format("2006-01-02T15:04:05.000000000Z"))
	require.NoError(err)

	opts := eventServiceOptionsWithWebhook(s.opts, s.webhook)
	restarted, err := New(t.Context(), f.Store, opts)
	require.NoError(err)
	var floor, retained int64
	require.NoError(f.Store.DB().QueryRow(`SELECT pruned_through_seq FROM mcp_event_clock WHERE singleton=1`).Scan(&floor))
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&retained))
	assert.Equal(int64(1), floor)
	assert.Zero(retained)
	stopped, err := f.Store.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	require.NotNil(stopped)
	assert.Equal("stopped", stopped.State)
	assert.Equal("retention", stopped.StopReason)

	req.Cursor = &first.Cursor
	resumed, err := restarted.Subscribe(t.Context(), restarted.principal, req)
	require.NoError(err)
	assert.True(resumed.Truncated, "replay below the retention floor must resume from the head")
	active, err := f.Store.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	require.NotNil(active)
	assert.Equal("active", active.State)
	assert.Equal(int64(1), active.CursorSeq)
}

func TestRetentionSweepIntervalTracksConfiguredRetention(t *testing.T) {
	assert := Assert.New(t)
	assert.Equal(time.Second, retentionSweepInterval(time.Nanosecond))
	assert.Equal(30*time.Second, retentionSweepInterval(time.Minute))
	assert.Equal(time.Hour, retentionSweepInterval(7*24*time.Hour))
}
