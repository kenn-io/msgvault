package slack

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInterruptedPublicSyncReachesEveryChannel(t *testing.T) {
	for _, interrupt := range []string{"between conversations", "during history"} {
		t.Run(interrupt, func(t *testing.T) {
			require := require.New(t)
			f := testWorkspace(t)
			f.scopes = "channels:read,channels:history,users:read,users:read.email"
			f.searchMissingScope = true
			f.convs = []*fakeConv{
				{ID: "C01", Name: "first", Kind: "public", Members: []string{"UME"},
					Msgs: []fakeMsg{{TS: ts(1), User: "UME", Text: "original"}}},
				{ID: "C02", Name: "second", Kind: "public", Members: []string{"UME"},
					Msgs: []fakeMsg{{TS: ts(1), User: "UME", Text: "original"}}},
				{ID: "C03", Name: "third", Kind: "public", Members: []string{"UME"},
					Msgs: []fakeMsg{{TS: ts(1), User: "UME", Text: "original"}}},
			}
			imp, opts := testImporter(t, f)
			now := tsBase.Add(24 * time.Hour)
			imp.now = func() time.Time { return now }
			_, err := imp.Import(context.Background(), opts)
			require.NoError(err)

			// All channels have new top-level activity and a first reply on
			// an old root. Only the history audit can discover that reply.
			f.mu.Lock()
			for _, c := range f.convs {
				c.Msgs[0].Replies = []fakeMsg{
					{TS: ts(1500), ThreadTS: ts(1), User: "UME", Text: "late reply"},
				}
				c.Msgs = append(c.Msgs, fakeMsg{TS: ts(1501), User: "UME", Text: "new message"})
			}
			f.mu.Unlock()
			now = now.Add(2 * time.Hour)

			// Recreate the importer between attempts: progress must survive
			// in the store, not just in one process's in-memory rotation.
			for range 12 {
				ctx, cancel := context.WithCancel(context.Background())
				f.mu.Lock()
				historyCalls := 0
				f.onHistory = func(string) {
					historyCalls++
					if interrupt == "during history" && historyCalls == 2 {
						cancel()
					}
				}
				f.mu.Unlock()
				opts.Progress = func(line string) {
					if interrupt == "between conversations" && strings.HasPrefix(line, "conversation ") {
						cancel()
					}
				}
				resumed := NewImporter(imp.store, imp.client, opts.TeamID)
				resumed.now = func() time.Time { return now }
				_, err = resumed.Import(ctx, opts)
				cancel()
				require.ErrorIs(err, context.Canceled)
				now = now.Add(time.Minute)
			}

			for _, channelID := range []string{"C01", "C02", "C03"} {
				var count int
				err = imp.store.DB().QueryRow(imp.store.Rebind(`
					SELECT COUNT(*) FROM messages m
					JOIN conversations c ON c.id = m.conversation_id
					WHERE c.source_conversation_id = ?`), channelID).Scan(&count)
				require.NoError(err)
				assert.Equal(t, 3, count, "%s must retain its original, new message, and late reply", channelID)
			}
		})
	}
}
