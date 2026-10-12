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

func TestImportArchiveSurvivesSourceRemovalAndRetainsOlderEpochReceipt(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	f := storetest.New(t)
	const owner = "owner:synthetic-import-archive"
	const family = "msgvault.import_message_archived"
	cfg := store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: family, SourceType: "eml", Kinds: []string{"message"}}}}
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	now := time.Now().UTC()
	_, binding, err := f.Store.ResolveMCPSubscriptionResource(t.Context(), family, "archive", 0, []string{"message"}, "")
	require.NoError(err)
	require.NotNil(binding)
	activate := func(id string) *store.MCPSubscription {
		input := store.MCPSubscription{ID: id, Principal: owner, Name: family, Arguments: []byte(`{"include_from_me":false,"source_id":"*"}`), ScopeKind: "archive", ScopeID: 0, SourceID: 0, CallbackURL: "https://receiver.example.net/imports", SecretEnc: []byte("synthetic-encrypted-secret"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour)}
		active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ResourceBinding: binding, Now: now})
		Require.NoError(t, err)
		Require.NotNil(t, active)
		return active
	}
	active := activate("sub_synthetic_import_archive")
	deadSubscriber := activate("sub_synthetic_import_dead_letter")
	var storedSource sql.NullInt64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT source_id FROM mcp_event_subscriptions WHERE id=?`), active.ID).Scan(&storedSource))
	assert.False(storedSource.Valid, "archive subscription has no arbitrary source owner")
	importOne := func(fixture *storetest.Fixture, key string) int64 {
		view, finish := newSyntheticImportView(t, fixture, owner, true)
		id, err := view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: fixture.NewMessage().WithSourceMessageID(key).Build(), BodyText: sql.NullString{String: "Synthetic receipt body", Valid: true}})
		Require.NoError(t, err)
		finish()
		return id
	}
	importOne(f, "synthetic-import-source-a")
	envelope := func(_ store.MCPSubscription, _ store.MCPEvent) ([]byte, error) {
		return []byte(`{"synthetic_receipt":true}`), nil
	}
	pendingA, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, envelope)
	require.NoError(err)
	require.NotNil(pendingA)
	require.Equal(f.Source.ID, pendingA.Event.SourceID)
	deadA, err := f.Store.PrepareMCPDelivery(t.Context(), deadSubscriber.ID, deadSubscriber.Generation, now, envelope)
	require.NoError(err)
	require.NotNil(deadA)
	require.NoError(f.Store.FinishMCPDelivery(t.Context(), deadSubscriber.ID, deadSubscriber.Generation, deadA.Event.Seq, now, 413, time.Time{}))
	sourceB, err := f.Store.GetOrCreateSource("eml", "synthetic-source-b@example.net")
	require.NoError(err)
	convB, err := f.Store.EnsureConversation(sourceB.ID, "synthetic-thread-b", "Synthetic thread B")
	require.NoError(err)
	b := &storetest.Fixture{T: t, Store: f.Store, Source: sourceB, ConvID: convB}
	messageB := importOne(b, "synthetic-import-source-b")
	require.NoError(f.Store.RemoveSource(f.Source.ID))
	pendingB, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, envelope)
	require.NoError(err)
	require.NotNil(pendingB, "removing A cannot terminate the archive subscription before B")
	assert.Equal(sourceB.ID, pendingB.Event.SourceID)
	assert.Equal(messageB, pendingB.Event.MessageID)
	retained, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(retained)
	assert.Equal("active", retained.State)
	var deadLetters int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_dead_letters`).Scan(&deadLetters))
	assert.Zero(deadLetters)
	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, pendingA.Event.Seq, owner, now)
	require.Error(err)
	require.NoError(f.Store.CheckMCPDeliveryOccurrence(t.Context(), active.ID, active.Generation, pendingB.Event.Seq, now))
	require.NoError(f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, pendingB.Event.Seq, now, 200, time.Time{}))
	cfg.Capabilities = append(cfg.Capabilities, store.MCPEventCapability{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}})
	changed, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Greater(changed.Epoch, clock.Epoch)
	event, err := f.Store.GetMCPEvent(t.Context(), active.ID, pendingB.Event.Seq, owner, now)
	require.NoError(err)
	assert.Equal(messageB, event.MessageID)
	require.NoError(f.Store.ReadMCPEventMessage(t.Context(), active.ID, event.Seq, owner, messageB, now, func(tx *sql.Tx) error {
		var body string
		err := tx.QueryRowContext(t.Context(), f.Store.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), messageB).Scan(&body)
		Assert.Equal(t, "Synthetic receipt body", body)
		return err
	}))
	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, event.Seq, "owner:synthetic-other-owner", now)
	require.Error(err)
	// A retained receipt pins the occurrence's original source lifetime, even
	// when the archive subscription remains valid for other sources.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sources SET identifier=? WHERE id=?`), "synthetic-replacement-b@example.net", sourceB.ID)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sources SET identifier=? WHERE id=?`), sourceB.Identifier, sourceB.ID)
	require.NoError(err)
	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, event.Seq, owner, now)
	require.Error(err, "an away-and-back identity change cannot revive a retained import receipt")
	require.NoError(f.Store.EndMCPSubscription(t.Context(), active.ID, owner, now))
	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, event.Seq, owner, now)
	require.Error(err)
}
