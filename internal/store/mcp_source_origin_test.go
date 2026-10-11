package store_test

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPSourceOccurrencePinsOriginWithoutStoppingSource(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	const family = "msgvault.source_message_archived"
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
	cfg := mcpStoreConfig()
	cfg.Capabilities = append(cfg.Capabilities, store.MCPEventCapability{Family: family, SourceType: "gmail", Kinds: []string{"message"}})
	_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	resource, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	now := time.Now().UTC()
	input := newMCPStoreSubscription(t, f, 71, now)
	input.Name = family
	input.ScopeKind = "source"
	input.ScopeID = f.Source.ID
	input.Arguments, err = json.Marshal(map[string]any{"source_id": strconv.FormatInt(f.Source.ID, 10), "source_ref": resource.Reference, "include_from_me": false, "include_reactions": true})
	require.NoError(err)
	sub, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	persist := func(nativeID string) int64 {
		t.Helper()
		id, err := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID(nativeID).Build(), BodyText: sql.NullString{String: "Synthetic source occurrence", Valid: true}})
		Require.NoError(t, err)
		return id
	}
	first := persist("origin-first")
	envelope := func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"synthetic-source-origin"}`), nil
	}
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, envelope)
	require.NoError(err)
	require.NotNil(pending)
	require.Equal(first, pending.Event.MessageID)
	_, err = f.Store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now)
	require.NoError(err)
	var original sql.NullString
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT source_conversation_id FROM conversations WHERE id=?`), f.ConvID).Scan(&original))
	// Exercise the native identity triggers, including an away-and-back update.
	// Archive content and the encompassing source remain present throughout.
	var originalID any
	if original.Valid {
		originalID = original.String
	}
	for _, nativeID := range []any{"replacement-origin", originalID} {
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET source_conversation_id=? WHERE id=?`), nativeID, f.ConvID)
		require.NoError(err)
	}
	unchanged, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	assert.Equal(resource.Reference, unchanged.Reference)
	_, err = f.Store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now)
	require.Error(err, "source validity cannot authorize a replaced origin")
	require.Error(f.Store.CheckMCPDeliveryOccurrence(t.Context(), sub.ID, sub.Generation, pending.Event.Seq, now), "origin replacement must be rechecked before receiver I/O")
	called := false
	err = f.Store.ReadMCPEventMessage(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, first, now, func(*sql.Tx) error { called = true; return nil })
	require.Error(err)
	assert.False(called)
	f.ConvID, err = f.Store.EnsureConversationWithType(f.Source.ID, "unaffected-origin", "email", "Synthetic unaffected conversation")
	require.NoError(err)
	second := persist("origin-second")
	next, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now.Add(time.Minute), envelope)
	require.NoError(err)
	require.NotNil(next)
	assert.Equal(second, next.Event.MessageID)
	current, err := f.Store.GetMCPSubscription(t.Context(), sub.ID)
	require.NoError(err)
	assert.Equal("active", current.State)
}
