package store_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestNativeGmailInboxMembershipAtomicCapture(t *testing.T) {
	for _, mode := range []string{"live", "history", "no_native_membership", "wrong_account", "unknown_label", "removed_label", "membership_write_failure", "move"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f := storetest.New(t)
			resource, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
			require.NoError(err)
			require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: f.Source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier, Known: true, MessageRead: true}))
			inventory := store.NativeMessageScopeInventory{SourceID: f.Source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier, Kind: "gmail_label", Complete: true, Scopes: []store.NativeMessageScope{{ProviderID: "INBOX", DisplayName: "Inbox"}, {ProviderID: "OTHER", DisplayName: "Other"}}}
			require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
			page, err := f.Store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: f.Source.ID})
			require.NoError(err)
			require.Len(page.Resources, 2)
			var inbox, other store.MCPResource
			for _, r := range page.Resources {
				if r.ProviderID == "INBOX" {
					inbox = r
				} else {
					other = r
				}
			}
			_, err = f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}, {Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"message"}}, {Family: "msgvault.inbox_message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
			require.NoError(err)
			data := &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID("synthetic-inbox-message").Build(), BodyText: sql.NullString{String: "Synthetic native body", Valid: true}, NativeScopes: &store.NativeMessageScopeMembership{SourceReference: resource.Reference, Scopes: []store.NativeMessageScopeIdentity{{Kind: "gmail_label", ProviderID: "INBOX"}}}}
			wantFailure := false
			wantEvents, wantInbox := 3, 1
			ingest := store.IngestLive
			switch mode {
			case "history":
				ingest = store.IngestBackfill
				wantEvents, wantInbox = 0, 0
			case "no_native_membership":
				data.NativeScopes = nil
				wantEvents, wantInbox = 2, 0
			case "wrong_account":
				foreign, err := f.Store.GetOrCreateSource("gmail", "foreign@example.net")
				require.NoError(err)
				foreignResource, err := f.Store.GetMCPResource(t.Context(), "source", foreign.ID)
				require.NoError(err)
				data.NativeScopes.SourceReference = foreignResource.Reference
				wantFailure = true
			case "unknown_label":
				data.NativeScopes.Scopes[0].ProviderID = "ARCHIVED_ONLY"
				_, err = f.Store.EnsureLabel(f.Source.ID, "ARCHIVED_ONLY", "Archived only", "user")
				require.NoError(err)
				wantFailure = true
			case "removed_label":
				inventory.Scopes = inventory.Scopes[1:]
				require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
				wantFailure = true
			case "membership_write_failure":
				if f.Store.IsPostgreSQL() {
					_, err = f.Store.DB().Exec(`CREATE FUNCTION reject_native_scope_membership() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic membership failure'; END $$`)
					require.NoError(err)
					_, err = f.Store.DB().Exec(`CREATE TRIGGER reject_native_scope_membership BEFORE INSERT ON native_message_scope_memberships FOR EACH ROW EXECUTE FUNCTION reject_native_scope_membership()`)
				} else {
					_, err = f.Store.DB().Exec(`CREATE TRIGGER reject_native_scope_membership BEFORE INSERT ON native_message_scope_memberships BEGIN SELECT RAISE(ABORT,'synthetic membership failure'); END`)
				}
				require.NoError(err)
				wantFailure = true
			}
			id, err := f.Store.WithIngestContext(store.IngestContext{Mode: ingest}).PersistMessageContext(t.Context(), data)
			if wantFailure {
				require.Error(err)
				for _, table := range []string{"messages", "message_bodies", "native_message_scope_memberships", "mcp_event_log"} {
					var n int
					require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n))
					assert.Zero(n, "failed native membership rolls back "+table)
				}
				return
			}
			require.NoError(err)
			require.Positive(id)
			if mode == "move" {
				data.NativeScopes.Scopes[0].ProviderID = "OTHER"
				again, err := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive}).PersistMessageContext(t.Context(), data)
				require.NoError(err)
				assert.Equal(id, again)
				var current int64
				require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT scope_id FROM native_message_scope_memberships WHERE message_id=?`), id).Scan(&current))
				assert.Equal(other.ID, current, "move updates current membership with the snapshot")
				var captured int64
				require.NoError(f.Store.DB().QueryRow(`SELECT scope_id FROM mcp_event_log WHERE family='msgvault.inbox_message_archived'`).Scan(&captured))
				assert.Equal(inbox.ID, captured, "move does not rewrite or append a captured inbox arrival")
			}
			var events, inboxEvents int
			require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Equal(wantEvents, events)
			require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.inbox_message_archived'`).Scan(&inboxEvents))
			assert.Equal(wantInbox, inboxEvents)
		})
	}
}

func TestNativeGmailInboxPartialAndStaleInventory(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f := storetest.New(t)
	resource, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	inventory := store.NativeMessageScopeInventory{SourceID: f.Source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier, Kind: "gmail_label", Complete: true, Scopes: []store.NativeMessageScope{{ProviderID: "label-a", DisplayName: "Alpha"}, {ProviderID: "label-b", DisplayName: "Beta"}}}
	require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
	page := func() []store.MCPResource {
		p, err := f.Store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: f.Source.ID})
		require.NoError(err)
		return p.Resources
	}
	first := page()
	require.Len(first, 2)
	inventory.Complete = false
	inventory.Scopes = inventory.Scopes[:1]
	require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
	assert.Len(page(), 2, "partial domain observation cannot retire a quiet native identity")
	inventory.Complete = true
	require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
	assert.Len(page(), 1)
	inventory.Scopes = append(inventory.Scopes, store.NativeMessageScope{ProviderID: "label-b", DisplayName: "Beta"})
	require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
	restored := page()
	require.Len(restored, 2)
	assert.Equal(first[1].ID, restored[1].ID)
	assert.NotEqual(first[1].Reference, restored[1].Reference)
	require.NoError(f.Store.UpdateSourceIdentifier(f.Source.ID, "replacement@example.net"))
	require.NoError(f.Store.UpdateSourceIdentifier(f.Source.ID, f.Source.Identifier))
	_, err = f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	require.Error(f.Store.ObserveNativeMessageScopes(t.Context(), inventory), "stale complete observation cannot authorize the replacement lifetime")
	assert.Empty(page(), "old presence cannot bridge an away-and-back account replacement")
}
