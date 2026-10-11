package slack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelPurgeInvalidatesCoverageDuringUnfinishedRepair(t *testing.T) {
	for _, searchReplies := range []bool{true, false} {
		name := "with search"
		if !searchReplies {
			name = "without search"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := testWorkspace(t)
			if !searchReplies {
				f.scopes = "channels:read,channels:history,users:read,users:read.email"
				f.conv("C02").Kind = "public"
			}
			f.convs = append(f.convs, &fakeConv{ID: "C03", Name: "last", Kind: "public", Members: []string{"UME"}})
			imp, opts := testImporter(t, f)
			opts.ChannelIDs = []string{"C01", "C02", "C03"}
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			source, err := imp.store.GetSourceByIdentifier("T01:UME")
			require.NoError(err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.mu.Lock()
			f.onHistory = func(channel string) {
				if channel == "C03" {
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
			assert.Equal(before.Conversations["C03"], after.Conversations["C03"], "purge preserves the other channel's repair cursor")
			if !searchReplies {
				require.NotNil(after.HistoryPass)
				assert.False(after.HistoryPass.Visited["C01"], "purged work must be revisited")
				assert.True(after.HistoryPass.Visited["C02"], "completed visits to other channels survive purge")
			}
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			var count int
			require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT count(*) FROM messages WHERE source_id=? AND source_message_id=?`), source.ID, "C01:"+ts(0)).Scan(&count))
			assert.Equal(1, count, "explicit repair must restore old non-thread messages after purge")
		})
	}
}
