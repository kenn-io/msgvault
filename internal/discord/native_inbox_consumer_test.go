package discord

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available to callback assertions.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available to callback assertions.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type nativeDiscordInboxFeed struct {
	mu                 sync.Mutex
	bot                string
	noHistory          bool
	permissionRequests int
	blockMessages      <-chan struct{}
	messagesEntered    chan<- struct{}
	guildStatus        int
	guildID            string
	blockGuild         <-chan struct{}
	guildEntered       chan<- struct{}
	channels           []Channel
	threads            []Channel
	messages           map[string][]Message
	denied             map[string]bool
	listFailure        bool
}

func newNativeDiscordInboxFeed(t *testing.T) (*nativeDiscordInboxFeed, *Importer, ImportOptions) {
	t.Helper()
	f := &nativeDiscordInboxFeed{
		bot:      "101",
		channels: []Channel{{ID: "300", GuildID: "200", Type: channelTypeGuildText, Name: "Synthetic channel"}, {ID: "301", GuildID: "200", Type: channelTypeGuildText, Name: "Synthetic quiet channel"}},
		threads:  []Channel{{ID: "400", GuildID: "200", Type: channelTypePublicThread, ParentID: "300", Name: "Synthetic thread"}},
		messages: map[string][]Message{}, denied: map[string]bool{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/users/@me":
			_ = json.NewEncoder(w).Encode(User{ID: f.bot, Bot: true, Username: "synthetic-bot"})
		case r.URL.Path == "/guilds/200":
			if f.blockGuild != nil {
				block, entered := f.blockGuild, f.guildEntered
				f.blockGuild = nil
				f.guildEntered = nil
				close(entered)
				select {
				case <-block:
				case <-r.Context().Done():
					return
				}
			}
			if f.guildStatus != 0 {
				w.WriteHeader(f.guildStatus)
				_, _ = w.Write([]byte(`{"code":50001}`))
				return
			}
			guildID := f.guildID
			if guildID == "" {
				guildID = "200"
			}
			_ = json.NewEncoder(w).Encode(Guild{ID: guildID, Name: "Synthetic guild"})
		case r.URL.Path == "/guilds/200/members/101":
			f.permissionRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"user": User{ID: "101", Bot: true}, "roles": []string{}})
		case r.URL.Path == "/guilds/200/roles":
			f.permissionRequests++
			permissions := "66560"
			if f.noHistory {
				permissions = "1024"
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "200", "permissions": permissions}})
		case r.URL.Path == "/guilds/200/channels":
			if f.listFailure {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(f.channels)
		case r.URL.Path == "/guilds/200/threads/active":
			_ = json.NewEncoder(w).Encode(map[string]any{"threads": f.threads})
		case strings.Contains(r.URL.Path, "/threads/archived/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"threads": []Channel{}, "has_more": false})
		case strings.HasSuffix(r.URL.Path, "/messages"):
			channel := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/channels/"), "/messages")
			if channel == "400" && f.blockMessages != nil {
				block, entered := f.blockMessages, f.messagesEntered
				f.blockMessages = nil
				f.messagesEntered = nil
				close(entered)
				select {
				case <-block:
				case <-r.Context().Done():
					return
				}
			}

			if f.denied[channel] {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":50001}`))
				return
			}
			var messages []Message
			for _, m := range f.messages[channel] {
				id, _ := strconv.ParseUint(m.ID, 10, 64)
				before, _ := strconv.ParseUint(r.URL.Query().Get("before"), 10, 64)
				after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
				if (before == 0 || id < before) && (after == 0 || id > after) {
					messages = append(messages, m)
				}
			}
			sort.Slice(messages, func(i, j int) bool {
				a, _ := strconv.ParseUint(messages[i].ID, 10, 64)
				b, _ := strconv.ParseUint(messages[j].ID, 10, 64)
				return a > b
			})
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit > 0 && len(messages) > limit {
				messages = messages[:limit]
			}
			if messages == nil {
				messages = []Message{}
			}
			_ = json.NewEncoder(w).Encode(messages)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: "discord", Kinds: []string{"message"}},
		{Family: "msgvault.source_message_archived", SourceType: "discord", Kinds: []string{"message"}},
		{Family: "msgvault.inbox_message_archived", SourceType: "discord", Kinds: []string{"message"}},
	}})
	Require.NoError(t, err)
	client, err := NewClient(srv.URL, "synthetic-token")
	Require.NoError(t, err)
	return f, NewImporter(st, client), ImportOptions{GuildID: "200", BotUserID: "101"}
}

// Omitting native inventory loses quiet resources. Omitting transaction-bound
// channel/thread membership loses the exact inbox occurrences for a live reply.
func TestNativeDiscordInboxInventoryAndAtomicMembership(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f, imp, opts := newNativeDiscordInboxFeed(t)
	now, err := TimestampFromSnowflake("1500000000000000000")
	require.NoError(err)
	imp.now = func() time.Time { return now.Add(time.Hour) }
	f.messages["400"] = []Message{nativeDiscordInboxMessage("1500000000000000000", "400", "Synthetic history")}
	summary, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	resources := func() map[string]store.MCPResource {
		p, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: summary.SourceID})
		Require.NoError(t, err)
		out := map[string]store.MCPResource{}
		for _, resource := range p.Resources {
			out[resource.ProviderID] = resource
		}
		return out
	}
	first := resources()
	require.Len(first, 3)
	allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), summary.SourceID)
	require.NoError(err)
	require.True(allowed, "current native bot and guild membership establish source authority")
	assert.Equal("discord_channel", first["300"].ScopeKind)
	assert.Equal("discord_channel", first["301"].ScopeKind)
	assert.Equal("discord_thread", first["400"].ScopeKind)
	assert.Equal("300", first["400"].ParentProviderID)
	run := func() { _, err := imp.Import(t.Context(), opts); Require.NoError(t, err) }
	f.mu.Lock()
	f.channels[0].Name = "Renamed synthetic channel"
	f.mu.Unlock()
	run()
	assert.Equal(first["300"].Reference, resources()["300"].Reference)
	f.mu.Lock()
	f.listFailure = true
	f.mu.Unlock()
	_, err = imp.Import(t.Context(), opts)
	require.Error(err)
	assert.Equal(first["301"].Reference, resources()["301"].Reference, "failed enumeration does not retire a quiet channel")
	f.mu.Lock()
	f.listFailure = false
	f.threads = nil
	f.messages["400"] = append(f.messages["400"], nativeDiscordInboxMessage("1500000000000000001", "400", "Synthetic live reply"))
	f.mu.Unlock()
	run()
	assert.Equal(first["400"].Reference, resources()["400"].Reference, "an incremental thread catalog does not prove omitted quiet threads gone")
	var occurrences int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&occurrences))
	assert.Equal(4, occurrences, "conversation, source, parent channel, and exact thread each capture once")
	var scopes []string
	rows, err := imp.store.DB().Query(`SELECT scope_kind FROM native_message_scopes WHERE id IN (SELECT scope_id FROM mcp_event_log WHERE family='msgvault.inbox_message_archived') ORDER BY scope_kind`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	for rows.Next() {
		var kind string
		require.NoError(rows.Scan(&kind))
		scopes = append(scopes, kind)
	}
	require.NoError(rows.Err())
	require.NoError(rows.Close())
	assert.Equal([]string{"discord_channel", "discord_thread"}, scopes)
}

// Archive rows and a configured bot ID cannot replace current native identity
// or read-history authority. An empty successful list is also returned when
// Discord denies READ_MESSAGE_HISTORY, so it must not admit quiet scopes.
func TestNativeDiscordInboxRequiresCurrentReadAuthority(t *testing.T) {
	for _, mode := range []string{"unknown_credential", "wrong_bot", "no_history"} {
		t.Run(mode, func(t *testing.T) {
			require := Require.New(t)
			f, imp, opts := newNativeDiscordInboxFeed(t)
			switch mode {
			case "unknown_credential":
				opts.BotUserID = ""
			case "wrong_bot":
				opts.BotUserID = "999"
			case "no_history":
				f.noHistory = true
			}
			summary, err := imp.Import(t.Context(), opts)
			if mode == "wrong_bot" {
				require.Error(err)
				return
			}
			require.NoError(err, "ordinary archiving stays available without Events authority")
			page, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: summary.SourceID})
			require.NoError(err)
			require.Empty(page.Resources)
			if mode == "unknown_credential" {
				_, _, err = imp.store.ValidateMCPEventScope(t.Context(), "msgvault.source_message_archived", "source", summary.SourceID, []string{"message"})
				require.Error(err, "unknown native credential must reject before receiver I/O")
			}
		})
	}
}

func nativeDiscordInboxMessage(id, channel, body string) Message {
	message := importerTestMessage(id, channel, body)
	message.Author = User{ID: "201", Username: "synthetic-sender"}
	return message
}

// Reusing the provider-local guild ID after an observed identity replacement
// must not let an old native response establish the replacement's authority.
func TestNativeDiscordPinsSourceBeforeProviderWork(t *testing.T) {
	require := Require.New(t)
	f, imp, opts := newNativeDiscordInboxFeed(t)
	summary, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	before, err := imp.store.GetMCPResource(t.Context(), "source", summary.SourceID)
	require.NoError(err)
	blocked, entered := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(blocked) }) })
	f.mu.Lock()
	f.blockGuild = blocked
	f.guildEntered = entered
	f.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := imp.Import(t.Context(), opts); done <- err }()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		require.FailNow("native guild request did not enter")
	}
	require.NoError(imp.store.UpdateSourceIdentifier(summary.SourceID, "201"))
	require.NoError(imp.store.UpdateSourceIdentifier(summary.SourceID, "200"))
	after, err := imp.store.GetMCPResource(t.Context(), "source", summary.SourceID)
	require.NoError(err)
	require.NotEqual(before.Reference, after.Reference)
	release.Do(func() { close(blocked) })
	select {
	case err := <-done:
		require.Error(err, "an obsolete native observation cannot authorize the replacement lifetime")
	case <-time.After(15 * time.Second):
		require.FailNow("native import did not terminate")
	}
}

// Events-only permission discovery must not change ordinary archival when the
// feature is disabled or only another source type is configured.
func TestNativeDiscordWithoutSelectedEventsDoesNotReadExtraPermissions(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f, imp, opts := newNativeDiscordInboxFeed(t)
			_, err := imp.store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: enabled, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "slack", Kinds: []string{"message"}}}})
			require.NoError(err)
			summary, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			f.mu.Lock()
			requests := f.permissionRequests
			f.mu.Unlock()
			assert.Zero(requests, "ordinary archival does not add Events-only member/role requests")
			allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), summary.SourceID)
			require.NoError(err)
			assert.False(allowed)
		})
	}
}

// A failed/invalid current native observation may never leave a previous
// process's read authority usable. Unknown evidence preserves lifetimes;
// positive account denial cannot revive old receipts on restoration.
func TestNativeDiscordCurrentPermissionObservationStopsOldAuthority(t *testing.T) {
	for _, mode := range []string{"failed_inventory", "malformed_overwrite", "guild_denied", "wrong_guild", "changed_bot"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f, imp, opts := newNativeDiscordInboxFeed(t)
			first, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			before, err := imp.store.GetMCPResource(t.Context(), "source", first.SourceID)
			require.NoError(err)
			page, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: first.SourceID})
			require.NoError(err)
			require.NotEmpty(page.Resources)
			inbox := page.Resources[0]
			f.mu.Lock()
			switch mode {
			case "failed_inventory":
				f.listFailure = true
				f.noHistory = true
			case "malformed_overwrite":
				f.channels[0].PermissionOverwrites = []PermissionOverwrite{{ID: "200", Type: 0, Allow: "invalid", Deny: "0"}}
			case "guild_denied":
				f.guildStatus = http.StatusForbidden
			case "wrong_guild":
				f.guildID = "201"
			case "changed_bot":
				f.bot = "102"
			}
			f.mu.Unlock()
			_, err = imp.Import(t.Context(), opts)
			require.Error(err)
			allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), first.SourceID)
			require.NoError(err)
			assert.False(allowed, "failed or invalid current evidence must pause the old native grant")
			after, err := imp.store.GetMCPResource(t.Context(), "source", first.SourceID)
			require.NoError(err)
			if mode == "failed_inventory" || mode == "malformed_overwrite" {
				assert.Equal(before.Reference, after.Reference, "unknown permission cannot assert source removal")
				retained, err := imp.store.GetMCPResource(t.Context(), "inbox", inbox.ID)
				require.NoError(err)
				assert.Equal(inbox.Reference, retained.Reference, "invalid metadata must not assert inbox removal")
			} else {
				assert.NotEqual(before.Reference, after.Reference, "positive native identity/permission denial retires old authority")
			}
		})
	}
}

// A readable forum/media parent is a native channel even before its first
// thread. Discord has no messages endpoint for the empty parent itself.
func TestNativeDiscordQuietForumAndMediaParents(t *testing.T) {
	for _, kind := range []int{channelTypeGuildForum, channelTypeGuildMedia} {
		t.Run(strconv.Itoa(kind), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f, imp, opts := newNativeDiscordInboxFeed(t)
			f.channels[0].Type = kind
			f.threads = nil
			first, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			p, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: first.SourceID})
			require.NoError(err)
			var ids []string
			for _, row := range p.Resources {
				ids = append(ids, row.ProviderID)
			}
			assert.ElementsMatch([]string{"300", "301"}, ids, "verified current parent permissions admit quiet forum/media channels")
		})
	}
}

// A response started before native revocation must not restore that revoked
// scope or borrow a later replacement lifetime when the response arrives.
func TestNativeDiscordReadPinsScopeBeforeProviderWork(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(strconv.FormatBool(replace), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f, imp, opts := newNativeDiscordInboxFeed(t)
			now, err := TimestampFromSnowflake("1500000000000000000")
			require.NoError(err)
			imp.now = func() time.Time { return now.Add(time.Hour) }
			f.messages["400"] = []Message{nativeDiscordInboxMessage("1500000000000000000", "400", "synthetic history")}
			first, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			resource, err := imp.store.GetMCPResource(t.Context(), "source", first.SourceID)
			require.NoError(err)
			unblock, entered := make(chan struct{}), make(chan struct{})
			f.mu.Lock()
			f.blockMessages = unblock
			f.messagesEntered = entered
			f.messages["400"] = append(f.messages["400"], nativeDiscordInboxMessage("1500000000000000001", "400", "synthetic stale response"))
			f.mu.Unlock()
			done := make(chan error, 1)
			go func() { _, err := imp.Import(t.Context(), opts); done <- err }()
			select {
			case <-entered:
			case <-time.After(15 * time.Second):
				close(unblock)
				require.FailNow("native message request did not start")
			}
			retired := store.NativeMessageScopeInventory{SourceID: first.SourceID, SourceReference: resource.Reference, SourceType: "discord", NativeIdentity: "200", Kind: "discord_thread", ParentProviderID: "300", RetiredProviderIDs: []string{"400"}}
			retireErr := imp.store.ObserveNativeMessageScopes(t.Context(), retired)
			var replaceErr error
			if replace && retireErr == nil {
				retired.RetiredProviderIDs = nil
				retired.Scopes = []store.NativeMessageScope{{ProviderID: "400", DisplayName: "Synthetic replacement thread"}}
				replaceErr = imp.store.ObserveNativeMessageScopes(t.Context(), retired)
			}
			close(unblock)
			require.NoError(retireErr)
			require.NoError(replaceErr)
			select {
			case err := <-done:
				require.Error(err, "the stale response must not restore or borrow the post-revocation scope")
			case <-time.After(15 * time.Second):
				require.FailNow("native importer did not stop")
			}
			var events, stale int
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Zero(events)
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='1500000000000000001'`).Scan(&stale))
			assert.Zero(stale, "mandatory native membership failure rolls back the message snapshot")
		})
	}
}

func TestNativeDiscordStaleReadDenialCannotRetireReplacement(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f, imp, opts := newNativeDiscordInboxFeed(t)
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	source, err := imp.store.GetMCPResource(t.Context(), "source", first.SourceID)
	require.NoError(err)
	unblock, entered := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.blockMessages = unblock
	f.messagesEntered = entered
	f.denied["400"] = true
	f.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := imp.Import(t.Context(), opts); done <- err }()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		close(unblock)
		require.FailNow("native message request did not start")
	}
	observation := store.NativeMessageScopeInventory{SourceID: first.SourceID, SourceReference: source.Reference, SourceType: "discord", NativeIdentity: "200", Kind: "discord_thread", ParentProviderID: "300", RetiredProviderIDs: []string{"400"}}
	retireErr := imp.store.ObserveNativeMessageScopes(t.Context(), observation)
	observation.RetiredProviderIDs = nil
	observation.Scopes = []store.NativeMessageScope{{ProviderID: "400", DisplayName: "Synthetic replacement thread"}}
	replaceErr := imp.store.ObserveNativeMessageScopes(t.Context(), observation)
	replacement, found, lookupErr := imp.store.LookupNativeMessageScope(t.Context(), first.SourceID, source.Reference, "discord_thread", "400")
	close(unblock)
	require.NoError(retireErr)
	require.NoError(replaceErr)
	require.NoError(lookupErr)
	require.True(found)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		require.FailNow("native importer did not stop")
	}
	current, found, err := imp.store.LookupNativeMessageScope(t.Context(), first.SourceID, source.Reference, "discord_thread", "400")
	require.NoError(err)
	require.True(found)
	assert.True(current.Present, "a stale native403 cannot retire the replacement lifetime")
	assert.Equal(replacement.Identity.Generation, current.Identity.Generation)
}
