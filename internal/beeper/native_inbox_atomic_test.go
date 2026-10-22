package beeper

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Constructors remain available inside callbacks.
	Require "github.com/stretchr/testify/require" //nolint:importas // Constructors remain available inside callbacks.
	"go.kenn.io/msgvault/internal/store"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativeBeeperReadCannotBorrowReplacementRoute(t *testing.T) {
	for _, stage := range []string{"detail", "messages"} {
		t.Run(stage, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f := newNativeBeeperInboxFeed(t)
			sum, err := f.imp.Import(t.Context(), f.opts)
			require.NoError(err)
			source, err := f.imp.store.GetMCPResource(t.Context(), "source", sum.SourceID)
			require.NoError(err)
			before, found, err := f.imp.store.LookupNativeMessageScope(t.Context(), sum.SourceID, source.Reference, "beeper_member", f.chat.ID)
			require.NoError(err)
			require.True(found)
			f.fake.appendMsg(f.chat.ID, fakeMsg{ID: "synthetic-raced-live", SortKey: 100, Timestamp: time.Now().UTC(), Text: "Synthetic stale native route", SenderID: "@events-sender:example.test"})
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			f.blockPath = "/v1/chats/" + f.chat.ID
			if stage == "messages" {
				f.blockPath += "/messages"
			}
			f.blockEntered, f.blockRelease = entered, release
			done := make(chan error, 1)
			go func() { _, importErr := f.imp.Import(t.Context(), f.opts); done <- importErr }()
			select {
			case <-entered:
			case <-time.After(30 * time.Second):
				require.FailNow("native read did not reach the observed barrier")
			}
			observation := store.NativeMessageScopeInventory{SourceID: sum.SourceID, SourceReference: source.Reference, SourceType: "beeper", NativeIdentity: f.opts.AccountID, Kind: "beeper_member", ParentProviderID: f.opts.AccountID, RetiredProviderIDs: []string{f.chat.ID}}
			require.NoError(f.imp.store.ObserveNativeMessageScopes(t.Context(), observation))
			observation.RetiredProviderIDs = nil
			observation.Scopes = []store.NativeMessageScope{{ProviderID: f.chat.ID, DisplayName: "Restored synthetic member route"}}
			require.NoError(f.imp.store.ObserveNativeMessageScopes(t.Context(), observation))
			replacement, found, err := f.imp.store.LookupNativeMessageScope(t.Context(), sum.SourceID, source.Reference, "beeper_member", f.chat.ID)
			require.NoError(err)
			require.True(found)
			require.NotEqual(before.Identity.Generation, replacement.Identity.Generation)
			unblock()
			select {
			case importErr := <-done:
				require.Error(importErr, "an old response cannot capture into the replacement route")
			case <-time.After(30 * time.Second):
				require.FailNow("native importer did not settle after response release")
			}
			current, found, err := f.imp.store.LookupNativeMessageScope(t.Context(), sum.SourceID, source.Reference, "beeper_member", f.chat.ID)
			require.NoError(err)
			require.True(found)
			assert.Equal(replacement.Identity.Generation, current.Identity.Generation)
			assert.True(current.Present)
			var messages, events int
			require.NoError(f.imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='synthetic-raced-live'`).Scan(&messages))
			assert.Zero(messages)
			require.NoError(f.imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Zero(events)
		})
	}
}

func TestNativeBeeperMembershipFailureRollsBackMessage(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f := newNativeBeeperInboxFeed(t)
	_, err := f.imp.Import(t.Context(), f.opts)
	require.NoError(err)
	if f.imp.store.IsPostgreSQL() {
		_, err = f.imp.store.DB().Exec(`CREATE FUNCTION reject_native_member() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic native membership failure'; END; $$`)
		require.NoError(err)
		_, err = f.imp.store.DB().Exec(`CREATE TRIGGER reject_native_member BEFORE INSERT ON native_message_scope_memberships FOR EACH ROW EXECUTE FUNCTION reject_native_member()`)
		require.NoError(err)
	} else {
		_, err = f.imp.store.DB().Exec(`CREATE TRIGGER reject_native_member BEFORE INSERT ON native_message_scope_memberships BEGIN SELECT RAISE(ABORT,'synthetic native membership failure'); END`)
		require.NoError(err)
	}
	f.fake.appendMsg(f.chat.ID, fakeMsg{ID: "synthetic-rollback-live", SortKey: 100, Timestamp: time.Now().UTC(), Text: "Synthetic rejected native membership", SenderID: "@events-sender:example.test"})
	_, err = f.imp.Import(t.Context(), f.opts)
	require.ErrorContains(err, "synthetic native membership failure")
	var messages, events int
	require.NoError(f.imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='synthetic-rollback-live'`).Scan(&messages))
	assert.Zero(messages)
	require.NoError(f.imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events)
}

func TestNativeBeeperDisabledCapturePausesWithoutExtraRouteIO(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f := newNativeBeeperInboxFeed(t)
	sum, err := f.imp.Import(t.Context(), f.opts)
	require.NoError(err)
	source, err := f.imp.store.GetMCPResource(t.Context(), "source", sum.SourceID)
	require.NoError(err)
	countRoutes := func() int {
		f.fake.mu.Lock()
		defer f.fake.mu.Unlock()
		count := 0
		for _, path := range f.fake.reqs {
			if strings.HasPrefix(path, "/v1/chats/"+f.chat.ID+"?") {
				count++
			}
		}
		return count
	}
	before := countRoutes()
	_, err = f.imp.store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: false, Principal: "synthetic-owner"})
	require.NoError(err)
	_, err = f.imp.Import(t.Context(), f.opts)
	require.NoError(err)
	assert.Equal(before, countRoutes(), "disabled Events do not add native route reads")
	allowed, err := f.imp.store.NativeSourceMessagePermission(t.Context(), sum.SourceID)
	require.NoError(err)
	assert.False(allowed)
	current, err := f.imp.store.GetMCPResource(t.Context(), "source", sum.SourceID)
	require.NoError(err)
	assert.Equal(source.Reference, current.Reference)
}
