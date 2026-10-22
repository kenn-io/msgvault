package matrix

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Constructors remain available in nested callbacks.
	Require "github.com/stretchr/testify/require" //nolint:importas // Constructors remain available in nested callbacks.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

type nativeMatrixInboxFeed struct {
	syncEntered, syncRelease, memberEntered, memberRelease chan struct{}
	failurePath                                            string
	failureStatus                                          int
	failureEntered, failureRelease                         chan struct{}
	limited                                                bool
	device                                                 string
	memberStatus                                           int
	missingSelf                                            bool
	eventRoom                                              string
	whoamiRequests, syncRequests                           int

	mu                  sync.Mutex
	round               int
	quietOmitted, leave bool
	user                string
	root                string
	ordinary            bool
}

func newNativeMatrixInboxFeed(t *testing.T) (*nativeMatrixInboxFeed, *Importer, ImportOptions, int64) {
	t.Helper()
	require := Require.New(t)
	f := &nativeMatrixInboxFeed{user: "@archive:example.org", root: "$root", device: "synthetic-scope-device"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if f.failurePath != "" && r.URL.Path == f.failurePath {
			if f.failureEntered != nil {
				close(f.failureEntered)
				<-f.failureRelease
			}
			code := "M_UNKNOWN"
			if f.failureStatus == http.StatusUnauthorized {
				code = "M_UNKNOWN_TOKEN"
			}
			if f.failureStatus == http.StatusForbidden {
				code = "M_FORBIDDEN"
			}
			w.WriteHeader(f.failureStatus)
			Assert.NoError(t, json.MarshalWrite(w, map[string]any{"errcode": code, "error": "Synthetic read failure"}))
			return
		}

		var value any
		switch r.URL.Path {
		case "/_matrix/client/v3/account/whoami":
			f.whoamiRequests++
			value = map[string]any{"user_id": f.user, "device_id": f.device}
		case "/_matrix/client/v3/sync":
			f.syncRequests++
			if f.syncEntered != nil {
				close(f.syncEntered)
				<-f.syncRelease
			}
			eventID, body := "$root", "Synthetic native history"
			content := map[string]any{"msgtype": "m.text", "body": body}
			if f.round > 0 {
				eventID = fmt.Sprintf("$reply-%d", f.round)
				relation := map[string]any{"m.in_reply_to": map[string]any{"event_id": f.root}}
				if !f.ordinary {
					relation["rel_type"] = "m.thread"
					relation["event_id"] = f.root
					relation["is_falling_back"] = true
				}
				content = map[string]any{"msgtype": "m.text", "body": "Synthetic native reply", "m.relates_to": relation}
			}
			eventRoom := f.eventRoom
			if eventRoom == "" {
				eventRoom = "!room:example.org"
			}
			events := []any{map[string]any{"type": "m.room.message", "room_id": eventRoom, "event_id": eventID, "sender": "@sender:example.org", "origin_server_ts": int64(1700000000000) + int64(f.round), "content": content}}
			joined := map[string]any{
				"!room:example.org": map[string]any{"state": map[string]any{"events": []any{map[string]any{"type": "m.room.name", "state_key": "", "content": map[string]any{"name": "Synthetic room"}}}}, "timeline": map[string]any{"events": events, "limited": f.limited, "prev_batch": "native-page"}},
			}
			if !f.quietOmitted {
				joined["!quiet:example.org"] = map[string]any{"timeline": map[string]any{"events": []any{}}}
			}
			rooms := map[string]any{"join": joined}
			if f.leave {
				delete(joined, "!room:example.org")
				rooms["leave"] = map[string]any{"!room:example.org": map[string]any{"timeline": map[string]any{"events": []any{}}}}
			}
			value = map[string]any{"next_batch": fmt.Sprintf("native-sync-%d", f.round), "rooms": rooms}
		case "/_matrix/client/v3/rooms/!room:example.org/joined_members", "/_matrix/client/v3/rooms/!quiet:example.org/joined_members":
			if f.memberEntered != nil && r.URL.Path == "/_matrix/client/v3/rooms/!room:example.org/joined_members" {
				close(f.memberEntered)
				<-f.memberRelease
			}
			if r.URL.Path == "/_matrix/client/v3/rooms/!room:example.org/joined_members" && f.memberStatus != 0 {
				w.WriteHeader(f.memberStatus)
				_ = json.MarshalWrite(w, map[string]any{"errcode": "M_FORBIDDEN", "error": "Synthetic member read failure"})
				return
			}
			members := map[string]any{"@archive:example.org": map[string]any{"display_name": "Synthetic owner"}, "@sender:example.org": map[string]any{"display_name": "Synthetic sender"}}
			if f.missingSelf && r.URL.Path == "/_matrix/client/v3/rooms/!room:example.org/joined_members" {
				delete(members, "@archive:example.org")
			}
			value = map[string]any{"joined": members}
		case "/_matrix/client/v3/rooms/!room:example.org/messages", "/_matrix/client/v3/rooms/!quiet:example.org/messages":
			value = map[string]any{"chunk": []any{}, "end": ""}
		case "/_matrix/client/v3/user/@archive:example.org/account_data/m.direct":
			value = map[string]any{}
		default:
			http.NotFound(w, r)
			return
		}
		Assert.NoError(t, json.MarshalWrite(w, value))
	}))
	t.Cleanup(server.Close)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	_, err = st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: SourceType, Kinds: []string{"message"}},
		{Family: "msgvault.source_message_archived", SourceType: SourceType, Kinds: []string{"message"}},
		{Family: "msgvault.inbox_message_archived", SourceType: SourceType, Kinds: []string{"message"}},
	}})
	require.NoError(err)
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "synthetic-token")
	require.NoError(err)
	client.DeviceID = id.DeviceID("synthetic-scope-device")
	return f, NewImporter(st, &Runtime{Client: client}), ImportOptions{UserID: "@archive:example.org"}, source.ID
}

// The native mautrix parser must distinguish m.thread from a rich reply. Quiet
// incremental rooms stay available and a thread need not have an archived root.
func TestNativeMatrixInboxRoomAndStandardThreadMembership(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f, imp, opts, sourceID := newNativeMatrixInboxFeed(t)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	resources := func() map[string]store.MCPResource {
		page, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: sourceID})
		Require.NoError(t, err)
		result := map[string]store.MCPResource{}
		for _, row := range page.Resources {
			result[row.ProviderID] = row
		}
		return result
	}
	first := resources()
	require.Contains(first, "!room:example.org")
	require.Contains(first, "!quiet:example.org")
	require.Contains(first, "$root")
	assert.Equal("matrix_room", first["!room:example.org"].ScopeKind)
	assert.Equal("matrix_thread", first["$root"].ScopeKind)
	assert.Equal("!room:example.org", first["$root"].ParentProviderID)
	var history int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&history))
	assert.Zero(history)
	f.mu.Lock()
	f.round = 1
	f.quietOmitted = true
	f.mu.Unlock()
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(first["!quiet:example.org"].Reference, resources()["!quiet:example.org"].Reference, "incremental omission cannot retire a quiet joined room")
	countRoot := func() int {
		var count int
		Require.NoError(t, imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.inbox_message_archived' AND scope_id=?`), first["$root"].ID).Scan(&count))
		return count
	}
	assert.Equal(1, countRoot(), "a native standard thread reply captures its exact root")
	f.mu.Lock()
	f.round = 2
	f.ordinary = true
	f.mu.Unlock()
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, countRoot(), "ordinary rich-reply fallback is not membership in the target thread")
	f.mu.Lock()
	f.round = 3
	f.ordinary = false
	f.root = "$not-archived"
	f.mu.Unlock()
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.Contains(resources(), "$not-archived", "the native relation supplies the root identity without archive fallback")
	var missingRootMessages int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='$not-archived'`).Scan(&missingRootMessages))
	assert.Zero(missingRootMessages)
}

func TestNativeMatrixCurrentRoomAuthorityStopsInvalidRead(t *testing.T) {
	for _, mode := range []string{"members_denied", "missing_self", "transient_members", "wrong_room"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f, imp, opts, sourceID := newNativeMatrixInboxFeed(t)
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			source, err := imp.store.GetMCPResource(t.Context(), "source", sourceID)
			require.NoError(err)
			before, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_room", "!room:example.org")
			require.NoError(err)
			require.True(found)
			f.mu.Lock()
			f.round = 1
			switch mode {
			case "members_denied":
				f.memberStatus = http.StatusForbidden
			case "missing_self":
				f.missingSelf = true
			case "transient_members":
				f.memberStatus = http.StatusBadRequest
			case "wrong_room":
				f.eventRoom = "!other:example.org"
			}
			f.mu.Unlock()
			_, err = imp.Import(t.Context(), opts)
			require.Error(err, "current native room evidence must stop this observation")
			current, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_room", "!room:example.org")
			require.NoError(err)
			require.True(found)
			if mode == "members_denied" || mode == "missing_self" {
				assert.False(current.Present, "positive native non-membership retires the room")
				allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), sourceID)
				require.NoError(err)
				assert.True(allowed, "a room denial does not revoke the independently verified account")
				quiet, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_room", "!quiet:example.org")
				require.NoError(err)
				require.True(found)
				assert.True(quiet.Present)
			} else {
				assert.Equal(before.Identity.Generation, current.Identity.Generation, "unknown or inconsistent data is not room removal")
				allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), sourceID)
				require.NoError(err)
				assert.False(allowed, "unknown current permission pauses the previous source grant")
			}
			var events, messages int
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Zero(events)
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='$reply-1'`).Scan(&messages))
			assert.Zero(messages)
		})
	}
}
func TestNativeMatrixRejectsChangedSelectedIdentityBeforeSync(t *testing.T) {
	for _, device := range []bool{false, true} {
		t.Run(strconv.FormatBool(device), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f, imp, opts, sourceID := newNativeMatrixInboxFeed(t)
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			before, err := imp.store.GetMCPResource(t.Context(), "source", sourceID)
			require.NoError(err)
			f.mu.Lock()
			if device {
				f.device = "synthetic-other-device"
			} else {
				f.user = "@other:example.org"
			}
			requests := f.syncRequests
			f.mu.Unlock()
			_, err = imp.Import(t.Context(), opts)
			require.Error(err)
			f.mu.Lock()
			currentRequests := f.syncRequests
			f.mu.Unlock()
			assert.Equal(requests, currentRequests)
			allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), sourceID)
			require.NoError(err)
			assert.False(allowed)
			after, err := imp.store.GetMCPResource(t.Context(), "source", sourceID)
			require.NoError(err)
			assert.NotEqual(before.Reference, after.Reference)
		})
	}
}
func TestNativeMatrixUnselectedEventsDoNotRetainOldGrant(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f, imp, opts, sourceID := newNativeMatrixInboxFeed(t)
			source, err := imp.store.GetMCPResource(t.Context(), "source", sourceID)
			require.NoError(err)
			require.NoError(imp.store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: sourceID, SourceReference: source.Reference, SourceType: SourceType, NativeIdentity: opts.UserID, Known: true, MessageRead: true}))
			_, err = imp.store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: enabled, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "slack", Kinds: []string{"message"}}}})
			require.NoError(err)
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			f.mu.Lock()
			requests := f.whoamiRequests
			f.mu.Unlock()
			assert.Zero(requests, "ordinary archival does not add Events-only identity calls")
			allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), sourceID)
			require.NoError(err)
			assert.False(allowed, "disabled/unselected capture cannot retain an earlier Events grant")
		})
	}
}

// Native timeline and membership responses cannot borrow a room lifetime
// replaced while the request is in flight. The real HTTP handler is blocked;
// revocation/restoration use the same Store APIs as native observations.
func TestNativeMatrixReadCannotBorrowReplacementLifetime(t *testing.T) {
	for _, stage := range []string{"sync", "members"} {
		t.Run(stage, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f, imp, opts, sourceID := newNativeMatrixInboxFeed(t)
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			source, err := imp.store.GetMCPResource(t.Context(), "source", sourceID)
			require.NoError(err)
			before, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_room", "!room:example.org")
			require.NoError(err)
			require.True(found)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			f.mu.Lock()
			f.round = 1
			if stage == "sync" {
				f.syncEntered, f.syncRelease = entered, release
			} else {
				f.memberEntered, f.memberRelease = entered, release
			}
			f.mu.Unlock()
			done := make(chan error, 1)
			go func() { _, importErr := imp.Import(t.Context(), opts); done <- importErr }()
			select {
			case <-entered:
			case <-time.After(30 * time.Second):
				require.FailNow("native request did not reach observed barrier")
			}
			observation := store.NativeMessageScopeInventory{SourceID: sourceID, SourceReference: source.Reference, SourceType: SourceType, NativeIdentity: opts.UserID, Kind: "matrix_room", RetiredProviderIDs: []string{"!room:example.org"}}
			require.NoError(imp.store.ObserveNativeMessageScopes(t.Context(), observation))
			observation.RetiredProviderIDs = nil
			observation.Scopes = []store.NativeMessageScope{{ProviderID: "!room:example.org", DisplayName: "Restored synthetic room"}}
			require.NoError(imp.store.ObserveNativeMessageScopes(t.Context(), observation))
			replacement, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_room", "!room:example.org")
			require.NoError(err)
			require.True(found)
			require.NotEqual(before.Identity.Generation, replacement.Identity.Generation)
			unblock()
			select {
			case importErr := <-done:
				require.Error(importErr, "an old native response cannot capture into a replacement lifetime")
			case <-time.After(30 * time.Second):
				require.FailNow("native importer did not settle after response release")
			}
			after, found, err := imp.store.LookupNativeMessageScope(t.Context(), sourceID, source.Reference, "matrix_room", "!room:example.org")
			require.NoError(err)
			require.True(found)
			assert.Equal(replacement.Identity.Generation, after.Identity.Generation)
			assert.True(after.Present, "a stale request must not retire the readable replacement")
			var messages, events int
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='$reply-1'`).Scan(&messages))
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Zero(messages)
			assert.Zero(events)
		})
	}
}
