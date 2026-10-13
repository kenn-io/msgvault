package matrix

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// Revoking an exact standard thread is independent of its still-readable
// room. An older sync must not revive or borrow the thread's new lifetime.
func TestNativeMatrixReadCannotBorrowReplacementThread(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(strconv.FormatBool(replace), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f, imp, opts, sourceID := newNativeMatrixInboxFeed(t)
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			source, err := imp.store.GetMCPResource(t.Context(), "source", sourceID)
			require.NoError(err)
			before, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_thread", "$root")
			require.NoError(err)
			require.True(found)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			f.mu.Lock()
			f.round = 1
			f.syncEntered, f.syncRelease = entered, release
			f.mu.Unlock()
			done := make(chan error, 1)
			go func() { _, err := imp.Import(t.Context(), opts); done <- err }()
			select {
			case <-entered:
			case <-time.After(15 * time.Second):
				unblock()
				require.FailNow("native sync request did not start")
			}
			observation := store.NativeMessageScopeInventory{SourceID: sourceID, SourceReference: source.Reference, SourceType: SourceType, NativeIdentity: opts.UserID, Kind: "matrix_thread", ParentProviderID: "!room:example.org", RetiredProviderIDs: []string{"$root"}}
			retireErr := imp.store.ObserveNativeMessageScopes(t.Context(), observation)
			var replaceErr error
			if replace && retireErr == nil {
				observation.RetiredProviderIDs = nil
				observation.Scopes = []store.NativeMessageScope{{ProviderID: "$root", DisplayName: "Synthetic replacement thread"}}
				replaceErr = imp.store.ObserveNativeMessageScopes(t.Context(), observation)
			}
			unblock()
			require.NoError(retireErr)
			require.NoError(replaceErr)
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				require.FailNow("native sync did not finish")
			}
			current, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_thread", "$root")
			require.NoError(err)
			require.True(found)
			assert.Equal(replace, current.Present)
			if replace {
				assert.NotEqual(before.Identity.Generation, current.Identity.Generation)
			}
			var count int
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Zero(count)
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='$reply-1'`).Scan(&count))
			assert.Zero(count, "stale thread rejects the mandatory message snapshot")
		})
	}
}
