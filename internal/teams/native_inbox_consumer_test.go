package teams

import (
	"context"
	"encoding/json"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Local test helpers retain constructors for callbacks.
	Require "github.com/stretchr/testify/require" //nolint:importas // Local test helpers retain constructors for callbacks.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type nativeTeamsInboxFeed struct {
	selfPresent bool

	blockRead   <-chan struct{}
	readEntered chan<- struct{}

	scopes          []string
	afterReadScopes []string

	mu          sync.Mutex
	chats       []Chat
	messages    map[string][]map[string]any
	profile     string
	listFailure bool
	denied      map[string]bool
}

func newNativeTeamsInboxFeed(t *testing.T) (*nativeTeamsInboxFeed, *Importer, ImportOptions) {
	t.Helper()
	f := &nativeTeamsInboxFeed{profile: "owner@example.test", scopes: []string{"Chat.Read"}, chats: []Chat{{ID: "event-chat", ChatType: "group", Topic: "Synthetic event chat"}, {ID: "quiet-chat", ChatType: "oneOnOne", Topic: "Synthetic quiet chat"}}, messages: map[string][]map[string]any{}, denied: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.selfPresent && selfChatAbsent(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "synthetic-owner-id", "mail": f.profile, "userPrincipalName": f.profile})
		case r.URL.Path == "/me/chats":
			if f.listFailure {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": f.chats})
		case strings.HasPrefix(r.URL.Path, "/chats/") && strings.HasSuffix(r.URL.Path, "/members"):
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{"id": "synthetic-owner-id", "userId": "synthetic-owner-id", "email": "owner@example.test", "displayName": "Synthetic Owner"}}})
		case strings.HasPrefix(r.URL.Path, "/me/chats/") && strings.HasSuffix(r.URL.Path, "/messages"):
			chat := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/me/chats/"), "/messages")
			if f.denied[chat] {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if f.blockRead != nil {
				block, entered := f.blockRead, f.readEntered
				f.blockRead = nil
				f.readEntered = nil
				close(entered)
				select {
				case <-block:
				case <-r.Context().Done():
					return
				}
			}
			if f.afterReadScopes != nil {
				f.scopes = f.afterReadScopes
				f.afterReadScopes = nil
			}
			messages := f.messages[chat]
			if messages == nil {
				messages = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": messages})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: "teams", Kinds: []string{"message"}},
		{Family: "msgvault.source_message_archived", SourceType: "teams", Kinds: []string{"message"}},
		{Family: "msgvault.inbox_message_archived", SourceType: "teams", Kinds: []string{"message"}},
	}})
	Require.NoError(t, err)
	client := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000, WithGrantedScopesReader(func() ([]string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]string(nil), f.scopes...), nil
	}))
	return f, NewImporter(st, client), ImportOptions{Email: "owner@example.test"}
}

// Removing native scope observation or the transaction-bound exact membership
// loses quiet discovery or source/conversation/chat fanout respectively.
func TestNativeTeamsChatInventoryAndAtomicMembership(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f, imp, opts := newNativeTeamsInboxFeed(t)
	f.messages["event-chat"] = []map[string]any{nativeTeamsEventMessage("old", "2026-01-01T00:00:00Z", "Synthetic history", "sender@example.test")}
	run := func() { _, err := imp.Import(t.Context(), opts); Require.NoError(t, err) }
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	page := func() map[string]store.MCPResource {
		p, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: sum.SourceID})
		Require.NoError(t, err)
		out := map[string]store.MCPResource{}
		for _, r := range p.Resources {
			Assert.Equal(t, "teams_chat", r.ScopeKind)
			out[r.ProviderID] = r
		}
		return out
	}
	first := page()
	require.Len(first, 2)
	allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), sum.SourceID)
	require.NoError(err)
	require.True(allowed)
	f.mu.Lock()
	f.chats[0].Topic = "Renamed synthetic chat"
	f.mu.Unlock()
	run()
	assert.Equal(first["event-chat"].Reference, page()["event-chat"].Reference)
	f.mu.Lock()
	f.listFailure = true
	f.mu.Unlock()
	_, err = imp.Import(t.Context(), opts)
	require.Error(err)
	assert.Equal(first["quiet-chat"].Reference, page()["quiet-chat"].Reference, "failed native inventory preserves the quiet chat")
	f.mu.Lock()
	f.listFailure = false
	f.messages["event-chat"] = append(f.messages["event-chat"], nativeTeamsEventMessage("live", "2026-01-02T00:00:00Z", "Synthetic live chat arrival", "sender@example.test"))
	f.mu.Unlock()
	run()
	var count, memberships int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(3, count, "conversation, source and exact chat are separate occurrences")
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM native_message_scope_memberships`).Scan(&memberships))
	assert.Equal(2, memberships)
	var inboxID int64
	require.NoError(imp.store.DB().QueryRow(`SELECT scope_id FROM mcp_event_log WHERE family='msgvault.inbox_message_archived'`).Scan(&inboxID))
	assert.Equal(first["event-chat"].ID, inboxID)
	f.mu.Lock()
	f.chats = f.chats[1:]
	f.mu.Unlock()
	run()
	_, err = imp.store.GetMCPResource(t.Context(), "inbox", first["event-chat"].ID)
	require.Error(err, "native removal retires eligibility while archived messages remain")
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(2, count)
	f.mu.Lock()
	f.chats = append(f.chats, Chat{ID: "event-chat", ChatType: "group", Topic: "Restored synthetic chat"})
	f.mu.Unlock()
	run()
	assert.NotEqual(first["event-chat"].Reference, page()["event-chat"].Reference)
	assert.Equal(first["quiet-chat"].Reference, page()["quiet-chat"].Reference)
}

// An account profile is authoritative identity; neither the account label nor
// a successful archive endpoint may substitute another Graph account.
func TestNativeTeamsRejectsWrongNativeAccount(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f, imp, opts := newNativeTeamsInboxFeed(t)
	f.profile = "other@example.test"
	_, err := imp.Import(t.Context(), opts)
	require.ErrorContains(err, "native_authority_unavailable")
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Zero(count)
	source, err := imp.store.GetSourceByTypeAndIdentifier("teams", opts.Email)
	require.NoError(err)
	allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed)
}

// Native denial can arrive while ListChats still contains the chat. It must
// revoke the chat before another subscription or receipt can use that scope.
func TestNativeTeamsPositiveReadDenialRetiresExactChat(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f, imp, opts := newNativeTeamsInboxFeed(t)
	f.messages["event-chat"] = []map[string]any{nativeTeamsEventMessage("old", "2026-01-01T00:00:00Z", "Synthetic archive", "sender@example.test")}
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	page, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: sum.SourceID})
	require.NoError(err)
	require.Len(page.Resources, 2)
	resources := map[string]store.MCPResource{}
	for _, r := range page.Resources {
		resources[r.ProviderID] = r
	}
	f.mu.Lock()
	f.denied["event-chat"] = true
	f.mu.Unlock()
	second, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Positive(second.Errors)
	_, err = imp.store.GetMCPResource(t.Context(), "inbox", resources["event-chat"].ID)
	require.Error(err)
	quiet, err := imp.store.GetMCPResource(t.Context(), "inbox", resources["quiet-chat"].ID)
	require.NoError(err)
	assert.Equal(resources["quiet-chat"].Reference, quiet.Reference)
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(1, count)
}

// A refresh reducing Chat.Read to metadata-only access is applied before a
// fetched message can publish; the ordinary archive still retains that message.
func TestNativeTeamsMidflightReadGrantReductionPausesCapture(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f, imp, opts := newNativeTeamsInboxFeed(t)
	f.messages["event-chat"] = []map[string]any{nativeTeamsEventMessage("old", "2026-01-01T00:00:00Z", "Synthetic history", "sender@example.test")}
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	f.mu.Lock()
	f.afterReadScopes = []string{"Chat.ReadBasic"}
	f.messages["event-chat"] = append(f.messages["event-chat"], nativeTeamsEventMessage("live", "2026-01-02T00:00:00Z", "Synthetic fetched after reduction", "sender@example.test"))
	f.mu.Unlock()
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), sum.SourceID)
	require.NoError(err)
	assert.False(allowed)
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Zero(count)
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(2, count)
}

// Metadata enumeration is not proof that a positively denied message read is
// restored. Admission must remain closed while the next native read is pending.
func TestNativeTeamsDeniedChatWaitsForVerifiedReadRestoration(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	native, imp, opts := newNativeTeamsInboxFeed(t)
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	page, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: sum.SourceID})
	require.NoError(err)
	var chat store.MCPResource
	for _, r := range page.Resources {
		if r.ProviderID == "event-chat" {
			chat = r
		}
	}
	require.Positive(chat.ID)
	native.mu.Lock()
	native.denied["event-chat"] = true
	native.mu.Unlock()
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	_, err = imp.store.GetMCPResource(t.Context(), "inbox", chat.ID)
	require.Error(err)
	release, entered := make(chan struct{}), make(chan struct{})
	native.mu.Lock()
	native.denied["event-chat"] = false
	native.blockRead = release
	native.readEntered = entered
	native.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := imp.Import(t.Context(), opts); done <- err }()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		close(release)
		require.FailNow("native read did not reach the restoration boundary")
	}
	_, lookupErr := imp.store.GetMCPResource(t.Context(), "inbox", chat.ID)
	require.Error(lookupErr, "listing a denied chat must not restore its read authority before the actual native read succeeds")
	close(release)
	select {
	case runErr := <-done:
		require.NoError(runErr)
	case <-time.After(15 * time.Second):
		require.FailNow("native read restoration did not finish")
	}
	restored, err := imp.store.GetMCPResource(t.Context(), "inbox", chat.ID)
	require.NoError(err)
	assert.NotEqual(chat.Reference, restored.Reference)
}

// A native response naming another chat cannot be assigned to the selected
// chat by its URL alone. Preserve the prior archive and publish no occurrence.
func TestNativeTeamsRejectsConflictingResponseChatIdentity(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	native, imp, opts := newNativeTeamsInboxFeed(t)
	native.messages["event-chat"] = []map[string]any{nativeTeamsEventMessage("old", "2026-01-01T00:00:00Z", "Synthetic history", "sender@example.test")}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	foreign := nativeTeamsEventMessage("foreign", "2026-01-02T00:00:00Z", "Synthetic conflicting native chat", "sender@example.test")
	foreign["chatId"] = "other-chat"
	native.mu.Lock()
	native.messages["event-chat"] = append(native.messages["event-chat"], foreign)
	native.mu.Unlock()
	_, err = imp.Import(t.Context(), opts)
	require.ErrorContains(err, "native_chat_identity_mismatch")
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(1, count)
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Zero(count)
}

// A successful native empty self-chat read proves a quiet chat exists. Its
// first later message is live after that completed empty history boundary.
func TestNativeTeamsEmptySelfChatDiscoveryAndFirstArrival(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	native, imp, opts := newNativeTeamsInboxFeed(t)
	native.selfPresent = true
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	page, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: sum.SourceID})
	require.NoError(err)
	var self store.MCPResource
	for _, r := range page.Resources {
		if r.ProviderID == SelfChatID {
			self = r
		}
	}
	require.Positive(self.ID, "an actual 200 empty self-chat must be discoverable without an archived message")
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Zero(count)
	native.mu.Lock()
	native.messages[SelfChatID] = []map[string]any{nativeTeamsEventMessage("self-first", "2026-01-02T00:00:00Z", "Synthetic first self note", "owner@example.test")}
	native.mu.Unlock()
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(3, count, "conversation, source and self-chat observe one new native arrival")
	var inboxID int64
	require.NoError(imp.store.DB().QueryRow(`SELECT scope_id FROM mcp_event_log WHERE family='msgvault.inbox_message_archived'`).Scan(&inboxID))
	assert.Equal(self.ID, inboxID)
	current, err := imp.store.GetMCPResource(t.Context(), "inbox", self.ID)
	require.NoError(err)
	assert.Equal(self.Reference, current.Reference)
}
