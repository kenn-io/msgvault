package discord

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Constructors remain available to callback assertions.
	Require "github.com/stretchr/testify/require" //nolint:importas // Constructors remain available to callback assertions.
	"go.kenn.io/msgvault/internal/store"
	"testing"
)

// Legacy archive origins predate native inventories. A bounded thread catalog
// cannot prove absence, but the complete parent list can prove a parent gone.
func TestNativeDiscordLegacyThreadOriginRetiresWithAbsentParent(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f, imp, opts := newNativeDiscordInboxFeed(t)
	cfg := store.MCPEventsConfig{Enabled: false, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: "discord", Kinds: []string{"message"}},
		{Family: "msgvault.source_message_archived", SourceType: "discord", Kinds: []string{"message"}},
		{Family: "msgvault.inbox_message_archived", SourceType: "discord", Kinds: []string{"message"}},
	}}
	_, err := imp.store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	var threadID, quietID int64
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id='400'`), first.SourceID).Scan(&threadID))
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id='301'`), first.SourceID).Scan(&quietID))
	thread, err := imp.store.GetMCPResource(t.Context(), "conversation", threadID)
	require.NoError(err)
	quiet, err := imp.store.GetMCPResource(t.Context(), "conversation", quietID)
	require.NoError(err)
	var scopes int
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM native_message_scopes WHERE source_id=?`), first.SourceID).Scan(&scopes))
	require.Zero(scopes, "ordinary pre-inventory archive has no native scope rows")
	cfg.Enabled = true
	_, err = imp.store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	f.mu.Lock()
	f.threads = nil
	f.mu.Unlock()
	opts.Full = true
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	same, err := imp.store.GetMCPResource(t.Context(), "conversation", threadID)
	require.NoError(err)
	assert.Equal(thread.Reference, same.Reference, "omission from a bounded thread catalog does not revoke a quiet old thread")
	f.mu.Lock()
	f.channels = f.channels[1:]
	f.mu.Unlock()
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	retired, err := imp.store.GetMCPResource(t.Context(), "conversation", threadID)
	require.NoError(err)
	assert.NotEqual(thread.Reference, retired.Reference, "positive parent disappearance must revoke an old thread origin even without a native scope row")
	currentQuiet, err := imp.store.GetMCPResource(t.Context(), "conversation", quietID)
	require.NoError(err)
	assert.Equal(quiet.Reference, currentQuiet.Reference, "independent old origins remain valid")
	var archived int
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM conversations WHERE id=?`), threadID).Scan(&archived))
	assert.Equal(1, archived)
}
