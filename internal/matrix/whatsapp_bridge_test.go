package matrix

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

func TestSplitBridgeMessageID(t *testing.T) {
	sender, waID, ok := splitBridgeMessageID("123@g.us:456@lid:3EB0ABC", "123@g.us")
	assert.True(t, ok)
	assert.Equal(t, "456@lid", sender)
	assert.Equal(t, "3EB0ABC", waID)

	_, _, ok = splitBridgeMessageID("other@g.us:456@lid:3EB0ABC", "123@g.us")
	assert.False(t, ok, "a key from another chat is rejected")
	_, _, ok = splitBridgeMessageID("123@g.us:456@lid:", "123@g.us")
	assert.False(t, ok, "an empty message ID is rejected")
}

func TestBridgeJIDPhoneResolution(t *testing.T) {
	snap := &bridgeSnapshot{lidPhones: map[string]string{"777": "+33612345678"}}
	assert.Equal(t, "+33612345678", snap.jidPhone("777@lid"))
	assert.Equal(t, "+41791234567", snap.jidPhone("41791234567:12@s.whatsapp.net"), "device part is dropped")
	assert.Equal(t, "+41791234567", snap.jidPhone("41791234567"), "bare user part is a phone")
	assert.Empty(t, snap.jidPhone("888@lid"), "unknown LID has no phone")
	assert.Empty(t, snap.jidPhone("status@broadcast"))

	assert.Equal(t, "33612345678@s.whatsapp.net", snap.canonicalChat("777@lid"),
		"a resolvable LID chat uses the phone key of the Desktop importer")
	assert.Equal(t, "888@lid", snap.canonicalChat("888@LID"))
	assert.Equal(t, "120363@g.us", snap.canonicalChat("120363@g.us"))
	assert.False(t, importableWhatsAppChat("status@broadcast"))
	assert.False(t, importableWhatsAppChat("123@newsletter"))
}

const bridgeTestSchema = `
CREATE TABLE user_login (bridge_id TEXT, user_mxid TEXT, id TEXT);
CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT);
CREATE TABLE whatsmeow_contacts (our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT, push_name TEXT, business_name TEXT);
CREATE TABLE user_portal (bridge_id TEXT, user_mxid TEXT, login_id TEXT, portal_id TEXT, portal_receiver TEXT);
CREATE TABLE portal (bridge_id TEXT, id TEXT, receiver TEXT, mxid TEXT, name TEXT);
CREATE TABLE message (rowid INTEGER PRIMARY KEY, bridge_id TEXT, id TEXT, part_id TEXT, mxid TEXT,
	room_id TEXT, room_receiver TEXT, timestamp BIGINT, edit_count INTEGER);
`

type bridgeFixture struct {
	db   *sql.DB
	path string
}

func newBridgeFixture(t *testing.T) *bridgeFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mautrix-whatsapp.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(bridgeTestSchema)
	require.NoError(t, err)
	return &bridgeFixture{db: db, path: path}
}

func (f *bridgeFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	_, err := f.db.Exec(query, args...)
	require.NoError(t, err)
}

func (f *bridgeFixture) message(t *testing.T, chat, receiver, sender, waID, mxid string, when time.Time, edits int) {
	t.Helper()
	f.exec(t, `INSERT INTO message (bridge_id, id, part_id, mxid, room_id, room_receiver, timestamp, edit_count)
		VALUES ('whatsapp', ?, '', ?, ?, ?, ?, ?)`, chat+":"+sender+":"+waID, mxid, chat, receiver, when.UnixNano(), edits)
}

func TestImportWhatsAppBridgeWritesNativeWhatsAppMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const me = "@me:example.org"
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	f := newBridgeFixture(t)
	f.exec(t, `INSERT INTO user_login VALUES ('whatsapp', ?, '33600000001')`, me)
	f.exec(t, `INSERT INTO user_login VALUES ('whatsapp', '@other:example.org', '41700000009')`)
	f.exec(t, `INSERT INTO whatsmeow_lid_map VALUES ('111', '33600000001'), ('222', '33611111111'), ('333', '33622222222')`)
	f.exec(t, `INSERT INTO whatsmeow_contacts VALUES ('33600000001:4@s.whatsapp.net', '33611111111@s.whatsapp.net', 'Alice', 'Alice Martin', 'ali', '')`)
	f.exec(t, `INSERT INTO portal VALUES ('whatsapp', '222@lid', '33600000001', '!dm:example.org', 'Alice (WA)')`)
	f.exec(t, `INSERT INTO portal VALUES ('whatsapp', '120363@g.us', '', '!group:example.org', 'Bike ride')`)
	f.exec(t, `INSERT INTO portal VALUES ('whatsapp', 'status@broadcast', '33600000001', '!status:example.org', 'Status')`)
	f.exec(t, `INSERT INTO user_portal VALUES ('whatsapp', ?, '33600000001', '120363@g.us', '')`, me)
	f.message(t, "222@lid", "33600000001", "222@lid", "WA-IN", "$in", now.Add(-3*time.Hour), 0)
	f.message(t, "222@lid", "33600000001", "111@lid", "WA-OUT", "$out", now.Add(-2*time.Hour), 1)
	f.message(t, "222@lid", "33600000001", "222@lid", "WA-DESKTOP", "$desktop", now.Add(-90*time.Minute), 0)
	f.message(t, "120363@g.us", "", "333@lid", "WA-GROUP", "$group", now.Add(-time.Hour), 0)
	f.message(t, "status@broadcast", "33600000001", "222@lid", "WA-STATUS", "$status", now.Add(-time.Hour), 0)
	f.message(t, "120363@g.us", "", "333@lid", "WA-OLD", "$old", now.Add(-30*24*time.Hour), 0)
	f.message(t, "120363@g.us", "", "333@lid", "WA-FAKE", "~fake:abc", now.Add(-time.Hour), 0)
	f.message(t, "120363@g.us", "", "333@lid", "WA-BAD", "$bad", now.Add(-time.Hour), 0)

	events := map[string]string{
		"$in":      `{"type":"m.room.message","event_id":"$in","room_id":"!dm:example.org","sender":"@whatsapp_lid-222:example.org","origin_server_ts":1,"content":{"msgtype":"m.text","body":"hello"}}`,
		"$out":     `{"type":"m.room.message","event_id":"$out","room_id":"!dm:example.org","sender":"@me:example.org","origin_server_ts":2,"content":{"msgtype":"m.text","body":"> <@whatsapp_lid-222:example.org> hello\n\nhi bak","m.relates_to":{"m.in_reply_to":{"event_id":"$in"}}}}`,
		"$desktop": `{"type":"m.room.message","event_id":"$desktop","room_id":"!dm:example.org","sender":"@whatsapp_lid-222:example.org","origin_server_ts":3,"content":{"msgtype":"m.text","body":"from desktop too"}}`,
		"$group":   `{"type":"m.room.message","event_id":"$group","room_id":"!group:example.org","sender":"@whatsapp_lid-333:example.org","origin_server_ts":4,"content":{"msgtype":"m.image","body":"pic.jpg","filename":"pic.jpg","url":"mxc://example.org/pic"}}`,
		"$old":     `{"type":"m.room.message","event_id":"$old","room_id":"!group:example.org","sender":"@whatsapp_lid-333:example.org","origin_server_ts":0,"content":{"msgtype":"m.text","body":"last month"}}`,
	}
	var fetches atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/rooms/{room}/event/{event}", func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if r.PathValue("event") == "$bad" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errcode":"M_BAD_JSON","error":"deserialization failed"}`))
			return
		}
		body, ok := events[r.PathValue("event")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"no event"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("GET /_matrix/client/v1/rooms/{room}/relations/{event}/m.replace", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("$out", r.PathValue("event"))
		_, _ = w.Write([]byte(`{"chunk":[
			{"type":"m.room.message","event_id":"$edit1","room_id":"!dm:example.org","sender":"@me:example.org","origin_server_ts":5,"content":{"msgtype":"m.text","body":"* hi back","m.new_content":{"msgtype":"m.text","body":"hi back"},"m.relates_to":{"rel_type":"m.replace","event_id":"$out"}}},
			{"type":"m.room.message","event_id":"$edit0","room_id":"!dm:example.org","sender":"@me:example.org","origin_server_ts":4,"content":{"msgtype":"m.text","body":"* hi bak!","m.new_content":{"msgtype":"m.text","body":"hi bak!"},"m.relates_to":{"rel_type":"m.replace","event_id":"$out"}}},
			{"type":"m.room.message","event_id":"$forged","room_id":"!dm:example.org","sender":"@whatsapp_lid-222:example.org","origin_server_ts":9,"content":{"msgtype":"m.text","body":"* forged","m.new_content":{"msgtype":"m.text","body":"forged"},"m.relates_to":{"rel_type":"m.replace","event_id":"$out"}}}
		]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID(me), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(whatsAppSourceType, "+33600000001")
	require.NoError(err)
	// WhatsApp Desktop already archived one of the bridged messages.
	desktopConv, err := st.EnsureConversationWithTypeContext(t.Context(), source.ID, "33611111111@s.whatsapp.net", "direct_chat", "")
	require.NoError(err)
	_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{
			ConversationID: desktopConv, SourceID: source.ID, SourceMessageID: "WA-DESKTOP",
			MessageType: "whatsapp", SentAt: sql.NullTime{Time: now.Add(-90 * time.Minute), Valid: true},
		},
		BodyText: sql.NullString{String: "from desktop too", Valid: true},
		RawMIME:  []byte(`{}`), RawFormat: "whatsapp_apple_json",
	})
	require.NoError(err)

	bridge, err := OpenWhatsAppBridge(t.Context(), f.path)
	require.NoError(err)
	defer func() { _ = bridge.Close() }()
	portals, err := bridge.PortalRooms(t.Context())
	require.NoError(err)
	assert.Equal([]string{"!dm:example.org", "!group:example.org", "!status:example.org"}, portals)

	opts := WhatsAppBridgeOptions{UserID: me, now: func() time.Time { return now }}
	sum, err := ImportWhatsAppBridge(t.Context(), st, &Runtime{Client: client}, bridge, opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.Logins, "only the logins of the syncing Matrix account are imported")
	assert.Equal(int64(4), sum.MessagesAdded, "first run is full, so last month's group message is included")
	assert.Equal(int64(1), sum.MessagesSkipped, "the Desktop copy is left alone")
	assert.Equal(int64(1), sum.ErrorsCount, "a refused event is counted and skipped, the fake one never fetched")

	count, err := st.CountMessagesForSource(source.ID)
	require.NoError(err)
	assert.Equal(int64(5), count, "the shared message is stored once")
	other, err := st.GetSourceByTypeAndIdentifier(t.Context(), whatsAppSourceType, "+41700000009")
	assert.True(err != nil || other == nil, "another Matrix user's login gets no source")

	ids, err := st.MessageExistsBatch(source.ID, []string{"WA-IN", "WA-OUT", "WA-GROUP", "WA-OLD", "WA-DESKTOP"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(t.Context(), ids["WA-OUT"])
	require.NoError(err)
	assert.Equal("hi back", body, "latest edit by the original sender wins; reply fallback is stripped")
	body, err = st.GetMessageBodyText(t.Context(), ids["WA-GROUP"])
	require.NoError(err)
	assert.Equal("[image]", body)

	type row struct {
		conv, phone, name string
		fromMe            bool
		replyTo           sql.NullInt64
		edited            bool
	}
	read := func(messageID int64) row {
		var r row
		require.NoError(st.DB().QueryRow(`
			SELECT c.source_conversation_id, COALESCE(p.phone_number, ''), COALESCE(p.display_name, ''),
			       COALESCE(m.source_is_from_me, 0), m.reply_to_message_id, COALESCE(m.is_edited, 0)
			FROM messages m JOIN conversations c ON c.id = m.conversation_id
			LEFT JOIN participants p ON p.id = m.sender_id
			WHERE m.id = ?`, messageID).Scan(&r.conv, &r.phone, &r.name, &r.fromMe, &r.replyTo, &r.edited))
		return r
	}
	in := read(ids["WA-IN"])
	assert.Equal("33611111111@s.whatsapp.net", in.conv, "bridged DM joins the Desktop conversation")
	assert.Equal("+33611111111", in.phone, "LID sender resolves to the phone identity")
	assert.Equal("Alice Martin", in.name)
	assert.False(in.fromMe)
	out := read(ids["WA-OUT"])
	assert.True(out.fromMe, "own LID is the login")
	assert.Equal("+33600000001", out.phone)
	assert.Equal(ids["WA-IN"], out.replyTo.Int64, "reply links to the WhatsApp message it quotes")
	assert.True(out.edited)
	group := read(ids["WA-GROUP"])
	assert.Equal("120363@g.us", group.conv)
	assert.Equal("+33622222222", group.phone)
	var title string
	require.NoError(st.DB().QueryRow(`SELECT COALESCE(title, '') FROM conversations WHERE source_id = ? AND source_conversation_id = '120363@g.us'`, source.ID).Scan(&title))
	assert.Equal("Bike ride", title)
	var identifiers int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participant_identifiers WHERE identifier_type = 'whatsapp' AND identifier_value IN ('+33611111111', '+33622222222')`).Scan(&identifiers))
	assert.Equal(2, identifiers, "senders carry whatsapp phone identifiers, not Matrix ghost IDs")

	// The next run only revisits the window, and only refetches edited messages.
	before := fetches.Load()
	sum, err = ImportWhatsAppBridge(t.Context(), st, &Runtime{Client: client}, bridge, opts)
	require.NoError(err)
	assert.Equal(int64(0), sum.MessagesAdded)
	assert.Equal(int64(0), sum.MessagesUpdated)
	assert.Equal(int64(5), sum.MessagesProcessed, "the month-old message is outside the window")
	assert.Equal(int64(2), fetches.Load()-before, "only the edited and the refused messages are fetched again")
}

func TestOpenWhatsAppBridgeNeverWrites(t *testing.T) {
	f := newBridgeFixture(t)
	bridge, err := OpenWhatsAppBridge(t.Context(), f.path)
	require.NoError(t, err)
	defer func() { _ = bridge.Close() }()
	_, err = bridge.db.Exec(`INSERT INTO portal VALUES ('whatsapp', 'x', '', '!x:example.org', 'x')`)
	require.Error(t, err)
	assert.True(t, strings.Contains(strings.ToLower(err.Error()), "readonly") || strings.Contains(strings.ToLower(err.Error()), "read-only"), err.Error())
}
