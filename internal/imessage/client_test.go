package imessage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
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

func TestImportIndexesMessageBodies(t *testing.T) {
	testutil.SkipIfPostgres(t, "inspects and repairs the SQLite FTS5 table")
	for _, tt := range []struct {
		name       string
		service    string
		outgoing   bool
		owner      string
		handle     any
		attributed bool
		wantFrom   string
		wantTo     string
	}{
		{name: "incoming SMS", service: "SMS", handle: "peer@example.test", wantFrom: "peer@example.test", wantTo: "me@imessage.local"},
		{name: "incoming attributed iMessage", service: "iMessage", handle: "peer@example.test", attributed: true, wantFrom: "peer@example.test", wantTo: "me@imessage.local"},
		{name: "outgoing iMessage", service: "iMessage", outgoing: true, owner: "owner@example.test", handle: "peer@example.test", wantFrom: "owner@example.test", wantTo: "peer@example.test"},
		{name: "outgoing SMS phone", service: "SMS", outgoing: true, owner: "+12025550100", handle: "+12025550101", wantFrom: "+12025550100", wantTo: ""},
		{name: "unknown owner", service: "SMS", outgoing: true, handle: "peer@example.test", wantFrom: "me@imessage.local", wantTo: "peer@example.test"},
		{name: "missing handle", service: "iMessage", wantTo: "me@imessage.local"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			ctx := context.Background()
			path, db := newChatDB(t)
			_, err := db.Exec(`UPDATE handle SET id = ?`, tt.handle)
			require.NoError(err)
			body := strings.Repeat("padding ", 20) + "oldmarker"
			var text any = body
			var attributed []byte
			if tt.attributed {
				text = nil
				attributed = append([]byte("\x04\x0bstreamtyped\x84\x01+\x81"), byte(len(body)))
				attributed = append(attributed, body...)
			}
			_, err = db.Exec(`INSERT INTO message (date,text,attributedBody,service,is_from_me) VALUES (?,?,?,?,?)`, 725760000, text, attributed, tt.service, tt.outgoing)
			require.NoError(err)
			client, err := NewClient(path, WithOwnerHandle(tt.owner))
			require.NoError(err)
			t.Cleanup(func() { _ = client.Close() })
			st := testutil.NewTestStore(t)
			require.True(st.FTS5Available())
			src, err := st.GetOrCreateSource("apple_messages", "fixture")
			require.NoError(err)
			for range 2 {
				summary, err := client.Import(ctx, st, src.ID)
				require.NoError(err)
				assert.Equal(1, summary.MessagesImported)
				assert.Zero(summary.Skipped)
			}
			var id int64
			require.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ?`, src.ID).Scan(&id))
			ids, total, err := st.SearchMessageIDsQueryContext(ctx, &search.Query{TextTerms: []string{"oldmarker"}}, 10)
			require.NoError(err)
			assert.Equal(int64(1), total)
			assert.Equal([]int64{id}, ids)
			var subject, indexedBody, from, to, cc string
			require.NoError(st.DB().QueryRow(`SELECT subject,body,from_addr,to_addr,cc_addr FROM messages_fts WHERE rowid = ?`, id).Scan(&subject, &indexedBody, &from, &to, &cc))
			assert.Equal(body, indexedBody)
			assert.Equal(tt.wantFrom, from)
			assert.Empty(subject)
			assert.Equal(tt.wantTo, to)
			assert.Empty(cc)
			var count int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ?`, id).Scan(&count))
			assert.Equal(1, count)

			// Simulate an older archive that needed a real backfill before re-import.
			_, err = st.DB().Exec(`DELETE FROM messages_fts WHERE rowid = ?`, id)
			require.NoError(err)
			inserted, err := st.BackfillFTS(nil)
			require.NoError(err)
			require.Equal(int64(1), inserted)
			_, err = db.Exec(`UPDATE message SET text = ?, attributedBody = NULL`, strings.Repeat("padding ", 20)+"newmarker")
			require.NoError(err)
			_, err = client.Import(ctx, st, src.ID)
			require.NoError(err)
			ids, total, err = st.SearchMessageIDsQueryContext(ctx, &search.Query{TextTerms: []string{"newmarker"}}, 10)
			require.NoError(err)
			assert.Equal(int64(1), total)
			assert.Equal([]int64{id}, ids)
			ids, total, err = st.SearchMessageIDsQueryContext(ctx, &search.Query{TextTerms: []string{"oldmarker"}}, 10)
			require.NoError(err)
			assert.Zero(total)
			assert.Empty(ids)

			// Empty source text currently retains the stored body; keep it searchable.
			for _, empty := range []any{"", nil} {
				_, err = db.Exec(`UPDATE message SET text = ?`, empty)
				require.NoError(err)
				_, err = client.Import(ctx, st, src.ID)
				require.NoError(err)
				ids, _, err = st.SearchMessageIDsQueryContext(ctx, &search.Query{TextTerms: []string{"newmarker"}}, 10)
				require.NoError(err)
				assert.Equal([]int64{id}, ids)
				storedBody, err := st.GetMessageBodyText(id)
				require.NoError(err)
				assert.Equal(strings.Repeat("padding ", 20)+"newmarker", storedBody)
			}
		})
	}
}

func TestImportReimportPreservesBackfilledRecipients(t *testing.T) {
	testutil.SkipIfPostgres(t, "inspects and repairs the SQLite FTS5 table")
	for _, tt := range []struct {
		name       string
		outgoing   bool
		wantTo     string
		searchTerm string
	}{
		{name: "incoming owner recipient", wantTo: "owner@example.test", searchTerm: "owner"},
		{name: "outgoing peer recipient", outgoing: true, wantTo: "peer@example.test", searchTerm: "peer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			ctx := context.Background()
			path, db := newChatDB(t)
			_, err := db.Exec(`INSERT INTO message (date,text,is_from_me) VALUES (725760000, ?, ?)`, strings.Repeat("padding ", 20)+"oldmarker", tt.outgoing)
			require.NoError(err)

			client, err := NewClient(path, WithOwnerHandle("owner@example.test"))
			require.NoError(err)
			t.Cleanup(func() { _ = client.Close() })
			st := testutil.NewTestStore(t)
			require.True(st.FTS5Available())
			src, err := st.GetOrCreateSource("apple_messages", "fixture")
			require.NoError(err)
			_, err = client.Import(ctx, st, src.ID)
			require.NoError(err)

			var id int64
			require.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ?`, src.ID).Scan(&id))
			_, err = st.DB().Exec(`DELETE FROM messages_fts WHERE rowid = ?`, id)
			require.NoError(err)
			inserted, err := st.BackfillFTS(nil)
			require.NoError(err)
			require.Equal(int64(1), inserted)

			_, err = db.Exec(`UPDATE message SET text = ?`, strings.Repeat("padding ", 20)+"newmarker")
			require.NoError(err)
			_, err = client.Import(ctx, st, src.ID)
			require.NoError(err)

			var to string
			require.NoError(st.DB().QueryRow(`SELECT to_addr FROM messages_fts WHERE rowid = ?`, id).Scan(&to))
			assert.Equal(tt.wantTo, to)
			ids, _, err := st.SearchMessageIDsQueryContext(ctx, &search.Query{TextTerms: []string{tt.searchTerm}}, 10)
			require.NoError(err)
			assert.Equal([]int64{id}, ids)
		})
	}
}

func TestImportCanceledEnvelopeReadPreservesBackfilledRecipients(t *testing.T) {
	testutil.SkipIfPostgres(t, "inspects and repairs the SQLite FTS5 table")
	assert, require := assert.New(t), require.New(t)
	oldBody := strings.Repeat("padding ", 20) + "oldmarker"
	path, chatDB := newChatDB(t)
	_, err := chatDB.Exec(`INSERT INTO message (date,text) VALUES (725760000, ?)`, oldBody)
	require.NoError(err)

	var logs bytes.Buffer
	client, err := NewClient(path,
		WithOwnerHandle("owner@example.test"),
		WithImessageLogger(slog.New(slog.NewTextHandler(&logs, nil))),
	)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	st := testutil.NewTestStore(t)
	require.True(st.FTS5Available())
	src, err := st.GetOrCreateSource("apple_messages", "fixture")
	require.NoError(err)
	_, err = client.Import(context.Background(), st, src.ID)
	require.NoError(err)

	var id int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ?`, src.ID).Scan(&id))
	_, err = st.DB().Exec(`DELETE FROM messages_fts WHERE rowid = ?`, id)
	require.NoError(err)
	inserted, err := st.BackfillFTS(nil)
	require.NoError(err)
	require.Equal(int64(1), inserted)
	ids, total, err := st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{"owner"}}, 10)
	require.NoError(err)
	require.Equal(int64(1), total)
	require.Equal([]int64{id}, ids)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var canceled atomic.Bool
	st.DB().SetMaxOpenConns(1)
	st.DB().SetMaxIdleConns(1)
	conn, err := st.DB().Conn(context.Background())
	require.NoError(err)
	err = conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		if !ok {
			return errors.New("test store is not using SQLite")
		}
		return sqliteConn.RegisterFunc("cancel_import_context", func() int64 {
			canceled.Store(true)
			cancel()
			return 1
		}, false)
	})
	require.NoError(err)
	require.NoError(conn.Close())
	_, err = st.DB().Exec(`
		CREATE TRIGGER cancel_import_context_after_recipient_delete
		AFTER DELETE ON message_recipients
		BEGIN
			SELECT cancel_import_context();
		END
	`)
	require.NoError(err)
	_, err = client.Import(ctx, st, src.ID)
	require.NoError(err)
	require.True(canceled.Load(), "recipient replacement should cancel the envelope lookup")
	assert.Contains(logs.String(), "failed to load stored message envelope for search")
	assert.Contains(logs.String(), "context canceled")

	var indexedBody, to string
	require.NoError(st.DB().QueryRow(`SELECT body,to_addr FROM messages_fts WHERE rowid = ?`, id).Scan(&indexedBody, &to))
	assert.Equal(oldBody, indexedBody)
	assert.Equal("owner@example.test", to)
	ids, total, err = st.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{"owner"}}, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	assert.Equal([]int64{id}, ids)
}

func TestImportFTSFailureContinues(t *testing.T) {
	testutil.SkipIfPostgres(t, "drops the SQLite FTS5 table")
	assert, require := assert.New(t), require.New(t)
	path, db := newChatDB(t)
	_, err := db.Exec(`INSERT INTO message (date,text) VALUES (725760000, 'Synthetic body'), (725760001, 'Subsequent body')`)
	require.NoError(err)
	var logs bytes.Buffer
	client, err := NewClient(path, WithImessageLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("apple_messages", "fixture")
	require.NoError(err)
	_, err = st.DB().Exec(`DROP TABLE messages_fts`)
	require.NoError(err)
	summary, err := client.Import(context.Background(), st, src.ID)
	require.NoError(err)
	assert.Equal(2, summary.MessagesImported)
	assert.Zero(summary.Skipped)
	assert.Contains(logs.String(), "failed to index message for search")
	var id int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ?`, src.ID).Scan(&id))
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("Synthetic body", body)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.NotEmpty(raw)
	var recipients, labels int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`, id).Scan(&recipients))
	assert.Equal(2, recipients)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_labels WHERE message_id = ?`, id).Scan(&labels))
	assert.Equal(1, labels)
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
