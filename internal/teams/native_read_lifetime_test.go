package teams

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// A response from before a native chat's revocation cannot restore that chat
// or borrow a later incarnation, even while the account itself stays valid.
func TestNativeTeamsReadPinsScopeBeforeProviderWork(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(strconv.FormatBool(replace), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f, imp, opts := newNativeTeamsInboxFeed(t)
			f.messages["event-chat"] = []map[string]any{nativeTeamsEventMessage("old", "2026-01-01T00:00:00Z", "Synthetic history", "sender@example.test")}
			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			source, err := imp.store.GetMCPResource(t.Context(), "source", sum.SourceID)
			require.NoError(err)
			before, found, err := imp.store.LookupNativeMessageScope(t.Context(), sum.SourceID, source.Reference, "teams_chat", "event-chat")
			require.NoError(err)
			require.True(found)
			release, entered := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			f.mu.Lock()
			f.blockRead, f.readEntered = release, entered
			f.messages["event-chat"] = append(f.messages["event-chat"], nativeTeamsEventMessage("stale", "2026-01-02T00:00:00Z", "Synthetic stale response", "sender@example.test"))
			f.mu.Unlock()
			done := make(chan error, 1)
			go func() { _, err := imp.Import(t.Context(), opts); done <- err }()
			select {
			case <-entered:
			case <-time.After(15 * time.Second):
				unblock()
				require.FailNow("native message request did not start")
			}
			observation := store.NativeMessageScopeInventory{SourceID: sum.SourceID, SourceReference: source.Reference, SourceType: "teams", NativeIdentity: opts.Email, Kind: "teams_chat", RetiredProviderIDs: []string{"event-chat"}}
			retireErr := imp.store.ObserveNativeMessageScopes(t.Context(), observation)
			var replaceErr error
			if replace && retireErr == nil {
				observation.RetiredProviderIDs = nil
				observation.Scopes = []store.NativeMessageScope{{ProviderID: "event-chat", DisplayName: "Synthetic replacement chat"}}
				replaceErr = imp.store.ObserveNativeMessageScopes(t.Context(), observation)
			}
			unblock()
			require.NoError(retireErr)
			require.NoError(replaceErr)
			select {
			case err := <-done:
				require.Error(err, "the pre-revocation response must not borrow or restore a current chat lifetime")
			case <-time.After(15 * time.Second):
				require.FailNow("native importer did not finish")
			}
			current, found, err := imp.store.LookupNativeMessageScope(t.Context(), sum.SourceID, source.Reference, "teams_chat", "event-chat")
			require.NoError(err)
			require.True(found)
			assert.Equal(replace, current.Present)
			if replace {
				assert.NotEqual(before.Identity.Generation, current.Identity.Generation)
			}
			var count int
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Zero(count)
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='chat:event-chat:stale'`).Scan(&count))
			assert.Zero(count)
		})
	}
}
