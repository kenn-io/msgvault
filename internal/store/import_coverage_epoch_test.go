package store_test

import (
	"database/sql"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportPermissionCoverageDoesNotInterruptLiveEvents(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	f := storetest.New(t)
	const owner = "owner:synthetic-independent-coverage"
	liveCap := store.MCPEventCapability{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}
	cfg := store.MCPEventsConfig{Enabled: true, Principal: owner, Capabilities: []store.MCPEventCapability{liveCap}}
	before, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	now := time.Now().UTC()
	input := store.MCPSubscription{ID: "sub_synthetic_live_coverage", Principal: owner, Name: liveCap.Family, Arguments: []byte(`{"include_from_me":false}`), ScopeKind: "conversation", ScopeID: f.ConvID, SourceID: f.Source.ID, CallbackURL: "https://receiver.example.net/live", SecretEnc: []byte("synthetic-ciphertext"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour)}
	require.NoError(f.Store.BindMCPSubscriptionScope(t.Context(), &input))
	sub, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Now: now, Subscription: input})
	require.NoError(err)
	require.NotNil(sub)
	cfg.ImportNotifications = true
	cfg.Capabilities = append(cfg.Capabilities, store.MCPEventCapability{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}})
	enabled, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(before.Epoch, enabled.Epoch, "import coverage cannot interrupt an existing live subscription")
	require.NoError(f.Store.CheckMCPSubscription(t.Context(), sub.ID, sub.Generation, now))
	live := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: now})
	_, err = live.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID("synthetic-live-after-import-optin").Build(), BodyText: sql.NullString{String: "Synthetic live body", Valid: true}})
	require.NoError(err)
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, func(_ store.MCPSubscription, _ store.MCPEvent) ([]byte, error) { return []byte(`{}`), nil })
	require.NoError(err)
	require.NotNil(pending)
	cfg.ImportNotifications = false
	cfg.Capabilities = []store.MCPEventCapability{liveCap}
	disabled, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(before.Epoch, disabled.Epoch)
	require.NoError(f.Store.CheckMCPSubscription(t.Context(), sub.ID, sub.Generation, now))
	retry, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, func(_ store.MCPSubscription, _ store.MCPEvent) ([]byte, error) { return []byte(`{}`), nil })
	require.NoError(err)
	require.NotNil(retry)
	assert.Equal(pending.Event.Seq, retry.Event.Seq)
}
