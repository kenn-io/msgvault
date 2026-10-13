package beeper

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Constructors remain available in nested tests.
	Require "github.com/stretchr/testify/require" //nolint:importas // Constructors remain available in nested tests.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type nativeBeeperInboxFeed struct {
	emptyAccount, wrongSearchAccount    bool
	blockPath                           string
	blockEntered, blockRelease          chan struct{}
	credentialStatus, enumerationStatus int
	fake                                *fakeBeeper
	chat                                *fakeChat
	imp                                 *Importer
	opts                                ImportOptions
	detailStatus                        int
	messageStatus                       int
	wrongAccount                        bool
	wrongChat                           bool
}

func newNativeBeeperInboxFeed(t *testing.T) *nativeBeeperInboxFeed {
	t.Helper()
	require := Require.New(t)
	f := &nativeBeeperInboxFeed{fake: newFakeBeeper(t)}
	f.chat = nativeEventsChat("synthetic-login-a", "!member-a:example.org")
	f.fake.addChat(f.chat)
	quiet := nativeEventsChat(f.chat.AccountID, "!quiet:example.org")
	quiet.Msgs = nil
	quiet.LastActivity = time.Now().UTC().Add(-100 * 24 * time.Hour)
	f.fake.addChat(quiet)
	other := nativeEventsChat("synthetic-login-b", "!member-b:example.org")
	f.fake.addChat(other)
	provider := f.fake.handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if f.blockPath != "" && r.URL.Path == f.blockPath {
			close(f.blockEntered)
			<-f.blockRelease
		}
		if f.credentialStatus != 0 || r.URL.Path == "/v1/chats/search" && f.enumerationStatus != 0 {
			status := f.credentialStatus
			if status == 0 {
				status = f.enumerationStatus
			}
			w.WriteHeader(status)
			Assert.NoError(t, json.MarshalWrite(w, map[string]any{"error": "synthetic selected credential failure"}))
			return
		}
		if r.URL.Path == "/v1/chats/"+f.chat.ID && f.detailStatus != 0 || r.URL.Path == "/v1/chats/"+f.chat.ID+"/messages" && f.messageStatus != 0 {
			status := f.detailStatus
			if strings.HasSuffix(r.URL.Path, "/messages") {
				status = f.messageStatus
			}
			w.WriteHeader(status)
			Assert.NoError(t, json.MarshalWrite(w, map[string]any{"error": "synthetic native read failure"}))
			return
		}
		if r.URL.Path == "/v1/chats/"+f.chat.ID && (f.wrongAccount || f.wrongChat || f.emptyAccount) {
			native := httptest.NewRecorder()
			provider(native, r)
			var value map[string]any
			if !Assert.NoError(t, json.Unmarshal(native.Body.Bytes(), &value)) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if f.emptyAccount {
				delete(value, "accountID")
			}
			if f.wrongAccount {
				value["accountID"] = "synthetic-login-b"
			}
			if f.wrongChat {
				value["id"] = "!different:example.org"
			}
			w.Header().Set("Content-Type", "application/json")
			Assert.NoError(t, json.MarshalWrite(w, value))
			return
		}
		if f.wrongSearchAccount && r.URL.Path == "/v1/chats/search" {
			native := httptest.NewRecorder()
			provider(native, r)
			var value map[string]any
			if !Assert.NoError(t, json.Unmarshal(native.Body.Bytes(), &value)) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			items, ok := value["items"].([]any)
			if !Assert.True(t, ok) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			for _, item := range items {
				row, ok := item.(map[string]any)
				if !Assert.True(t, ok) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if row["id"] == f.chat.ID {
					row["accountID"] = "synthetic-login-b"
				}
			}
			w.Header().Set("Content-Type", "application/json")
			Assert.NoError(t, json.MarshalWrite(w, value))
			return
		}
		provider(w, r)
	}))
	t.Cleanup(server.Close)
	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: "beeper", Kinds: []string{"message"}},
		{Family: "msgvault.source_message_archived", SourceType: "beeper", Kinds: []string{"message"}},
		{Family: "msgvault.inbox_message_archived", SourceType: "beeper", Kinds: []string{"message"}},
	}})
	require.NoError(err)
	f.imp = NewImporter(st, NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-native-token", nil }, 10000))
	f.opts = ImportOptions{AccountID: f.chat.AccountID, NoMedia: true}
	return f
}

func TestNativeBeeperInboxPinnedMemberRoute(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f := newNativeBeeperInboxFeed(t)
	first, err := f.imp.Import(t.Context(), f.opts)
	require.NoError(err)
	resources := func() map[string]store.MCPResource {
		page, err := f.imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: first.SourceID})
		Require.NoError(t, err)
		out := map[string]store.MCPResource{}
		for _, row := range page.Resources {
			out[row.ProviderID] = row
		}
		return out
	}
	initial := resources()
	require.Contains(initial, f.chat.ID)
	require.Contains(initial, "!quiet:example.org")
	require.Len(initial, 2, "another login on the same network cannot supply this source's route")
	assert.Equal("beeper_member", initial[f.chat.ID].ScopeKind)
	assert.Equal(f.opts.AccountID, initial[f.chat.ID].ParentProviderID)
	var history int
	require.NoError(f.imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&history))
	assert.Zero(history)
	f.fake.mu.Lock()
	f.chat.Title = "Renamed synthetic member route"
	f.fake.mu.Unlock()
	f.fake.appendMsg(f.chat.ID, fakeMsg{ID: "synthetic-live-member", SortKey: 100, Timestamp: time.Now().UTC(), Text: "Synthetic live pinned route", SenderID: "@events-sender:example.test"})
	_, err = f.imp.Import(t.Context(), f.opts)
	require.NoError(err)
	current := resources()
	assert.Equal(initial[f.chat.ID].Reference, current[f.chat.ID].Reference, "rename cannot change the account/chat route")
	assert.Equal(initial["!quiet:example.org"].Reference, current["!quiet:example.org"].Reference, "an activity-window omission cannot retire a quiet route")
	var selected, foreign int
	require.NoError(f.imp.store.DB().QueryRow(f.imp.store.Rebind(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.inbox_message_archived' AND scope_id=?`), initial[f.chat.ID].ID).Scan(&selected))
	assert.Equal(1, selected)
	require.NoError(f.imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.inbox_message_archived'`).Scan(&foreign))
	assert.Equal(1, foreign)
	var parents string
	require.NoError(f.imp.store.DB().QueryRow(`SELECT n.parent_provider_id FROM native_message_scope_memberships m JOIN native_message_scopes n ON n.id=m.scope_id WHERE m.message_id=(SELECT id FROM messages WHERE source_message_id='synthetic-live-member')`).Scan(&parents))
	assert.Equal(f.opts.AccountID, parents, "the exact route is committed with the message")
}

func TestNativeBeeperRejectsForeignSelectedMemberResponse(t *testing.T) {
	for _, mode := range []string{"account", "chat"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f := newNativeBeeperInboxFeed(t)
			f.wrongAccount = mode == "account"
			f.wrongChat = mode == "chat"
			_, err := f.imp.Import(t.Context(), f.opts)
			require.Error(err, "native route response must match the selected account and requested chat")
			var messages int
			require.NoError(f.imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			assert.Zero(messages)
			source, err := f.imp.store.GetOrCreateSource("beeper", f.opts.AccountID)
			require.NoError(err)
			allowed, err := f.imp.store.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed)
		})
	}
}

func TestNativeBeeperCurrentReadAuthority(t *testing.T) {
	for _, mode := range []string{"detail_denied", "detail_gone", "messages_denied", "detail_unknown", "messages_unknown", "messages_unknown_with_later_route", "wrong_chat", "empty_account", "wrong_search_account"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f := newNativeBeeperInboxFeed(t)
			sum, err := f.imp.Import(t.Context(), f.opts)
			require.NoError(err)
			source, err := f.imp.store.GetMCPResource(t.Context(), "source", sum.SourceID)
			require.NoError(err)
			before, found, err := f.imp.store.LookupNativeMessageScope(t.Context(), sum.SourceID, source.Reference, "beeper_member", f.chat.ID)
			require.NoError(err)
			require.True(found)
			f.fake.appendMsg(f.chat.ID, fakeMsg{ID: "synthetic-denied-live", SortKey: 100, Timestamp: time.Now().UTC(), Text: "Synthetic rejected native read", SenderID: "@events-sender:example.test"})
			switch mode {
			case "detail_denied":
				f.detailStatus = http.StatusForbidden
			case "detail_gone":
				f.detailStatus = http.StatusNotFound
			case "messages_denied":
				f.messageStatus = http.StatusForbidden
			case "detail_unknown":
				f.detailStatus = http.StatusBadRequest
			case "messages_unknown":
				f.messageStatus = http.StatusBadRequest
			case "messages_unknown_with_later_route":
				f.messageStatus = http.StatusBadRequest
				f.fake.mu.Lock()
				for _, chat := range f.fake.chats {
					if chat.ID == "!quiet:example.org" {
						chat.LastActivity = time.Now().UTC().Add(-time.Minute)
					}
				}
				f.fake.mu.Unlock()
			case "wrong_chat":
				f.wrongChat = true
			case "empty_account":
				f.emptyAccount = true
			case "wrong_search_account":
				f.wrongSearchAccount = true
			}
			_, err = f.imp.Import(t.Context(), f.opts)
			require.Error(err)
			current, found, err := f.imp.store.LookupNativeMessageScope(t.Context(), sum.SourceID, source.Reference, "beeper_member", f.chat.ID)
			require.NoError(err)
			require.True(found)
			allowed, err := f.imp.store.NativeSourceMessagePermission(t.Context(), sum.SourceID)
			require.NoError(err)
			if mode == "detail_denied" || mode == "detail_gone" || mode == "messages_denied" {
				assert.False(current.Present, "positive native denial retires the affected member route")
				assert.True(allowed, "a single chat denial does not revoke another route on the same account")
			} else {
				assert.True(current.Present, "unknown or inconsistent native evidence is not removal")
				assert.Equal(before.Identity.Generation, current.Identity.Generation)
				assert.False(allowed, "unknown current native read permission pauses persisted authority")
			}
			var messages, events int
			require.NoError(f.imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='synthetic-denied-live'`).Scan(&messages))
			assert.Zero(messages)
			require.NoError(f.imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Zero(events)
		})
	}
}

func TestNativeBeeperEarlyReadFailureStopsOldAuthority(t *testing.T) {
	for _, mode := range []string{"credential_denied", "enumeration_unknown"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f := newNativeBeeperInboxFeed(t)
			sum, err := f.imp.Import(t.Context(), f.opts)
			require.NoError(err)
			original, err := f.imp.store.GetMCPResource(t.Context(), "source", sum.SourceID)
			require.NoError(err)
			if mode == "credential_denied" {
				f.credentialStatus = http.StatusUnauthorized
			} else {
				f.enumerationStatus = http.StatusBadRequest
			}
			_, err = f.imp.Import(t.Context(), f.opts)
			require.Error(err)
			allowed, err := f.imp.store.NativeSourceMessagePermission(t.Context(), sum.SourceID)
			require.NoError(err)
			assert.False(allowed, "failure before syncChat cannot retain prior Events authority")
			current, err := f.imp.store.GetMCPResource(t.Context(), "source", sum.SourceID)
			require.NoError(err)
			if mode == "credential_denied" {
				assert.NotEqual(original.Reference, current.Reference, "positive native token rejection retires the old account authority")
			} else {
				assert.Equal(original.Reference, current.Reference, "unknown enumeration is not account removal")
			}
		})
	}
}
