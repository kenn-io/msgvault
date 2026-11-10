//go:build cgo

package query_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestSenderListBoundedWork(t *testing.T) {
	dbtest.SkipIfPostgres(t, "SQLite row-visit instrumentation")
	t.Run("30000", func(t *testing.T) {
		setupRequire := require.New(t)
		path := filepath.Join(t.TempDir(), "archive.db")
		st := testutil.NewSQLiteTestStoreAt(t, path)
		fixture := &dbtest.TestDB{DB: st.DB(), T: t}
		fixture.SeedStandardDataSet()
		_, err := st.DB().ExecContext(t.Context(), `WITH RECURSIVE ids(id) AS (SELECT 100 UNION ALL SELECT id+1 FROM ids WHERE id<30100)
 INSERT INTO messages(id,source_id,conversation_id,source_message_id,sent_at,sender_id,message_type)
 SELECT id,1,1,'bulk-' || id,'2024-06-01 00:00:00',CASE WHEN id=100 THEN 3 WHEN id<=1100 THEN 2 ELSE 1 END,'email' FROM ids;
 INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES (100,3,'from');
 UPDATE participants SET email_address='old@example.org' WHERE id=2;
 INSERT INTO message_labels(message_id,label_id) VALUES (101,1);
 INSERT INTO message_recipients(message_id,participant_id,recipient_type)
 SELECT id,1,'from' FROM messages WHERE sender_id=1;
 INSERT INTO sources(id,source_type,identifier) VALUES (2,'gmail','small@example.com');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES (2,2,'small','email_thread');
 WITH RECURSIVE ids(id) AS (SELECT 40001 UNION ALL SELECT id+1 FROM ids WHERE id<40010)
 INSERT INTO messages(id,source_id,conversation_id,source_message_id,sent_at,sender_id,message_type)
 SELECT id,2,2,'small-' || id,'2020-01-01 00:00:00',CASE WHEN id=40001 THEN 2 ELSE 1 END,'email' FROM ids`)
		setupRequire.NoError(err)
		setupRequire.NoError(st.Close())
		st, err = store.Open(path) // Include the production open-time optimizer.
		setupRequire.NoError(err)
		defer func() { setupRequire.NoError(st.Close()) }()
		visits := countSenderMessageVisits(t, st.DB())
		for _, tc := range []struct {
			sender, label string
			want          int
		}{
			{"carol@example.com", "", 1}, {"old@example.org", "", 100}, {"absent@example.com", "", 0}, {"old@example.org", "INBOX", 3},
		} {
			t.Run(tc.sender+tc.label, func(t *testing.T) {
				require := require.New(t)
				*visits = 0
				messages, err := query.NewSQLiteEngine(st.DB()).ListMessages(t.Context(), query.MessageFilter{Sender: tc.sender, Label: tc.label,
					Sorting: query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortDesc}, Pagination: query.Pagination{Limit: 100}})
				require.NoError(err)
				assert.Len(t, messages, tc.want)
				// The probe, membership query, and result query each visit sender candidates.
				assert.Less(t, *visits, 4000, "sender work must stay bounded as unrelated messages grow")
			})
		}
	})
}

func TestSenderListLiveCandidateWork(t *testing.T) {
	dbtest.SkipIfPostgres(t, "SQLite row-visit instrumentation")
	for _, tc := range []struct {
		name, hidden string
		count, want  int
		firstID      int64
		hideSource   bool
	}{
		{"deduplicated messages", "deleted_at", 4900, 1, 100, false},
		{"source-deleted messages", "deleted_from_source_at", 4900, 1, 100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "archive.db")
			st := testutil.NewSQLiteTestStoreAt(t, path)
			fixture := &dbtest.TestDB{DB: st.DB(), T: t}
			fixture.SeedStandardDataSet()
			_, err := st.DB().ExecContext(t.Context(), fmt.Sprintf(`INSERT INTO participants(id,email_address) VALUES(4,'bounded@example.org');
 WITH RECURSIVE ids(id) AS (SELECT 100 UNION ALL SELECT id+1 FROM ids WHERE id<300099)
 INSERT INTO messages(id,source_id,conversation_id,source_message_id,sent_at,sender_id,message_type)
 SELECT id,1,1,'bulk-' || id,'2024-06-01 00:00:00',CASE WHEN id<%d THEN 4 ELSE 1 END,'email' FROM ids;
 INSERT INTO message_recipients(message_id,participant_id,recipient_type)
 SELECT id,4,'from' FROM messages WHERE sender_id=4 AND ?`, 100+tc.count), tc.hidden == "")
			require.NoError(err)
			if tc.hidden != "" {
				_, err = st.DB().ExecContext(t.Context(), "UPDATE messages SET "+tc.hidden+"='2024-06-02 00:00:00' WHERE id>100 AND sender_id=4")
				require.NoError(err)
			}
			require.NoError(st.Close())
			st, err = store.Open(path)
			require.NoError(err)
			defer func() { require.NoError(st.Close()) }()
			visits := countSenderMessageVisits(t, st.DB())
			filter := query.MessageFilter{
				Sender: "bounded@example.org", HideDeletedFromSource: tc.hideSource,
				Sorting: query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortDesc}, Pagination: query.Pagination{Limit: 100},
			}
			engine := query.NewSQLiteEngine(st.DB())
			messages, err := engine.ListMessages(t.Context(), filter)
			require.NoError(err)
			require.Len(messages, tc.want)
			assert.Equal(t, tc.firstID, messages[0].ID)
			assert.Less(t, *visits, 20000, "overlap and hidden messages must retain bounded sender work")
			if tc.hideSource {
				filter.HideDeletedFromSource = false
				messages, err = engine.ListMessages(t.Context(), filter)
				require.NoError(err)
				require.Len(messages, 100)
				assert.Equal(t, int64(99+tc.count), messages[0].ID)
			}
		})
	}
}

func TestSenderListDistinctMessageWork(t *testing.T) {
	dbtest.SkipIfPostgres(t, "SQLite row-visit instrumentation")
	for _, tc := range []struct {
		name           string
		count, aliases int
	}{
		{"4900 dual representations", 4900, 1},
		{"3000 merged From aliases", 3000, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			path := filepath.Join(t.TempDir(), "archive.db")
			st := testutil.NewSQLiteTestStoreAt(t, path)
			fixture := &dbtest.TestDB{DB: st.DB(), T: t}
			fixture.SeedStandardDataSet()
			_, err := st.DB().ExecContext(t.Context(), `INSERT INTO participants(id,email_address) VALUES(4,'bounded@example.org');
WITH RECURSIVE ids(id) AS (SELECT 100 UNION ALL SELECT id+1 FROM ids WHERE id<300099)
INSERT INTO messages(id,source_id,conversation_id,source_message_id,sent_at,sender_id,message_type)
SELECT id,1,1,'bulk-' || id,datetime('2024-06-01','+' || id || ' seconds'),CASE WHEN id<? THEN 4 ELSE 1 END,'email' FROM ids`, 100+tc.count)
			require.NoError(err)
			for alias := range tc.aliases {
				// Merges preserve each envelope address on the surviving participant.
				_, err = st.DB().ExecContext(t.Context(), `INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address)
SELECT id,4,'from',? FROM messages WHERE sender_id=4`, fmt.Sprintf("alias-%d@example.org", alias))
				require.NoError(err)
			}
			require.NoError(st.Close())
			st, err = store.Open(path)
			require.NoError(err)
			defer func() { require.NoError(st.Close()) }()
			visits := countSenderMessageVisits(t, st.DB())
			messages, err := query.NewSQLiteEngine(st.DB()).ListMessages(t.Context(), query.MessageFilter{
				Sender:     "alias-0@example.org",
				Sorting:    query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortDesc},
				Pagination: query.Pagination{Limit: 100},
			})
			require.NoError(err)
			require.Len(messages, 100)
			assert.Equal(int64(99+tc.count), messages[0].ID)
			assert.Less(*visits, 30000, "duplicate sender representations must avoid scanning unrelated messages")
		})
	}
}

func TestSenderListEquivalence(t *testing.T) {
	dbtest.SkipIfPostgres(t, "SQLite candidate-path comparison")
	st := testutil.NewSQLiteTestStore(t)
	fixture := &dbtest.TestDB{DB: st.DB(), T: t}
	fixture.SeedStandardDataSet()
	_, err := st.DB().ExecContext(t.Context(), `UPDATE participants SET email_address='bob@example.org' WHERE id=2;
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
	baseline := make(map[string][]query.MessageSummary)
	for _, extra := range []int{0, 10001} {
		if extra > 0 {
			_, err := st.DB().ExecContext(t.Context(), `WITH RECURSIVE ids(id) AS (SELECT 100 UNION ALL SELECT id+1 FROM ids WHERE id<10100)
INSERT INTO messages(id,source_id,conversation_id,source_message_id,message_type,sent_at,sender_id,deleted_at)
SELECT id,1,1,'hidden-' || id,'email','2024-01-01 00:00:00',1,'2024-06-01 00:00:00' FROM ids;
INSERT INTO message_recipients(message_id,participant_id,recipient_type)
SELECT id,1,'from' FROM messages WHERE id>=100`)
			require.NoError(t, err)
		}
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
			{"absent", query.MessageFilter{Sender: "absent@example.org"}, []int64{}},
		} {
			t.Run(fmt.Sprintf("%s/extra=%d", tc.name, extra), func(t *testing.T) {
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
							got, err := engine.ListMessages(t.Context(), filter)
							require.NoError(err)
							key := fmt.Sprintf("%s/%v/%v/%d", tc.name, field, direction, offset)
							if extra == 0 {
								baseline[key] = got
							} else {
								assert.Equal(baseline[key], got, key)
							}
						}
					}
				}
			})
		}
	}
}

func countSenderMessageVisits(t *testing.T, db *sql.DB) *int {
	t.Helper()
	require := require.New(t)
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(t.Context())
	require.NoError(err)
	visits := new(int)
	require.NoError(conn.Raw(func(raw any) error {
		c, ok := raw.(*sqlite3.SQLiteConn)
		require.True(ok)
		return c.RegisterFunc("record_message_visit", func(int64) bool { (*visits)++; return true }, false)
	}))
	// A flattened view keeps the real message indexes and counts rows evaluated
	// by the production query, including its unsuccessful date walk.
	_, err = conn.ExecContext(t.Context(), `CREATE TEMP VIEW messages AS SELECT * FROM main.messages WHERE record_message_visit(id)`)
	require.NoError(err)
	require.NoError(conn.Close())
	return visits
}

func TestSenderListProbeWork(t *testing.T) {
	dbtest.SkipIfPostgres(t, "SQLite row-visit instrumentation")
	t.Run("hidden-prefix=true", func(t *testing.T) {
		require := require.New(t)
		path := filepath.Join(t.TempDir(), "archive.db")
		st := testutil.NewSQLiteTestStoreAt(t, path)
		fixture := &dbtest.TestDB{DB: st.DB(), T: t}
		fixture.SeedStandardDataSet()
		_, err := st.DB().ExecContext(t.Context(), `INSERT INTO participants(id,email_address) VALUES(4,'large@example.org');
WITH RECURSIVE ids(id) AS (SELECT 100 UNION ALL SELECT id+1 FROM ids WHERE id<200099)
INSERT INTO messages(id,source_id,conversation_id,source_message_id,sent_at,message_type,deleted_at)
SELECT id,1,1,'large-' || id,datetime('2024-06-01','+' || id || ' seconds'),'email',
 CASE WHEN id<100100 THEN '2024-06-02 00:00:00' END FROM ids;
INSERT INTO message_recipients(message_id,participant_id,recipient_type)
SELECT id,4,'from' FROM messages WHERE id>=100`)
		require.NoError(err)
		require.NoError(st.Close())
		st, err = store.Open(path)
		require.NoError(err)
		defer func() { require.NoError(st.Close()) }()
		visits := countSenderMessageVisits(t, st.DB())
		messages, err := query.NewSQLiteEngine(st.DB()).ListMessages(t.Context(), query.MessageFilter{
			Sender:     "large@example.org",
			Sorting:    query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortDesc},
			Pagination: query.Pagination{Limit: 100},
		})
		require.NoError(err)
		require.Len(messages, 100)
		assert.Equal(t, int64(200099), messages[0].ID)
		// A large sender must use the fallback. The candidate path walks
		// all 200,000 messages, including the hidden prefix.
		assert.Less(t, *visits, 101000, "large senders must use the fallback instead of the candidate path")
	})
}
