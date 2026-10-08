package slack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelPurgeInvalidatesCoverageDuringUnfinishedRepair(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	opts.ChannelIDs = []string{"C01", "C02"}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	source, err := imp.store.GetSourceByIdentifier("T01:UME")
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.mu.Lock()
	f.onHistory = func(channel string) {
		if channel == "C02" {
			cancel()
		}
	}
	f.mu.Unlock()
	opts.Full = true
	_, err = imp.Import(ctx, opts)
	require.Error(err)
	f.mu.Lock()
	f.onHistory = nil
	f.mu.Unlock()
	before := requireResumeState(t, imp, source.ID)
	require.True(before.RepairPending)
	require.True(before.Conversations["C01"].Done)
	require.NoError(imp.store.PurgeChannelContext(t.Context(), source.ID, "C01"))
	after := requireResumeState(t, imp, source.ID)
	assert.Equal(before.Conversations["C02"], after.Conversations["C02"], "purge preserves the other channel's repair cursor")
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT count(*) FROM messages WHERE source_id=? AND source_message_id=?`), source.ID, "C01:"+ts(0)).Scan(&count))
	assert.Equal(1, count, "explicit repair must restore old non-thread messages after purge")
}
