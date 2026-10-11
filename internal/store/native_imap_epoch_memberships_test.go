package store_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativeIMAPMembershipRequiresCurrentEpochAtomically(t *testing.T) {
	for _, mode := range []string{"live", "history", "wrong_epoch", "missing_epoch", "noncanonical_epoch", "overflow_epoch", "reset", "removed", "membership_write_failure"} {
		t.Run(mode, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("imap", "imaps://owner%40example.net@imap.example.net:993")
			require.NoError(err)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "imap", NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
			inventory := store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "imap", NativeIdentity: source.Identifier, Kind: "imap_mailbox", Complete: true, Scopes: []store.NativeMessageScope{{ProviderID: "Work", DisplayName: "Work", Epoch: "42"}}}
			require.NoError(st.ObserveNativeMessageScopes(t.Context(), inventory))
			conversation, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic thread")
			require.NoError(err)
			_, err = st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "imap", Kinds: []string{"message"}}, {Family: "msgvault.source_message_archived", SourceType: "imap", Kinds: []string{"message"}}, {Family: "msgvault.inbox_message_archived", SourceType: "imap", Kinds: []string{"message"}}}})
			require.NoError(err)
			data := &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "Work|1", MessageType: "email"}, BodyText: sql.NullString{String: "synthetic complete body", Valid: true}, RawMIME: []byte("Subject: synthetic\r\n\r\nsynthetic complete body"), NativeScopes: &store.NativeMessageScopeMembership{SourceReference: resource.Reference, Scopes: []store.NativeMessageScopeIdentity{{Kind: "imap_mailbox", ProviderID: "Work", Epoch: "42"}}}}
			ingest := store.IngestLive
			switch mode {
			case "history":
				ingest = store.IngestBackfill
			case "wrong_epoch":
				data.NativeScopes.Scopes[0].Epoch = "41"
			case "missing_epoch":
				data.NativeScopes.Scopes[0].Epoch = ""
			case "noncanonical_epoch":
				data.NativeScopes.Scopes[0].Epoch = "042"
			case "overflow_epoch":
				data.NativeScopes.Scopes[0].Epoch = "4294967296"
			case "reset":
				inventory.Scopes[0].Epoch = "43"
				require.NoError(st.ObserveNativeMessageScopes(t.Context(), inventory))
			case "removed":
				inventory.Scopes = nil
				require.NoError(st.ObserveNativeMessageScopes(t.Context(), inventory))
			case "membership_write_failure":
				if st.IsPostgreSQL() {
					_, err = st.DB().Exec(`CREATE FUNCTION reject_native_imap_membership() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic membership failure'; END $$`)
					require.NoError(err)
					_, err = st.DB().Exec(`CREATE TRIGGER reject_native_imap_membership BEFORE INSERT ON native_message_scope_memberships FOR EACH ROW EXECUTE FUNCTION reject_native_imap_membership()`)
				} else {
					_, err = st.DB().Exec(`CREATE TRIGGER reject_native_imap_membership BEFORE INSERT ON native_message_scope_memberships BEGIN SELECT RAISE(ABORT,'synthetic membership failure'); END`)
				}
				require.NoError(err)
			}
			_, err = st.WithIngestContext(store.IngestContext{Mode: ingest}).PersistMessageContext(t.Context(), data)
			if mode != "live" && mode != "history" {
				require.Error(err)
				for _, table := range []string{"messages", "message_bodies", "message_raw", "native_message_scope_memberships", "mcp_event_log"} {
					var count int
					require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count))
					assert.Zero(count, "a stale or failed mailbox membership must roll back "+table)
				}
				return
			}
			require.NoError(err)
			var memberships, events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM native_message_scope_memberships`).Scan(&memberships))
			assert.Equal(1, memberships)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			if mode == "history" {
				assert.Zero(events)
			} else {
				assert.Equal(3, events)
			}
		})
	}
}
