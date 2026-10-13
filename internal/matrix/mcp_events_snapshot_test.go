package matrix

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

// A read-only homeserver fixture exercises actual native sync/Store behavior.
type eventsMatrixFixture struct {
	store         *store.Store
	runtime       *Runtime
	round         atomic.Int64
	emptyTitle    bool
	replyID       string
	gap           bool
	failGap       bool
	failSync      bool
	emptyBaseline bool
	gapMessage    bool
	encrypted     bool
	extraMessages int
	secondRoom    bool
	failSecond    bool
	leave         bool
	quiet         bool
	sourceID      int64
}

func newEventsMatrixFixture(t *testing.T) *eventsMatrixFixture {
	t.Helper()
	require := require.New(t)
	f := &eventsMatrixFixture{store: testutil.NewTestStore(t)}
	source, err := f.store.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	f.sourceID = source.ID
	_, err = f.store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
	require.NoError(err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		live := f.round.Load() > 0
		var response any
		switch r.URL.Path {
		case "/_matrix/client/v3/sync":
			if f.failSync {
				http.Error(w, "synthetic sync interruption", http.StatusBadRequest)
				return
			}
			title, name, body := "Original room", "$root", "Synthetic history"
			content := map[string]any{"msgtype": "m.text", "body": body}
			if live {
				reply := "$root"
				if f.replyID != "" {
					reply = f.replyID
				}
				title, name = "Changed room", "$child"
				if f.round.Load() > 1 {
					name = fmt.Sprintf("$child-%d", f.round.Load())
				}
				if f.emptyTitle {
					title = ""
				}
				content = map[string]any{"msgtype": "m.text", "body": "Synthetic live reply", "m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": reply}}}
			}
			events := []any{map[string]any{"type": "m.room.message", "event_id": name, "sender": "@member:example.org", "origin_server_ts": int64(1700000000000), "content": content}}

			for i := range f.extraMessages {
				events = append(events, map[string]any{"type": "m.room.message", "event_id": fmt.Sprintf("$extra-%d-%d", f.round.Load(), i), "sender": "@member:example.org", "origin_server_ts": int64(1700000000001 + i), "content": map[string]any{"msgtype": "m.text", "body": "Synthetic batch message"}})
			}
			if !live && f.emptyBaseline {
				events = []any{}
			}
			if live && f.encrypted {
				events = append(events, map[string]any{"type": "m.room.encrypted", "event_id": "$encrypted", "sender": "@member:example.org", "content": map[string]any{"algorithm": "m.megolm.v1.aes-sha2", "ciphertext": "synthetic", "session_id": "synthetic-session", "sender_key": "synthetic-key"}})
			}
			joined := map[string]any{"!room:example.org": map[string]any{
				"state":    map[string]any{"events": []any{map[string]any{"type": "m.room.name", "state_key": "", "content": map[string]any{"name": title}}}},
				"timeline": map[string]any{"limited": live && f.gap, "prev_batch": map[bool]string{true: "gap", false: ""}[live && f.gap], "events": events},
			}}
			rooms := map[string]any{"join": joined}
			response = map[string]any{"next_batch": fmt.Sprintf("sync-%d", f.round.Load()), "rooms": rooms}
			if f.secondRoom {
				joined["!second:example.org"] = map[string]any{"timeline": map[string]any{"events": []any{}}}
			}
			if f.leave || f.quiet {
				delete(joined, "!room:example.org")
			}
			if f.leave {
				rooms["leave"] = map[string]any{"!room:example.org": map[string]any{"timeline": map[string]any{"events": []any{}}}}
			}
		case "/_matrix/client/v3/rooms/!second:example.org/joined_members":
			if f.failSecond {
				http.Error(w, "synthetic second-room interruption", http.StatusBadRequest)
				return
			}
			response = map[string]any{"joined": map[string]any{}}
		case "/_matrix/client/v3/rooms/!room:example.org/messages":
			if f.failGap {
				http.Error(w, "synthetic interruption", http.StatusBadRequest)
				return
			}
			chunk := []any{}
			if f.gapMessage {
				chunk = append(chunk, map[string]any{"type": "m.room.message", "event_id": "$gap-child", "sender": "@member:example.org", "origin_server_ts": int64(1700000000000), "content": map[string]any{"msgtype": "m.text", "body": "Synthetic gap message"}})
			}
			response = map[string]any{"chunk": chunk, "end": ""}
		case "/_matrix/client/v3/user/@archive:example.org/account_data/m.direct":
			rooms := []string{}
			if live {
				rooms = append(rooms, "!room:example.org")
			}
			response = map[string]any{"@member:example.org": rooms}
		case "/_matrix/client/v3/rooms/!room:example.org/joined_members":
			members := map[string]any{"@archive:example.org": map[string]any{"display_name": "Synthetic Owner"}, "@member:example.org": map[string]any{"display_name": "Synthetic Sender"}}
			if live {
				members["@new:example.org"] = map[string]any{"display_name": "Synthetic New Member"}
			}
			response = map[string]any{"joined": members}
		default:
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.MarshalWrite(w, response))
	}))
	t.Cleanup(srv.Close)
	client, err := mautrix.NewClient(srv.URL, id.UserID("@archive:example.org"), "synthetic-token")
	require.NoError(err)
	f.runtime = &Runtime{Client: client}
	return f
}

func (f *eventsMatrixFixture) importRound(t *testing.T, mode store.IngestMode) error {
	t.Helper()
	_, err := NewImporter(f.store.WithIngestContext(store.IngestContext{Mode: mode}), f.runtime).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	return err
}

func failMatrixSnapshotWrite(t *testing.T, st *store.Store, table, event string) func() {
	t.Helper()
	require := require.New(t)
	name := "fail_matrix_snapshot"
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_matrix_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic snapshot failure'; END; $$`)
		require.NoError(err)
		_, err = st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, name, event, table, name))
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT,'synthetic snapshot failure'); END`, name, event, table))
		require.NoError(err)
	}
	release := func() {
		if st.IsPostgreSQL() {
			_, err := st.DB().Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table))
			require.NoError(err)
			_, err = st.DB().Exec(`DROP FUNCTION IF EXISTS fail_matrix_snapshot()`)
			require.NoError(err)
		} else {
			_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS fail_matrix_snapshot`)
			require.NoError(err)
		}
	}
	t.Cleanup(release)
	return release
}

func TestMCPMatrixRequiredSnapshotRollback(t *testing.T) {
	for _, tc := range []struct {
		name, table, event string
		empty              bool
	}{
		{"body with new title", "message_bodies", "INSERT", false},
		{"raw with cleared title", "message_raw", "INSERT", true},
		{"reply link", "messages", "UPDATE OF reply_to_message_id", false},
		{"member count", "conversations", "UPDATE OF metadata", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newEventsMatrixFixture(t)
			require.NoError(f.importRound(t, store.IngestBackfill))
			f.emptyTitle = tc.empty
			f.round.Store(1)
			release := failMatrixSnapshotWrite(t, f.store, tc.table, tc.event)
			require.Error(f.importRound(t, store.IngestLive))
			var children, events, members, count int
			var title, kind, metadata string
			require.NoError(f.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='$child'`).Scan(&children))
			require.NoError(f.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			require.NoError(f.store.DB().QueryRow(`SELECT title,conversation_type,metadata FROM conversations WHERE source_conversation_id='!room:example.org'`).Scan(&title, &kind, &metadata))
			require.NoError(f.store.DB().QueryRow(`SELECT COUNT(*) FROM conversation_participants`).Scan(&members))
			assert.Zero(children)
			assert.Zero(events)
			assert.Equal("Original room", title)
			assert.Equal("group_chat", kind)
			var roomMetadata map[string]int
			require.NoError(json.Unmarshal([]byte(metadata), &roomMetadata))
			count = roomMetadata["member_count"]
			assert.Equal(2, count)
			assert.Equal(2, members)
			release()
			require.NoError(f.importRound(t, store.IngestLive))
			require.NoError(f.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Equal(1, events)
			require.NoError(f.store.DB().QueryRow(`SELECT title,conversation_type,metadata FROM conversations WHERE source_conversation_id='!room:example.org'`).Scan(&title, &kind, &metadata))
			wantTitle := "Changed room"
			if tc.empty {
				wantTitle = ""
			}
			assert.Equal(wantTitle, title)
			assert.Equal("direct_chat", kind)
			require.NoError(json.Unmarshal([]byte(metadata), &roomMetadata))
			count = roomMetadata["member_count"]
			assert.Equal(3, count)
			var childParent, root int64
			require.NoError(f.store.DB().QueryRow(`SELECT reply_to_message_id FROM messages WHERE source_message_id='$child'`).Scan(&childParent))
			require.NoError(f.store.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id='$root'`).Scan(&root))
			assert.Equal(root, childParent)
		})
	}
}

func TestMCPMatrixOptionalReplyTargets(t *testing.T) {
	for _, kind := range []string{"missing", "deleted", "foreign room"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newEventsMatrixFixture(t)
			require.NoError(f.importRound(t, store.IngestBackfill))
			switch kind {
			case "missing":
				f.replyID = "$absent"
			case "deleted":
				require.NoError(f.store.MarkMessageDeleted(f.sourceID, "$root"))
			case "foreign room":
				other, err := f.store.EnsureConversation(f.sourceID, "!other:example.org", "Other room")
				require.NoError(err)
				_, err = f.store.UpsertMessage(&store.Message{SourceID: f.sourceID, ConversationID: other, SourceMessageID: "$foreign", MessageType: SourceType})
				require.NoError(err)
				f.replyID = "$foreign"
			}
			f.round.Store(1)
			require.NoError(f.importRound(t, store.IngestLive))
			var noParent bool
			var count int
			require.NoError(f.store.DB().QueryRow(`SELECT reply_to_message_id IS NULL FROM messages WHERE source_message_id='$child'`).Scan(&noParent))
			require.NoError(f.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.True(noParent)
			assert.Equal(1, count)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$child"})
			require.NoError(err)
			body, err := f.store.GetMessageBodyText(ids["$child"])
			require.NoError(err)
			assert.Equal("Synthetic live reply", body)
			raw, err := f.store.GetMessageRaw(ids["$child"])
			require.NoError(err)
			assert.Contains(string(raw), "m.in_reply_to")
		})
	}
}

func TestMCPMatrixRetainedReplyRepairIsSilent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEventsMatrixFixture(t)
	require.NoError(f.importRound(t, store.IngestBackfill))
	f.replyID = "$later"
	f.gap, f.failGap = true, true
	f.round.Store(1)
	require.Error(f.importRound(t, store.IngestLive))
	checkpoint, err := f.store.GetLatestCheckpointedSyncByType(f.sourceID, SourceType)
	require.NoError(err)
	assert.Contains(checkpoint.CursorBefore.String, "$child")
	var convID int64
	require.NoError(f.store.DB().QueryRow(`SELECT conversation_id FROM messages WHERE source_message_id='$child'`).Scan(&convID))
	parent, err := f.store.UpsertMessage(&store.Message{SourceID: f.sourceID, ConversationID: convID, SourceMessageID: "$later", MessageType: SourceType})
	require.NoError(err)
	f.failGap = false
	require.NoError(f.importRound(t, store.IngestLive))
	var linked int64
	var count int
	require.NoError(f.store.DB().QueryRow(`SELECT reply_to_message_id FROM messages WHERE source_message_id='$child'`).Scan(&linked))
	require.NoError(f.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(parent, linked)
	assert.Equal(1, count, "later context repair cannot produce a second arrival")
}

func TestMCPMatrixNewRoomTypeSurvivesSnapshotFailure(t *testing.T) {
	require := require.New(t)
	f := newEventsMatrixFixture(t)
	failMatrixSnapshotWrite(t, f.store, "message_bodies", "INSERT")
	require.Error(f.importRound(t, store.IngestBackfill))
	var kind string
	require.NoError(f.store.DB().QueryRow(`SELECT conversation_type FROM conversations WHERE source_conversation_id='!room:example.org'`).Scan(&kind))
	assert.Equal(t, "group_chat", kind, "a reserved Matrix room must never be classified as email")
}

func TestMCPMatrixRoomProjectionWorkIsBoundedPerRound(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "history", true: "live"}[live], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newEventsMatrixFixture(t)
			if live {
				require.NoError(f.importRound(t, store.IngestBackfill))
				f.round.Store(1)
			}
			require.NoError(f.store.EnableEmbeddingChangeJournal(t.Context()))
			_, err := f.store.DB().Exec(`CREATE TABLE synthetic_projection_writes (writes INTEGER NOT NULL)`)
			require.NoError(err)
			_, err = f.store.DB().Exec(`INSERT INTO synthetic_projection_writes VALUES (0)`)
			require.NoError(err)
			if f.store.IsPostgreSQL() {
				_, err = f.store.DB().Exec(`CREATE FUNCTION count_matrix_projection() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE synthetic_projection_writes SET writes=writes+1; RETURN NEW; END; $$`)
				require.NoError(err)
				_, err = f.store.DB().Exec(`CREATE TRIGGER count_matrix_projection AFTER UPDATE OF metadata ON conversations FOR EACH ROW EXECUTE FUNCTION count_matrix_projection()`)
				require.NoError(err)
			} else {
				_, err = f.store.DB().Exec(`CREATE TRIGGER count_matrix_projection AFTER UPDATE OF metadata ON conversations BEGIN UPDATE synthetic_projection_writes SET writes=writes+1; END`)
				require.NoError(err)
			}
			f.extraMessages = 8
			require.NoError(f.importRound(t, store.IngestLive))
			var writes, messages int
			require.NoError(f.store.DB().QueryRow(`SELECT writes FROM synthetic_projection_writes`).Scan(&writes))
			require.NoError(f.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			assert.GreaterOrEqual(messages, 9)
			assert.LessOrEqual(writes, 2, "room projection work must not grow with message count")
		})
	}
}
