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

func TestImportArchiveGlobalDisableSuspendsDeliveryWithoutRevokingReceipt(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	f := storetest.New(t)
	const owner = "owner:synthetic-import-policy"
	const family = "msgvault.import_message_archived"
	cfg := store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: family, SourceType: "eml", Kinds: []string{"message"}}}}
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	_, binding, err := f.Store.ResolveMCPSubscriptionResource(t.Context(), family, "archive", 0, []string{"message"}, "")
	require.NoError(err)
	now := time.Now().UTC()
	sub, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Now: now, ResourceBinding: binding, Subscription: store.MCPSubscription{
		ID: "sub_synthetic_import_policy", Principal: owner, Name: family,
		Arguments: []byte(`{"include_from_me":false,"source_id":"*"}`), ScopeKind: "archive",
		CallbackURL: "https://receiver.example.net/imports", SecretEnc: []byte("synthetic-ciphertext"),
		SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour),
	}})
	require.NoError(err)
	require.NotNil(sub)
	view, finish := newSyntheticImportView(t, f, owner, true)
	messageID, err := view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID("synthetic-import-policy-message").Build(), BodyText: sql.NullString{String: "Synthetic imported policy body", Valid: true}})
	require.NoError(err)
	finish()
	envelope := func(_ store.MCPSubscription, _ store.MCPEvent) ([]byte, error) {
		return []byte(`{"synthetic_pending":true}`), nil
	}
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, envelope)
	require.NoError(err)
	require.NotNil(pending)
	require.NoError(f.Store.CheckMCPDeliveryOccurrence(t.Context(), sub.ID, sub.Generation, pending.Event.Seq, now))

	cfg.ImportNotifications = false
	disabled, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(clock.Epoch, disabled.Epoch, "import permission does not change live capture coverage")
	require.Error(f.Store.CheckMCPSubscription(t.Context(), sub.ID, sub.Generation, now), "disabled import delivery must fail worker preflight")
	require.Error(f.Store.CheckMCPDeliveryOccurrence(t.Context(), sub.ID, sub.Generation, pending.Event.Seq, now), "disabled import delivery must fail immediate occurrence preflight")
	called := false
	blocked, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, func(_ store.MCPSubscription, _ store.MCPEvent) ([]byte, error) {
		called = true
		return []byte(`{}`), nil
	})
	require.NoError(err)
	assert.Nil(blocked)
	assert.False(called)
	retained, err := f.Store.GetMCPSubscription(t.Context(), sub.ID)
	require.NoError(err)
	require.NotNil(retained)
	assert.Equal(pending.Subscription.PendingEnvelope, retained.PendingEnvelope, "permission toggle cannot rewrite persisted bytes")
	assert.Equal(pending.Event.Seq, retained.PendingSeq)
	event, err := f.Store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, owner, now)
	require.NoError(err, "a permission toggle cannot revoke an otherwise retained receipt")
	assert.Equal(messageID, event.MessageID)
	_, _, err = f.Store.ResolveMCPSubscriptionResource(t.Context(), family, "archive", 0, []string{"message"}, "")
	require.Error(err, "new import admission is disabled")

	cfg.ImportNotifications = true
	resumed, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(clock.Epoch, resumed.Epoch)
	require.NoError(f.Store.CheckMCPSubscription(t.Context(), sub.ID, sub.Generation, now))
	retry, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, envelope)
	require.NoError(err)
	require.NotNil(retry)
	assert.Equal(pending.Subscription.PendingEnvelope, retry.Subscription.PendingEnvelope)
	assert.Equal(pending.Event.Seq, retry.Event.Seq)
}
