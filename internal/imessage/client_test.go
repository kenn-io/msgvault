package imessage

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"log/slog"
	"math"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportAdvancesPastAFullPageOfFailures(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite trigger injection")
	require := require.New(t)
	path, db := testutil.NewIMessageChatDB(t)
	_, err := db.Exec(`INSERT INTO message (date, text) VALUES (725760000, 'one'), (725760001, 'two'), (725760002, 'three')`)
	require.NoError(err)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err)
	_, err = st.DB().Exec(`CREATE TRIGGER fail_message BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END`)
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	client.pageSize = 2
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	summary, err := client.Import(ctx, st, src.ID)
	require.NoError(err)
	assert.Equal(t, 3, summary.Skipped)
	_, err = st.DB().Exec(`DROP TRIGGER fail_message`)
	require.NoError(err)
	retried, err := client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	assert.Equal(t, 3, retried.MessagesAdded)
	assert.Zero(t, retried.Skipped)
}

func TestImportIndexesMessageBodies(t *testing.T) {
	for _, tt := range []struct {
		name string
		text any
		term string
	}{
		{name: "incoming SMS", text: strings.Repeat("padding ", 20) + "oldmarker", term: "oldmarker"},
		{name: "NULL", term: "peer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			ctx := t.Context()
			path, db := testutil.NewIMessageChatDB(t)
			_, err := db.ExecContext(t.Context(), `INSERT INTO message (date,text,cache_has_attachments) VALUES (725760000, ?, 1)`, tt.text)
			require.NoError(err)
			client, err := NewClient(t.Context(), path)
			require.NoError(err)
			t.Cleanup(func() { _ = client.Close() })
			st := testutil.NewTestStore(t)
			if !st.FTS5Available() {
				t.Skip("FTS5 not available")
			}
			src, err := st.GetOrCreateSource("apple_messages", "fixture")
			require.NoError(err)
			summary, err := client.Import(ctx, st, src.ID)
			require.NoError(err)
			assert.Equal(1, summary.MessagesImported)
			assert.Zero(summary.Skipped)
			var id int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_id = ?`), src.ID).Scan(&id))
			ids, total, err := st.SearchMessageIDsQueryContext(ctx, &search.Query{TextTerms: []string{tt.term}}, 10)
			require.NoError(err)
			assert.Equal(int64(1), total)
			assert.Equal([]int64{id}, ids)
			if tt.text == nil {
				return
			}
			_, err = st.RebuildFTSContext(t.Context(), nil)
			require.NoError(err)
			_, err = db.ExecContext(t.Context(), `UPDATE message SET text = ?`, strings.Repeat("padding ", 20)+"newmarker")
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
		})
	}
}

func TestImportPreservesArchivedBodyWhenAttributedBodyCannotBeDecoded(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	ctx := t.Context()
	path, db := testutil.NewIMessageChatDB(t)
	const originalBody = "Synthetic archived body oldmarker"
	_, err := db.ExecContext(t.Context(), `INSERT INTO message (date,text) VALUES (725760000, ?)`, originalBody)
	require.NoError(err)
	var logs bytes.Buffer
	client, err := NewClient(t.Context(), path, WithImessageLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	st := testutil.NewTestStore(t)
	if !st.FTS5Available() {
		t.Skip("FTS5 not available")
	}
	src, err := st.GetOrCreateSource("apple_messages", "fixture")
	require.NoError(err)
	summary, err := client.Import(ctx, st, src.ID)
	require.NoError(err)
	assert.Equal(1, summary.MessagesImported)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = '1'`), src.ID).Scan(&id))

	_, err = db.ExecContext(t.Context(), `UPDATE message SET text = NULL, attributedBody = ? WHERE ROWID = 1`, []byte("not a valid attributed body"))
	require.NoError(err)
	summary, err = client.Import(ctx, st, src.ID)
	require.NoError(err)
	assert.Equal(1, summary.MessagesImported)
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(err)
	assert.Equal(originalBody, body)
	ids, total, err := st.SearchMessageIDsQueryContext(ctx, &search.Query{TextTerms: []string{"oldmarker"}}, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	assert.Equal([]int64{id}, ids)
	assert.Equal(1, summary.BodiesUndecoded)
	assert.Contains(logs.String(), `level=WARN msg="failed to decode attributed body" rowid=1 archived_text_kept=true`)
}

func TestImportRepairsTruncatedLongAttributedBody(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	ctx := context.Background()
	path, db := testutil.NewIMessageChatDB(t)
	full := strings.Repeat("é", 146) + " tailmarker."
	// 0x0130 is the 304-byte length; earlier releases read only its low byte, 48.
	blob := []byte(streamtypedPrefix + "\x81\x30\x01" + full + "\x86")
	_, err := db.Exec(`INSERT INTO message (date,attributedBody) VALUES (725760000, ?)`, blob)
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	st := testutil.NewTestStore(t)
	if !st.FTS5Available() {
		t.Skip("FTS5 not available")
	}
	src, err := st.GetOrCreateSource("apple_messages", "fixture")
	require.NoError(err)
	_, err = client.Import(ctx, st, src.ID)
	require.NoError(err)
	var id int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ?`), src.ID).Scan(&id))
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: full[:48], Valid: true}, sql.NullString{}))

	summary, err := client.Import(ctx, st, src.ID)
	require.NoError(err)
	assert.Zero(summary.BodiesUndecoded)
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal(full, body)
	ids, _, err := st.SearchMessageIDsQueryContext(ctx, &search.Query{TextTerms: []string{"tailmarker"}}, 10)
	require.NoError(err)
	assert.Equal([]int64{id}, ids)
}

func TestImportFTSFailureContinues(t *testing.T) {
	testutil.SkipIfPostgres(t, "drops the SQLite FTS5 table")
	assert, require := assert.New(t), require.New(t)
	path, db := testutil.NewIMessageChatDB(t)
	_, err := db.ExecContext(t.Context(), `INSERT INTO message (date,text) VALUES (725760000, 'Synthetic body'), (725760001, 'Subsequent body')`)
	require.NoError(err)
	var logs bytes.Buffer
	client, err := NewClient(t.Context(), path, WithImessageLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	st := testutil.NewTestStore(t)
	if !st.FTS5Available() {
		t.Skip("FTS5 not available")
	}
	src, err := st.GetOrCreateSource("apple_messages", "fixture")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `DROP TABLE messages_fts`)
	require.NoError(err)
	summary, err := client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	assert.Equal(2, summary.MessagesImported)
	assert.Zero(summary.Skipped)
	assert.Contains(logs.String(), "failed to index message for search")
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_id = ?`), src.ID).Scan(&id))
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(err)
	assert.Equal("Synthetic body", body)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.NotEmpty(raw)
	var recipients, labels int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`, id).Scan(&recipients))
	assert.Equal(2, recipients)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM message_labels WHERE message_id = ?`, id).Scan(&labels))
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
			ctx := t.Context()
			path, db := testutil.NewIMessageChatDB(t)
			_, err := db.ExecContext(t.Context(), `INSERT INTO message (date,text,attributedBody,cache_has_attachments) VALUES (?,?,?,?)`, tt.date, tt.text, tt.attributed, tt.attachments)
			require.NoError(err)
			c, err := NewClient(t.Context(), path)
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
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id,sent_at,internal_date,has_attachments FROM messages WHERE source_id = ? AND source_message_id = '1'`), src.ID).Scan(&id, &sent, &internal, &hasAttachments))
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
					body, err := st.GetMessageBodyText(t.Context(), id)
					require.NoError(err)
					assert.Equal(tt.wantBody, body)
				}
				var recipients, labels int
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`), id).Scan(&recipients))
				assert.Equal(2, recipients)
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM message_labels WHERE message_id = ?`), id).Scan(&labels))
				assert.Equal(1, labels)
				if run == 0 {
					// Reproduce a previously poisoned archive row; an unfiltered rerun must clear it.
					badDate := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
					_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET sent_at = ?,internal_date = ? WHERE id = ?`), badDate, badDate, id)
					require.NoError(err)
				}
			}
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), src.ID).Scan(&count))
			assert.Equal(1, count)
		})
	}
}

func TestImportDateFiltersIgnoreSentinelsInFormatDetection(t *testing.T) {
	for _, useNano := range []bool{false, true} {
		t.Run(strconv.FormatBool(useNano), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			ctx := t.Context()
			path, db := testutil.NewIMessageChatDB(t)
			dates := []int64{725760000, 757382400, math.MinInt64, math.MaxInt64}
			if useNano {
				dates[0], dates[1] = 725760000000000000, 757382400000000000
			}
			for _, date := range dates {
				_, err := db.ExecContext(t.Context(), `INSERT INTO message (date,text) VALUES (?, 'Synthetic message')`, date)
				require.NoError(err)
			}
			c, err := NewClient(t.Context(), path,
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
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT source_message_id,sent_at,internal_date FROM messages WHERE source_id = ?`), src.ID).Scan(&sourceMessageID, &sent, &internal))
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
		path, db := testutil.NewIMessageChatDB(t)
		for _, date := range dates {
			_, err := db.ExecContext(t.Context(), `INSERT INTO message (date) VALUES (?)`, date)
			require.NoError(err)
		}
		c, err := NewClient(t.Context(), path)
		require.NoError(err)
		assert.False(c.useNanoseconds)
		require.NoError(c.Close())
	}
}

func TestNewClientDoesNotRequireHomeWhenAttachmentCaptureIsDisabled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows resolves the home directory from USERPROFILE")
	}
	path, _ := testutil.NewIMessageChatDB(t)
	t.Setenv("HOME", "")
	client, err := NewClient(t.Context(), path, WithAttachments("", "", attachmentpolicy.Policy{}))
	require.NoError(t, err)
	require.NoError(t, client.Close())
}

func TestImportClearsSearchIndexWhenMessageTextIsRemoved(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	ctx := context.Background()
	path, db := testutil.NewIMessageChatDB(t)
	_, err := db.Exec(`INSERT INTO message (date,text) VALUES (725760000, 'Synthetic oldmarker body')`)
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	st := testutil.NewTestStore(t)
	if !st.FTS5Available() {
		t.Skip("FTS5 not available")
	}
	src, err := st.GetOrCreateSource("apple_messages", "fixture")
	require.NoError(err)
	summary, err := client.Import(ctx, st, src.ID)
	require.NoError(err)
	assert.Equal(1, summary.MessagesImported)
	var id int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT id FROM messages WHERE source_id = ?`), src.ID).Scan(&id))
	ids, total, err := st.SearchMessageIDsQueryContext(ctx, &search.Query{TextTerms: []string{"oldmarker"}}, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	assert.Equal([]int64{id}, ids)

	_, err = db.Exec(`UPDATE message SET text = NULL`)
	require.NoError(err)
	summary, err = client.Import(ctx, st, src.ID)
	require.NoError(err)
	assert.Equal(1, summary.MessagesImported)
	assert.Equal(1, summary.MessagesUpdated)
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Empty(body)
	ids, total, err = st.SearchMessageIDsQueryContext(ctx, &search.Query{TextTerms: []string{"oldmarker"}}, 10)
	require.NoError(err)
	assert.Zero(total)
	assert.Empty(ids)
}

func TestImportCancellationIsNotAnAttachmentFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, st, source := importOnlyFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	summary, err := c.Import(cancelOnceStored{Context: ctx, cancel: cancel, db: st.DB()}, st, source)
	require.ErrorIs(err, context.Canceled)
	require.NotErrorIs(err, ErrAttachmentImport)
	assert.Equal(1, summary.MessagesAdded)
	assert.Equal(summary.MessagesAdded+summary.MessagesUpdated, summary.MessagesImported)
	assert.Zero(summary.AttachmentFailures)
}

func TestImportRerunWritesOnlyChanges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path, db := testutil.NewIMessageChatDB(t)
	_, err := db.Exec(`INSERT INTO message (date,text) VALUES (725760000,'one'), (725760001,'two'), (725760002,'three')`)
	require.NoError(err)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	first, err := client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	assert.Equal(3, first.MessagesAdded)
	// Pin a past modification time so even same-tick rewrites are observable.
	_, err = st.DB().Exec(`UPDATE messages SET last_modified = '2020-01-01 00:00:00'`)
	require.NoError(err)
	var before, after time.Time
	require.NoError(st.DB().QueryRow(`SELECT last_modified FROM messages ORDER BY last_modified DESC LIMIT 1`).Scan(&before))
	idle, err := client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	assert.Zero(idle.MessagesAdded)
	assert.Zero(idle.MessagesUpdated)
	require.NoError(st.DB().QueryRow(`SELECT last_modified FROM messages ORDER BY last_modified DESC LIMIT 1`).Scan(&after))
	assert.Equal(before, after)
	_, err = db.Exec(`UPDATE message SET text = 'edited' WHERE ROWID = 2;
 INSERT INTO message (date,text) VALUES (723168000,'late arrival')`)
	require.NoError(err)
	edited, err := client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	assert.Equal(1, edited.MessagesAdded)
	assert.Equal(1, edited.MessagesUpdated)
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = (SELECT id FROM messages WHERE source_id = ? AND source_message_id = '2')`), src.ID).Scan(&body))
	assert.Equal("edited", body)
}

func TestImportRepairsMissingProviderLabel(t *testing.T) {
	for _, service := range []string{"iMessage", "SMS"} {
		t.Run(service, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			path, db := testutil.NewIMessageChatDB(t)
			_, err := db.Exec(`INSERT INTO message (date,text,service) VALUES (725760000,'hello',?)`, service)
			require.NoError(err)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("apple_messages", "local")
			require.NoError(err)
			client, err := NewClient(path)
			require.NoError(err)
			t.Cleanup(func() { _ = client.Close() })
			_, err = client.Import(t.Context(), st, src.ID)
			require.NoError(err)
			var id int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ?`), src.ID).Scan(&id))
			provider, err := st.EnsureLabel(src.ID, service, service, "user")
			require.NoError(err)
			custom, err := st.EnsureLabel(src.ID, "custom", "custom", "user")
			require.NoError(err)
			require.NoError(st.LinkMessageLabel(id, custom))
			// Older imports committed body and raw data before linking the provider label.
			_, err = st.DB().Exec(st.Rebind(`DELETE FROM message_labels WHERE message_id = ? AND label_id = ?`), id, provider)
			require.NoError(err)
			summary, err := client.Import(t.Context(), st, src.ID)
			require.NoError(err)
			labels, err := st.MessageLabelIDsContext(t.Context(), id)
			require.NoError(err)
			assert.ElementsMatch([]int64{custom, provider}, labels)
			assert.Equal(1, summary.MessagesUpdated)
			idle, err := client.Import(t.Context(), st, src.ID)
			require.NoError(err)
			assert.Zero(idle.MessagesUpdated)
		})
	}
}

func TestImportLimitAdvancesPastDuplicateChatRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path, db := testutil.NewIMessageChatDB(t)
	_, err := db.Exec(`INSERT INTO message (date,text) VALUES (725760000,'one'), (725760001,'two'), (725760002,'three');
		INSERT INTO chat (guid) VALUES ('any;-;one'), ('any;-;two');
		INSERT INTO chat_message_join VALUES (1,1), (2,1)`)
	require.NoError(err)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err)
	client, err := NewClient(path, WithLimit(2))
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })

	summary, err := client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	assert.Equal(2, summary.MessagesAdded)
	var count int
	var lastID string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*), MAX(source_message_id) FROM messages WHERE source_id = ?`), src.ID).Scan(&count, &lastID))
	assert.Equal(2, count)
	assert.Equal("2", lastID)
}

func TestImportMultipleChatAssociationsStayStable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path, db := testutil.NewIMessageChatDB(t)
	_, err := db.Exec(`INSERT INTO message (date,text) VALUES (725760000,'shared');
		INSERT INTO chat (guid) VALUES ('any;-;one'), ('any;-;two');
		INSERT INTO chat_message_join VALUES (1,1), (2,1)`)
	require.NoError(err)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	first, err := client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	assert.Equal(1, first.MessagesAdded)
	assert.Zero(first.MessagesUpdated)
	for _, pageSize := range []int{client.pageSize, 1} {
		client.pageSize = pageSize
		idle, err := client.Import(t.Context(), st, src.ID)
		require.NoError(err)
		assert.Zero(idle.MessagesAdded)
		assert.Zero(idle.MessagesUpdated, "page size %d", pageSize)
	}
}

func TestImportFailedRecipientResolutionPreservesArchive(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite trigger injection")
	for _, tt := range []struct {
		name          string
		isFromMe      int
		recipientType string
		failOwner     bool
	}{
		{name: "outgoing", isFromMe: 1, recipientType: "to"},
		{name: "incoming", isFromMe: 0, recipientType: "from"},
		{name: "default owner", isFromMe: 0, recipientType: "to", failOwner: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			path, db := testutil.NewIMessageChatDB(t)
			_, err := db.Exec(`INSERT INTO message (date,text,is_from_me) VALUES (725760000,'message',?);
 INSERT INTO chat VALUES ('any;-;synthetic', '', 'peer@example.test');
 INSERT INTO chat_message_join VALUES (1,1);
 INSERT INTO chat_handle_join VALUES (1,1)`, tt.isFromMe)
			require.NoError(err)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("apple_messages", "local")
			require.NoError(err)
			client, err := NewClient(path)
			require.NoError(err)
			t.Cleanup(func() { _ = client.Close() })
			_, err = client.Import(t.Context(), st, src.ID)
			require.NoError(err)
			query := `SELECT p.email_address FROM message_recipients r JOIN participants p ON p.id = r.participant_id WHERE r.recipient_type = ?`
			var recipient string
			require.NoError(st.DB().QueryRow(query, tt.recipientType).Scan(&recipient))
			originalRecipient := "peer@example.test"
			if tt.failOwner {
				originalRecipient = "me@imessage.local"
			}
			require.Equal(originalRecipient, recipient)
			_, err = db.Exec(`UPDATE handle SET id = 'new-peer@example.test'`)
			require.NoError(err)
			trigger := `CREATE TRIGGER fail_participant BEFORE INSERT ON participants WHEN NEW.email_address = 'new-peer@example.test' BEGIN SELECT RAISE(ABORT, 'synthetic recipient failure'); END`
			if tt.failOwner {
				trigger = `CREATE TRIGGER fail_participant BEFORE INSERT ON participants WHEN NEW.email_address = 'me@imessage.local' BEGIN SELECT RAISE(ABORT, 'synthetic recipient failure'); END`
			}
			_, err = st.DB().Exec(trigger)
			require.NoError(err)
			summary, err := client.Import(t.Context(), st, src.ID)
			if tt.failOwner {
				require.ErrorContains(err, "resolve default owner")
			} else {
				require.NoError(err)
				assert.Equal(1, summary.Skipped)
			}
			require.NoError(st.DB().QueryRow(query, tt.recipientType).Scan(&recipient))
			assert.Equal(originalRecipient, recipient)
			_, err = st.DB().Exec(`DROP TRIGGER fail_participant`)
			require.NoError(err)
			retried, err := client.Import(t.Context(), st, src.ID)
			require.NoError(err)
			assert.Zero(retried.Skipped)
			assert.Equal(1, retried.MessagesUpdated)
			require.NoError(st.DB().QueryRow(query, tt.recipientType).Scan(&recipient))
			if tt.failOwner {
				assert.Equal("me@imessage.local", recipient)
			} else {
				assert.Equal("new-peer@example.test", recipient)
			}
		})
	}
}

func TestImportOwnerChangeRefreshesRecipients(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path, db := testutil.NewIMessageChatDB(t)
	_, err := db.Exec(`INSERT INTO message (date,text) VALUES (725760000,'incoming')`)
	require.NoError(err)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	_, err = client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	client.ownerHandle = "owner@example.test"
	summary, err := client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	assert.Equal(1, summary.MessagesUpdated)
	var recipient string
	require.NoError(st.DB().QueryRow(`SELECT p.email_address FROM message_recipients r JOIN participants p ON p.id = r.participant_id WHERE r.recipient_type = 'to'`).Scan(&recipient))
	assert.Equal("owner@example.test", recipient)
	idle, err := client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	assert.Zero(idle.MessagesUpdated)
}

func TestImportChangedMessageRollsBackOnRawFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "SQLite trigger injection")
	path, db := testutil.NewIMessageChatDB(t)
	_, err := db.Exec(`INSERT INTO message (date,text) VALUES (725760000,'original')`)
	require.NoError(err)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	_, err = client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	var id int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&id))
	label, err := st.EnsureLabel(src.ID, "custom", "custom", "user")
	require.NoError(err)
	require.NoError(st.LinkMessageLabel(id, label))
	_, err = db.Exec(`UPDATE message SET text = 'edited'`)
	require.NoError(err)
	_, err = st.DB().Exec(`CREATE TRIGGER fail_raw BEFORE UPDATE ON message_raw BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END`)
	require.NoError(err)
	summary, err := client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	assert.Equal(1, summary.Skipped)
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("original", body)
	var snippet string
	require.NoError(st.DB().QueryRow(`SELECT snippet FROM messages WHERE id = ?`, id).Scan(&snippet))
	assert.Equal("original", snippet)
	_, err = st.DB().Exec(`DROP TRIGGER fail_raw`)
	require.NoError(err)
	summary, err = client.Import(t.Context(), st, src.ID)
	require.NoError(err)
	assert.Equal(1, summary.MessagesUpdated)
	body, err = st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("edited", body)
	labels, err := st.MessageLabelIDsContext(t.Context(), id)
	require.NoError(err)
	assert.Contains(labels, label)
}
