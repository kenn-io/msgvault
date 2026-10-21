package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
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
