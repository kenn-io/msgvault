package teams

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The fixture replaces only Graph's external HTTP boundary. Parsing, identity
// resolution, native sync and SQL persistence use their production paths.
func nativeTeamsChatFixture(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if selfChatAbsent(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"synthetic-chat","chatType":"group","topic":"Synthetic chat"}]}`))
		case "/chats/synthetic-chat/members":
			_, _ = w.Write([]byte(`{"value":[{"@odata.type":"#microsoft.graph.aadUserConversationMember","id":"member-owner","userId":"owner-id","email":"owner@example.test","displayName":"Synthetic Owner"}]}`))
		case "/me/chats/synthetic-chat/messages":
			_, _ = w.Write([]byte(`{"value":[{"id":"message-one","createdDateTime":"2026-01-01T00:00:00Z","lastModifiedDateTime":"2026-01-01T00:00:00Z","from":{"user":{"id":"sender@example.test","displayName":"Synthetic Sender","userIdentityType":"emailUser"}},"body":{"contentType":"text","content":"Synthetic readable message"},"attachments":[{"id":"link-one","name":"Synthetic document","contentType":"reference","contentUrl":"https://example.test/document"}],"mentions":[{"id":0,"mentionText":"Synthetic Owner","mentioned":{"user":{"id":"owner@example.test","displayName":"Synthetic Owner","userIdentityType":"emailUser"}}}],"reactions":[{"reactionType":"like","createdDateTime":"2026-01-01T00:00:01Z","user":{"user":{"id":"owner@example.test","displayName":"Synthetic Owner","userIdentityType":"emailUser"}}}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMCPTeamsChatSnapshotRollback(t *testing.T) {
	for _, table := range []string{"message_bodies", "message_raw", "message_recipients", "conversation_participants", "reactions", "attachments"} {
		t.Run(table, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			srv := nativeTeamsChatFixture(t)
			imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 50))
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "teams", Kinds: []string{"message"}}}})
			require.NoError(err)
			release := failNativeTeamsInsert(t, st, table)
			_, err = imp.Import(t.Context(), ImportOptions{Email: "owner@example.test"})
			require.Error(err, "mandatory snapshot failure must stop native capture")
			var count int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			assert.Zero(count, "a failed snapshot must not leave a partial message")
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Zero(count)
			release()
			_, err = imp.Import(t.Context(), ImportOptions{Email: "owner@example.test"})
			require.NoError(err)
			var id int64
			require.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&id))
			var body, format string
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), id).Scan(&body))
			assert.Equal("Synthetic readable message", body)
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT raw_format FROM message_raw WHERE message_id=?`), id).Scan(&format))
			assert.Equal("teams_json", format)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&count))
			assert.Equal(1, count)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&count))
			assert.Equal(1, count)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE recipient_type='mention'`).Scan(&count))
			assert.Equal(1, count)
		})
	}
}

// Closed table names above target real writes on both supported databases.
func failNativeTeamsInsert(t *testing.T, st *store.Store, table string) func() {
	t.Helper()
	require := require.New(t)
	name := "fail_native_teams_" + table
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic snapshot failure'; END; $$`, name))
		require.NoError(err)
		_, err = st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, name, table, name))
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT, 'synthetic snapshot failure'); END`, name, table))
		require.NoError(err)
	}
	release := func() {
		if st.IsPostgreSQL() {
			_, err := st.DB().Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table))
			require.NoError(err)
			_, err = st.DB().Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
			require.NoError(err)
		} else {
			_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS ` + name)
			require.NoError(err)
		}
	}
	t.Cleanup(release)
	return release
}
