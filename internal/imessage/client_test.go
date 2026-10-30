package imessage

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"math"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	internalmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func newChatDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	require.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
 CREATE TABLE message (
  guid TEXT NOT NULL DEFAULT 'synthetic-message', text TEXT, attributedBody BLOB,
  date INTEGER NOT NULL, is_from_me INTEGER NOT NULL DEFAULT 0,
  service TEXT DEFAULT 'SMS', cache_has_attachments INTEGER NOT NULL DEFAULT 0,
  handle_id INTEGER DEFAULT 1
 );
 CREATE TABLE handle (id TEXT);
 CREATE TABLE chat (guid TEXT, display_name TEXT, chat_identifier TEXT);
 CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER);
 CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER);
 INSERT INTO handle (id) VALUES ('peer@example.test');
 `)
	require.NoError(err)
	return path, db
}

func TestImportEmailHandlesKeepSourceEvidenceAndRecipientAddresses(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	path, db := newChatDB(t)
	_, err := db.Exec(`INSERT INTO message (date,text) VALUES (?,?)`, 725760000, "Synthetic email handle message")
	requirements.NoError(err)
	client, err := NewClient(path, WithOwnerHandle("owner@example.test"))
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("apple_messages", "synthetic-email-handles")
	requirements.NoError(err)
	preexisting, err := st.EnsureParticipantsBatch([]internalmime.Address{{Email: "peer@example.test"}})
	requirements.NoError(err)
	preexistingPeerID := preexisting["peer@example.test"]
	requirements.NotZero(preexistingPeerID)

	for range 2 {
		summary, err := client.Import(t.Context(), st, source.ID)
		requirements.NoError(err)
		assertions.Equal(1, summary.MessagesImported)
	}

	var peerID, messageID int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT id FROM participants WHERE email_address=?`), "peer@example.test").Scan(&peerID))
	assertions.Equal(preexistingPeerID, peerID,
		"native import must recover provenance for participant rows created by an older import")
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT id FROM messages WHERE source_id=? AND source_message_id='1'`), source.ID).Scan(&messageID))
	var fromEmail string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT email_address
		FROM message_recipients WHERE message_id=? AND recipient_type='from' AND participant_id=?`),
		messageID, peerID).Scan(&fromEmail))
	assertions.Equal("peer@example.test", fromEmail,
		"native message recipients must retain the exact email envelope address")
	var ownerEmail string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT email_address
		FROM message_recipients WHERE message_id=? AND recipient_type='to'`), messageID).Scan(&ownerEmail))
	assertions.Equal("owner@example.test", ownerEmail)

	observations, err := st.ListParticipantObservationsContext(t.Context(), peerID, true)
	requirements.NoError(err)
	requirements.Len(observations, 1,
		"reimporting the same native handle must reuse its source-qualified email evidence")
	assertions.NotNil(observations[0].SourceID)
	assertions.Equal(source.ID, *observations[0].SourceID)
	requirements.NotNil(observations[0].Envelope.SourceRef)
	assertions.Contains(*observations[0].Envelope.SourceRef, "imessage:handle:")

	chatSource, err := st.GetOrCreateSource("beeper", "synthetic-cold-email-evidence")
	requirements.NoError(err)
	room, err := st.EnsureConversation(chatSource.ID, "cold-room", "Cold room")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), chatSource.ID, "cold-room", conversationpolicy.Cold))
	requirements.NoError(st.EnsureConversationParticipantContext(t.Context(), room, peerID, "member"))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE message_recipients
		SET email_address=NULL WHERE participant_id=?`), peerID)
	requirements.NoError(err)
	var eligible bool
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT `+
		store.FullParticipantValueWhere("p.id", "'email'", "p.email_address")+
		` FROM participants p WHERE p.id=?`), peerID).Scan(&eligible))
	assertions.True(eligible,
		"the independently imported iMessage observation must keep the email eligible after a Cold chat is linked")
}

func TestImportInvalidDatesRetainsMessages(t *testing.T) {
	cases := []struct {
		name        string
		date        int64
		text        any
		attributed  []byte
		attachments int
		wantBody    string
	}{
		{name: "minimum sentinel with body", date: math.MinInt64, text: "Synthetic message", wantBody: "Synthetic message"},
		{name: "maximum sentinel attributed body", date: math.MaxInt64, attributed: []byte("\x04\x0bstreamtyped\x84\x01+\x09Synthetic"), wantBody: "Synthetic"},
		{name: "empty placeholder", date: math.MinInt64},
		{name: "attachment only", date: math.MaxInt64, attachments: 1},
		{name: "zero timestamp", date: 0, text: "No date", wantBody: "No date"},
		{name: "calendar overflow", date: 252423993600, text: "No date", wantBody: "No date"},
		{name: "calendar underflow", date: -63113904001},
		{name: "Go zero instant", date: -63113904000},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			ctx := context.Background()
			path, db := newChatDB(t)
			_, err := db.Exec(`INSERT INTO message (date,text,attributedBody,cache_has_attachments) VALUES (?,?,?,?)`, tt.date, tt.text, tt.attributed, tt.attachments)
			require.NoError(err)
			c, err := NewClient(path)
			require.NoError(err)
			t.Cleanup(func() { _ = c.Close() })
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("apple_messages", "local")
			require.NoError(err)
			var originalID int64
			for run := range 2 {
				summary, err := c.Import(ctx, st, src.ID)
				require.NoError(err)
				assert.Equal(1, summary.MessagesImported)
				wantDatesCleared := 0
				if run == 1 {
					wantDatesCleared = 1
				}
				assert.Equal(wantDatesCleared, summary.DatesCleared)
				assert.Zero(summary.Skipped)
				var id int64
				var sent, internal sql.NullTime
				var hasAttachments bool
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id,sent_at,internal_date,has_attachments FROM messages WHERE source_id = ? AND source_message_id = '1'`), src.ID).Scan(&id, &sent, &internal, &hasAttachments))
				assert.False(sent.Valid)
				assert.False(internal.Valid)
				assert.Equal(tt.attachments != 0, hasAttachments)
				if run == 0 {
					originalID = id
				} else {
					assert.Equal(originalID, id)
				}
				raw, err := st.GetMessageRaw(id)
				require.NoError(err)
				var evidence struct {
					Date int64  `json:"date"`
					Body string `json:"body"`
				}
				require.NoError(json.Unmarshal(raw, &evidence))
				assert.Equal(tt.date, evidence.Date)
				assert.Equal(tt.wantBody, evidence.Body)
				if tt.wantBody != "" {
					body, err := st.GetMessageBodyText(id)
					require.NoError(err)
					assert.Equal(tt.wantBody, body)
				}
				var recipients, labels int
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`), id).Scan(&recipients))
				assert.Equal(2, recipients)
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_labels WHERE message_id = ?`), id).Scan(&labels))
				assert.Equal(1, labels)
				if run == 0 {
					// Reproduce a previously poisoned archive row; an unfiltered rerun must clear it.
					badDate := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
					_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET sent_at = ?,internal_date = ? WHERE id = ?`), badDate, badDate, id)
					require.NoError(err)
				}
			}
			var count int
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), src.ID).Scan(&count))
			assert.Equal(1, count)
		})
	}
}

func TestImportDateFiltersIgnoreSentinelsInFormatDetection(t *testing.T) {
	for _, useNano := range []bool{false, true} {
		t.Run(strconv.FormatBool(useNano), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			ctx := context.Background()
			path, db := newChatDB(t)
			dates := []int64{725760000, 757382400, math.MinInt64, math.MaxInt64}
			if useNano {
				dates[0], dates[1] = 725760000000000000, 757382400000000000
			}
			for _, date := range dates {
				_, err := db.Exec(`INSERT INTO message (date,text) VALUES (?, 'Synthetic message')`, date)
				require.NoError(err)
			}
			c, err := NewClient(path,
				WithAfterDate(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				WithBeforeDate(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)))
			require.NoError(err)
			t.Cleanup(func() { _ = c.Close() })
			assert.Equal(int64(1), c.CountFilteredMessages(ctx))
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("apple_messages", "local")
			require.NoError(err)
			summary, err := c.Import(ctx, st, src.ID)
			require.NoError(err)
			assert.Equal(1, summary.MessagesImported)
			var sourceMessageID string
			var sent, internal sql.NullTime
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_message_id,sent_at,internal_date FROM messages WHERE source_id = ?`), src.ID).Scan(&sourceMessageID, &sent, &internal))
			assert.Equal("1", sourceMessageID)
			want := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
			assert.True(sent.Valid)
			assert.Equal(want, sent.Time.UTC())
			assert.True(internal.Valid)
			assert.Equal(want, internal.Time.UTC())
		})
	}
}

func TestTimestampDetectionWithoutRealPositiveDates(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	for _, dates := range [][]int64{nil, {0, math.MinInt64, math.MaxInt64}} {
		path, db := newChatDB(t)
		for _, date := range dates {
			_, err := db.Exec(`INSERT INTO message (date) VALUES (?)`, date)
			require.NoError(err)
		}
		c, err := NewClient(path)
		require.NoError(err)
		assert.False(c.useNanoseconds)
		require.NoError(c.Close())
	}
}
