package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type nativeTeamsEventFeed struct {
	mu       sync.Mutex
	messages []map[string]any
}

func (f *nativeTeamsEventFeed) set(messages ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = messages
}

func nativeTeamsEventMessage(id, when, body, sender string) map[string]any {
	return map[string]any{
		"id": id, "createdDateTime": when, "lastModifiedDateTime": when,
		"from": map[string]any{"user": map[string]any{"id": sender, "displayName": "Synthetic Participant", "userIdentityType": "emailUser"}},
		"body": map[string]any{"contentType": "text", "content": body},
	}
}

func newNativeTeamsEventFeed(t *testing.T) (*nativeTeamsEventFeed, *Importer, ImportOptions) {
	t.Helper()
	f := &nativeTeamsEventFeed{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if selfChatAbsent(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"event-chat","chatType":"group","topic":"Synthetic event chat"}]}`))
		case "/chats/event-chat/members":
			_, _ = w.Write([]byte(`{"value":[{"id":"owner","userId":"owner","email":"owner@example.test","displayName":"Synthetic Owner"}]}`))
		case "/me/chats/event-chat/messages":
			f.mu.Lock()
			defer f.mu.Unlock()
			messages := make([]map[string]any, 0, len(f.messages))
			filter := r.URL.Query().Get("$filter")
			var since time.Time
			if filter != "" {
				var err error
				since, err = time.Parse(time.RFC3339Nano, strings.TrimPrefix(filter, "lastModifiedDateTime gt "))
				if err != nil {
					http.Error(w, "invalid synthetic filter", http.StatusBadRequest)
					return
				}
			}
			for _, m := range f.messages {
				modifiedText, ok := m["lastModifiedDateTime"].(string)
				if !ok {
					http.Error(w, "invalid synthetic timestamp", http.StatusInternalServerError)
					return
				}
				modified, err := time.Parse(time.RFC3339Nano, modifiedText)
				if err != nil {
					http.Error(w, "invalid synthetic timestamp", http.StatusInternalServerError)
					return
				}
				if since.IsZero() || modified.After(since) {
					messages = append(messages, m)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": messages})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "teams", Kinds: []string{"message"}}}})
	require.NoError(t, err)
	return f, NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 50)), ImportOptions{Email: "owner@example.test"}
}

func TestMCPTeamsChatLiveProvenance(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, imp, opts := newNativeTeamsEventFeed(t)
	old := nativeTeamsEventMessage("old", "2026-01-01T00:00:00Z", "Synthetic history", "sender@example.test")
	f.set(old)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Zero(count)
	// A new native identity at the existing timestamp is recovered by overlap.
	tie := nativeTeamsEventMessage("tie", "2026-01-01T00:00:00Z", "Synthetic live overlap", "sender@example.test")
	f.set(old, tie)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "overlap upserts do not duplicate arrival")
}

func TestMCPTeamsEmptyChatCoverage(t *testing.T) {
	require := require.New(t)
	f, imp, opts := newNativeTeamsEventFeed(t)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	f.set(nativeTeamsEventMessage("first", "2026-01-01T00:00:00Z", "Synthetic first arrival", "sender@example.test"))
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(t, 1, count, "completed empty history establishes live coverage without a clock cutoff")
}

func TestMCPTeamsFullRepairResumeHistorical(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, imp, opts := newNativeTeamsEventFeed(t)
	known := nativeTeamsEventMessage("known", "2026-01-03T00:00:00Z", "Synthetic known message", "sender@example.test")
	f.set(known)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	historical := nativeTeamsEventMessage("historic", "2026-01-01T00:00:00Z", "Synthetic recovered history", "sender@example.test")
	f.set(known, historical)
	release := failNativeTeamsInsert(t, imp.store, "message_raw")
	full := opts
	full.Full = true
	_, err = imp.Import(t.Context(), full)
	require.Error(err)
	release()
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='chat:event-chat:historic'`).Scan(&count))
	assert.Equal(1, count, "an older successful cursor must not hide unfinished full-repair history")
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Zero(count, "resumed repair stays historical")
	f.set(known, historical, nativeTeamsEventMessage("live", "2026-01-04T00:00:00Z", "Synthetic live arrival", "sender@example.test"))
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count)
}

func TestMCPTeamsLimitedHistoryRemainsMuted(t *testing.T) {
	for _, fullRepair := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", fullRepair), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f, imp, opts := newNativeTeamsEventFeed(t)
			known := nativeTeamsEventMessage("known", "2026-01-03T00:00:00Z", "Synthetic known", "sender@example.test")
			historic := nativeTeamsEventMessage("historic", "2026-01-01T00:00:00Z", "Synthetic history", "sender@example.test")
			if fullRepair {
				f.set(known)
				_, err := imp.Import(t.Context(), opts)
				require.NoError(err)
			}
			f.set(known, historic)
			limited := opts
			limited.Full = fullRepair
			limited.Limit = 1
			_, err := imp.Import(t.Context(), limited)
			require.NoError(err)
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			var messages, events int
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Equal(2, messages)
			assert.Zero(events, "limited history cannot establish live coverage")
			f.set(known, historic, nativeTeamsEventMessage("live", "2026-01-04T00:00:00Z", "Synthetic arrival", "sender@example.test"))
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Equal(1, events)
		})
	}
}

func TestMCPTeamsEditReactionAndOwnAttribution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, imp, opts := newNativeTeamsEventFeed(t)
	// An account label is not proof of ownership. Seed the confirmed synthetic
	// identity through the same Store API used by native identity management.
	source, err := imp.store.GetOrCreateSource(sourceTypeTeams, opts.Email)
	require.NoError(err)
	require.NoError(imp.store.AddAccountIdentity(source.ID, opts.Email, "manual"))
	old := nativeTeamsEventMessage("old", "2026-01-01T00:00:00Z", "Synthetic history", "sender@example.test")
	f.set(old)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	edited := nativeTeamsEventMessage("old", "2026-01-02T00:00:00Z", "Synthetic edited history", "sender@example.test")
	edited["reactions"] = []map[string]any{{"reactionType": "like", "createdDateTime": "2026-01-02T00:00:00Z", "user": map[string]any{"user": map[string]any{"id": "owner@example.test", "displayName": "Synthetic Owner", "userIdentityType": "emailUser"}}}}
	f.set(edited, nativeTeamsEventMessage("own", "2026-01-02T00:00:00Z", "Synthetic own message", "owner@example.test"))
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "edits and reaction snapshots are not new arrivals")
	var fromMe bool
	require.NoError(imp.store.DB().QueryRow(`SELECT from_me FROM mcp_event_log`).Scan(&fromMe))
	assert.True(fromMe, "event records final source-owner attribution")
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&count))
	assert.Equal(1, count, "muted reaction updates still reach the archive")
}

func TestSyncStateChatCoverage(t *testing.T) {
	for _, tc := range []struct {
		name, blob string
		want       bool
	}{
		{"new", "", false},
		{"legacy valid", `{"chats":{"chat":"2026-01-01T00:00:00.123Z"}}`, true},
		{"legacy malformed", `{"chats":{"chat":"invalid"}}`, false},
		{"empty completed", `{"covered_chats":{"chat":true}}`, true},
		{"empty incomplete", `{"covered_chats":{"chat":false}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			state, err := LoadSyncState(tc.blob)
			require.NoError(err)
			assert.Equal(tc.want, state.ChatCovered("chat"))
			blob, err := state.Marshal()
			require.NoError(err)
			restored, err := LoadSyncState(blob)
			require.NoError(err)
			assert.Equal(tc.want, restored.ChatCovered("chat"))
		})
	}
}
