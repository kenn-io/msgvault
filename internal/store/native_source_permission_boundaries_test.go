package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestNativeSourcePermissionUnknownPausesEveryEventsBoundary(t *testing.T) {
	for _, family := range []string{"msgvault.message_archived", "msgvault.source_message_archived"} {
		t.Run(family, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f := storetest.New(t)
			config := mcpStoreConfig()
			config.Capabilities = append(config.Capabilities, store.MCPEventCapability{Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"message"}})
			_, err := f.Store.ConfigureMCPEvents(t.Context(), config)
			require.NoError(err)
			source, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
			require.NoError(err)
			permission := store.NativeSourcePermission{SourceID: f.Source.ID, SourceReference: source.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier, Known: true, MessageRead: true}
			require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), permission))
			now := time.Now().UTC()
			input := newMCPStoreSubscription(t, f, 92, now)
			input.Name = family
			if family == "msgvault.source_message_archived" {
				input.ScopeKind = "source"
				input.ScopeID = f.Source.ID
				input.Arguments = []byte(`{"include_from_me":false}`)
			}
			_, binding, err := f.Store.ResolveMCPSubscriptionResource(t.Context(), family, input.ScopeKind, input.ScopeID, []string{"message"}, "")
			require.NoError(err)
			sub, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ResourceBinding: binding, Now: now})
			require.NoError(err)
			persist := func(nativeID string) int64 {
				t.Helper()
				id, err := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID(nativeID).Build(), BodyText: sql.NullString{String: "Synthetic native body", Valid: true}})
				require.NoError(err)
				return id
			}
			first := persist("native-authorized-before-unknown")
			envelope := func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
				return []byte(`{"eventId":"synthetic-permission-pause"}`), nil
			}
			pending, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, envelope)
			require.NoError(err)
			require.NotNil(pending)
			require.NoError(f.Store.CheckMCPSubscription(t.Context(), sub.ID, sub.Generation, now))
			require.NoError(f.Store.CheckMCPDeliveryOccurrence(t.Context(), sub.ID, sub.Generation, pending.Event.Seq, now))
			_, err = f.Store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now)
			require.NoError(err)
			permission.Known = false
			permission.MessageRead = false
			require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), permission))
			require.Error(f.Store.CheckMCPResourceBinding(t.Context(), binding), "callback verification must recheck current native permission")
			require.Error(f.Store.ValidateMCPSubscriptionResource(t.Context(), sub), "renewal must reject unknown native authority")
			require.Error(f.Store.CheckMCPSubscription(t.Context(), sub.ID, sub.Generation, now), "webhook preparation must recheck current native permission")
			require.Error(f.Store.CheckMCPDeliveryOccurrence(t.Context(), sub.ID, sub.Generation, pending.Event.Seq, now), "an already pending webhook must not dial")
			_, err = f.Store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now)
			require.Error(err, "retained receipt read needs current native authority")
			read := false
			err = f.Store.ReadMCPEventMessage(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, first, now, func(*sql.Tx) error { read = true; return nil })
			require.Error(err)
			assert.False(read, "protected message reader must not run")
			var before int64
			require.NoError(f.Store.DB().QueryRow("SELECT head_seq FROM mcp_event_clock WHERE singleton=1").Scan(&before))
			silent := persist("native-archive-while-permission-unknown")
			archived, err := f.Store.GetMessage(silent)
			require.NoError(err)
			require.NotNil(archived)
			var after int64
			require.NoError(f.Store.DB().QueryRow("SELECT head_seq FROM mcp_event_clock WHERE singleton=1").Scan(&after))
			assert.Equal(before, after, "archive persistence must not publish without native permission")
			_, err = f.Store.DB().Exec(`CREATE TABLE synthetic_permission_poll_writes (n INTEGER NOT NULL)`)
			require.NoError(err)
			_, err = f.Store.DB().Exec(`INSERT INTO synthetic_permission_poll_writes VALUES (0)`)
			require.NoError(err)
			if f.Store.IsPostgreSQL() {
				_, err = f.Store.DB().Exec(`CREATE FUNCTION count_permission_poll_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE synthetic_permission_poll_writes SET n=n+1; RETURN NEW; END $$`)
				require.NoError(err)
			}
			for _, table := range []string{"archive_metadata", "mcp_event_clock", "mcp_event_subscriptions"} {
				statement := fmt.Sprintf(`CREATE TRIGGER count_permission_poll_%s AFTER UPDATE ON %s BEGIN UPDATE synthetic_permission_poll_writes SET n=n+1; END`, table, table)
				if f.Store.IsPostgreSQL() {
					statement = fmt.Sprintf(`CREATE TRIGGER count_permission_poll_%s AFTER UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION count_permission_poll_write()`, table, table)
				}
				_, err = f.Store.DB().Exec(statement)
				require.NoError(err)
			}
			next, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now.Add(time.Minute), envelope)
			require.NoError(err)
			assert.Nil(next, "unknown permission pauses delivery")
			var pollWrites int
			require.NoError(f.Store.DB().QueryRow(`SELECT n FROM synthetic_permission_poll_writes`).Scan(&pollWrites))
			assert.Zero(pollWrites, "paused delivery must not contend for archive writer fences")
			retained, err := f.Store.GetMCPSubscription(t.Context(), sub.ID)
			require.NoError(err)
			assert.Equal("active", retained.State)
			assert.Empty(retained.StopReason)
			assert.Equal(pending.Subscription.PendingEnvelope, retained.PendingEnvelope, "a pause must retain the same durable envelope")
			assert.Equal(sub.Generation, retained.Generation)
			same, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
			require.NoError(err)
			assert.Equal(source.Reference, same.Reference, "unknown is not evidence of native removal")
			permission.Known = true
			permission.MessageRead = true
			require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), permission))
			require.NoError(f.Store.CheckMCPResourceBinding(t.Context(), binding))
			resumed, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now.Add(2*time.Minute), envelope)
			require.NoError(err)
			require.NotNil(resumed)
			assert.Equal(pending.Event.Seq, resumed.Event.Seq)
			assert.Equal(pending.Subscription.PendingEnvelope, resumed.Subscription.PendingEnvelope)
			_, err = f.Store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now)
			assert.NoError(err)
		})
	}
}
