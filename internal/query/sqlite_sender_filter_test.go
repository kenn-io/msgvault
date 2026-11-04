//go:build cgo

package query_test

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestSenderListBoundedWork(t *testing.T) {
	dbtest.SkipIfPostgres(t, "SQLite row-visit instrumentation")
	for _, size := range []int{3000, 30000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			setupRequire := require.New(t)
			path := filepath.Join(t.TempDir(), "archive.db")
			st := testutil.NewSQLiteTestStoreAt(t, path)
			fixture := &dbtest.TestDB{DB: st.DB(), T: t}
			fixture.SeedStandardDataSet()
			_, err := st.DB().Exec(fmt.Sprintf(`WITH RECURSIVE ids(id) AS (SELECT 100 UNION ALL SELECT id+1 FROM ids WHERE id<%d)
 INSERT INTO messages(id,source_id,conversation_id,source_message_id,sent_at,sender_id,message_type)
 SELECT id,1,1,'bulk-' || id,'2024-06-01 00:00:00',CASE WHEN id<=1100 THEN 2 ELSE 1 END,'email' FROM ids;
 INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES (100,3,'from');
 UPDATE participants SET email_address='old@example.org' WHERE id=2;
 INSERT INTO message_labels(message_id,label_id) VALUES (100,1);
 INSERT INTO message_recipients(message_id,participant_id,recipient_type)
 SELECT id,1,'from' FROM messages WHERE sender_id=1;
 INSERT INTO sources(id,source_type,identifier) VALUES (2,'gmail','small@example.com');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES (2,2,'small','email_thread');
 WITH RECURSIVE ids(id) AS (SELECT 40001 UNION ALL SELECT id+1 FROM ids WHERE id<40010)
 INSERT INTO messages(id,source_id,conversation_id,source_message_id,sent_at,sender_id,message_type)
 SELECT id,2,2,'small-' || id,'2020-01-01 00:00:00',CASE WHEN id=40001 THEN 2 ELSE 1 END,'email' FROM ids`, size+100))
			setupRequire.NoError(err)
			setupRequire.NoError(st.Close())
			st, err = store.Open(path) // Include the production open-time optimizer.
			setupRequire.NoError(err)
			defer func() { setupRequire.NoError(st.Close()) }()
			st.DB().SetMaxOpenConns(1)
			conn, err := st.DB().Conn(t.Context())
			setupRequire.NoError(err)
			visits := 0
			setupRequire.NoError(conn.Raw(func(raw any) error {
				c, ok := raw.(*sqlite3.SQLiteConn)
				setupRequire.True(ok)
				return c.RegisterFunc("record_message_visit", func(int64) bool { visits++; return true }, false)
			}))
			// A flattened view keeps the real message indexes and counts rows evaluated
			// by the production query, including its unsuccessful date walk.
			_, err = conn.ExecContext(t.Context(), `CREATE TEMP VIEW messages AS SELECT * FROM main.messages WHERE record_message_visit(id)`)
			setupRequire.NoError(err)
			setupRequire.NoError(conn.Close())
			for _, tc := range []struct {
				sender, label string
				want          int
			}{
				{"carol@example.com", "", 1}, {"old@example.org", "", 100}, {"absent@example.com", "", 0}, {"old@example.org", "INBOX", 3},
			} {
				t.Run(tc.sender+tc.label, func(t *testing.T) {
					require := require.New(t)
					visits = 0
					messages, err := query.NewSQLiteEngine(st.DB()).ListMessages(t.Context(), query.MessageFilter{Sender: tc.sender, Label: tc.label,
						Sorting: query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortDesc}, Pagination: query.Pagination{Limit: 100}})
					require.NoError(err)
					assert.Len(t, messages, tc.want)
					// The probe, membership query, and result query each visit sender candidates.
					assert.Less(t, visits, 4000, "sender work must stay bounded as unrelated messages grow")
				})
			}
			for _, scope := range []string{"single", "multiple", "context"} {
				t.Run("scope/"+scope, func(t *testing.T) {
					require := require.New(t)
					assert := assert.New(t)
					ctx := t.Context()
					filter := query.MessageFilter{Sender: "old@example.org", Pagination: query.Pagination{Limit: 100}}
					switch scope {
					case "single":
						filter.SourceID = new(int64(2))
					case "multiple":
						filter.SourceIDs = []int64{2}
					case "context":
						ctx = sourcescope.WithSourceIDs(ctx, []int64{2})
					}
					visits = 0
					messages, err := query.NewSQLiteEngine(st.DB()).ListMessages(ctx, filter)
					require.NoError(err)
					ids := make([]int64, len(messages))
					for i, message := range messages {
						ids[i] = message.ID
					}
					assert.Equal([]int64{40001}, ids)
					assert.Less(visits, 4000, "small source filters must retain bounded sender work")
					filter.Sender = "absent@example.org"
					visits = 0
					messages, err = query.NewSQLiteEngine(st.DB()).ListMessages(ctx, filter)
					require.NoError(err)
					assert.Empty(messages)
					assert.Less(visits, 4000, "absent sender work must stay bounded inside a small source")
				})
			}
		})
	}
}

func TestSenderListEquivalence(t *testing.T) {
	dbtest.SkipIfPostgres(t, "SQLite forced-bound comparison")
	st := testutil.NewSQLiteTestStore(t)
	fixture := &dbtest.TestDB{DB: st.DB(), T: t}
	fixture.SeedStandardDataSet()
	_, err := st.DB().Exec(`UPDATE participants SET email_address='bob@example.org' WHERE id=2;
 UPDATE participants SET phone_number='+12025550123' WHERE id=3;
 UPDATE messages SET sent_at='2024-01-01 00:00:00';
 UPDATE messages SET sender_id=1 WHERE id=1;
 UPDATE message_recipients SET display_name='Override' WHERE message_id=2 AND recipient_type='from';
 INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name) VALUES(2,2,'from','Bob');
 INSERT INTO sources(id,source_type,identifier) VALUES(2,'gmail','other@example.org');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES(2,2,'other','email_thread'),(3,1,'another','email_thread');
 INSERT INTO messages(id,source_id,conversation_id,source_message_id,message_type,sent_at,sender_id) VALUES
 (6,1,1,'phone-direct','sms','2024-01-01 00:00:00',3),
 (7,1,1,'phone-from','sms','2024-01-01 00:00:00',NULL),
 (8,2,2,'other-source','email','2024-01-01 00:00:00',1),
 (9,1,3,'other-conversation','email','2024-01-01 00:00:00',1);
 INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(7,3,'from')`)
	require.NoError(t, err)
	engine := query.NewSQLiteEngine(st.DB())
	for _, tc := range []struct {
		name   string
		filter query.MessageFilter
		want   []int64
	}{
		{"multi-author and duplicate", query.MessageFilter{Sender: "alice@example.com"}, []int64{9, 8, 3, 2, 1}},
		{"same author", query.MessageFilter{Sender: "alice@example.com", SenderName: "Bob"}, []int64{}},
		{"override", query.MessageFilter{Sender: "alice@example.com", SenderName: "Override"}, []int64{2}},
		{"phone", query.MessageFilter{Sender: "+12025550123", SenderName: "Carol White"}, []int64{7, 6}},
		{"source", query.MessageFilter{Sender: "alice@example.com", SourceID: new(int64(1))}, []int64{9, 3, 2, 1}},
		{"conversation", query.MessageFilter{Sender: "alice@example.com", ConversationID: new(int64(1))}, []int64{3, 2, 1}},
		{"label", query.MessageFilter{Sender: "alice@example.com", Label: "Work"}, []int64{1}},
		{"name only", query.MessageFilter{SenderName: "Override"}, []int64{2}},
		{"absent", query.MessageFilter{Sender: "absent@example.org"}, []int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			filter := tc.filter
			filter.Sorting = query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortDesc}
			messages, err := engine.ListMessages(t.Context(), filter)
			require.NoError(err)
			ids := make([]int64, len(messages))
			for i, m := range messages {
				ids[i] = m.ID
			}
			assert.Equal(tc.want, ids)
			for _, field := range []query.MessageSortField{query.MessageSortByDate, query.MessageSortBySubject, query.MessageSortBySize} {
				for _, direction := range []query.SortDirection{query.SortAsc, query.SortDesc} {
					for _, offset := range []int{0, 1, 20} {
						filter.Sorting = query.MessageSorting{Field: field, Direction: direction}
						filter.Pagination = query.Pagination{Limit: 2, Offset: offset}
						want, err := engine.ListMessagesWithSenderBound(t.Context(), filter, 0)
						require.NoError(err)
						for _, bound := range []int{2, 5000} {
							got, err := engine.ListMessagesWithSenderBound(t.Context(), filter, bound)
							require.NoError(err)
							assert.Equal(want, got, "field=%v direction=%v offset=%d bound=%d", field, direction, offset, bound)
						}
					}
				}
			}
		})
	}
}
