package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newFTSProbeDB(tb testing.TB, columnsizeZero bool) *sql.DB {
	tb.Helper()
	req := require.New(tb)
	db, err := sql.Open("sqlite3", filepath.Join(tb.TempDir(), "probe.db"))
	req.NoError(err)
	tb.Cleanup(func() { req.NoError(db.Close()) })
	_, err = db.Exec("CREATE TABLE messages (id INTEGER PRIMARY KEY)")
	req.NoError(err)
	options := ""
	if columnsizeZero {
		options = ", columnsize=0"
	}
	_, err = db.Exec("CREATE VIRTUAL TABLE messages_fts USING fts5(text" + options + ")")
	req.NoError(err)
	return db
}

func TestSQLiteFTSProbePresence(t *testing.T) {
	for _, zero := range []bool{false, true} {
		name := "docsize"
		if zero {
			name = "columnsize_zero"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			req := require.New(t)
			db := newFTSProbeDB(t, zero)
			d := &SQLiteDialect{}
			ctx := context.Background()
			assert.False(d.FTSNeedsBackfill(db))
			assert.False(d.FTSNeedsBackfillQuick(ctx, db))
			_, err := db.Exec("INSERT INTO messages VALUES (1), (2), (3)")
			req.NoError(err)
			assert.True(d.FTSNeedsBackfill(db))
			assert.True(d.FTSNeedsBackfillQuick(ctx, db))
			_, err = db.Exec("INSERT INTO messages_fts(rowid, text) VALUES (1, ''), (3, 'synthetic'), (4, 'extra')")
			req.NoError(err)
			assert.True(d.FTSNeedsBackfill(db), "equal counts must not hide the interior hole")
			assert.False(d.FTSNeedsBackfillQuick(ctx, db), "quick probe only checks the tail")
			_, err = db.Exec("INSERT INTO messages_fts(rowid, text) VALUES (2, '')")
			req.NoError(err)
			assert.False(d.FTSNeedsBackfill(db), "empty text still counts as indexed")
			assert.False(d.FTSNeedsBackfillQuick(ctx, db))
			_, err = db.Exec("DELETE FROM messages_fts WHERE rowid IN (3, 4)")
			req.NoError(err)
			assert.True(d.FTSNeedsBackfill(db))
			assert.True(d.FTSNeedsBackfillQuick(ctx, db))
			_, err = db.Exec("DELETE FROM messages WHERE id = 3")
			req.NoError(err)
			assert.False(d.FTSNeedsBackfill(db), "deleted messages need no index entry")
			_, err = db.Exec("DELETE FROM messages_fts WHERE rowid = 1")
			req.NoError(err)
			assert.True(d.FTSNeedsBackfill(db), "lowest ID hole must be found")
			assert.False(d.FTSNeedsBackfillQuick(ctx, db))
			_, err = db.Exec("INSERT INTO messages VALUES (4)")
			req.NoError(err)
			assert.True(d.FTSNeedsBackfillQuick(ctx, db), "uncancelled probe must find the tail")
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			assert.False(d.FTSNeedsBackfillQuick(cancelled, db))
		})
	}
}

func TestSQLiteFTSProbeAvoidsStoredContent(t *testing.T) {
	assert := assert.New(t)
	req := require.New(t)
	db := newFTSProbeDB(t, false)
	_, err := db.Exec("INSERT INTO messages VALUES (1), (2), (3)")
	req.NoError(err)
	_, err = db.Exec("INSERT INTO messages_fts(rowid, text) VALUES (1, 'synthetic')")
	req.NoError(err)
	// The row-ID metadata remains usable even if stored content cannot be read.
	_, err = db.Exec("DROP TABLE messages_fts_content")
	req.NoError(err)
	d := &SQLiteDialect{}
	assert.True(d.FTSNeedsBackfill(db))
	assert.True(d.FTSNeedsBackfillQuick(context.Background(), db))
}

func TestSQLiteFTSProbeErrors(t *testing.T) {
	for _, damage := range []string{"DROP TABLE messages_fts", "DROP TABLE messages_fts_docsize; CREATE TABLE messages_fts_docsize (wrong INTEGER)"} {
		t.Run(damage, func(t *testing.T) {
			assert := assert.New(t)
			req := require.New(t)
			db := newFTSProbeDB(t, false)
			_, err := db.Exec("INSERT INTO messages VALUES (1)")
			req.NoError(err)
			_, err = db.Exec(damage)
			req.NoError(err)
			d := &SQLiteDialect{}
			assert.False(d.FTSNeedsBackfill(db), "errors must not be treated as gaps")
			assert.False(d.FTSNeedsBackfillQuick(context.Background(), db))
		})
	}
}

func TestSQLiteFTSProbeQueryPlan(t *testing.T) {
	assert := assert.New(t)
	req := require.New(t)
	db := newFTSProbeDB(t, false)
	rows, err := db.Query("EXPLAIN QUERY PLAN " + sqliteFTSNeedsBackfillDocsizeSQL)
	req.NoError(err)
	defer func() { req.NoError(rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		req.NoError(rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	req.NoError(rows.Err())
	plan := strings.Join(details, "\n")
	assert.Contains(plan, "SEARCH d USING INTEGER PRIMARY KEY (rowid=?)")
	assert.NotContains(plan, "VIRTUAL TABLE")
}

func FuzzSQLiteFTSProbePresence(f *testing.F) {
	f.Add([]byte{}, false)
	f.Add([]byte{1, 1, 1}, false)
	f.Add([]byte{1, 0, 1}, false)
	f.Add([]byte{0, 1, 1}, true)
	f.Add([]byte{1, 1, 0}, true)
	f.Fuzz(func(t *testing.T, indexed []byte, zero bool) {
		assert := assert.New(t)
		req := require.New(t)
		db := newFTSProbeDB(t, zero)
		wantGap := false
		// Bound SQLite fixture work at materialization, retaining arbitrary fuzz inputs.
		if len(indexed) > 64 {
			indexed = indexed[:64]
		}
		for offset, present := range indexed {
			id := offset + 1
			_, err := db.Exec("INSERT INTO messages VALUES (?)", id)
			req.NoError(err)
			if present&1 == 0 {
				wantGap = true
				continue
			}
			_, err = db.Exec("INSERT INTO messages_fts(rowid, text) VALUES (?, '')", id)
			req.NoError(err)
		}
		assert.Equal(wantGap, (&SQLiteDialect{}).FTSNeedsBackfill(db))
	})
}

func BenchmarkSQLiteFTSProbe(b *testing.B) {
	req := require.New(b)
	db := newFTSProbeDB(b, false)
	tx, err := db.Begin()
	req.NoError(err)
	text := strings.Repeat("synthetic ", 409) + "sample"
	for id := 1; id <= 10000; id++ {
		_, err = tx.Exec("INSERT INTO messages VALUES (?)", id)
		req.NoError(err)
		_, err = tx.Exec("INSERT INTO messages_fts(rowid, text) VALUES (?, ?)", id, text)
		req.NoError(err)
	}
	req.NoError(tx.Commit())
	b.Run("production", func(b *testing.B) {
		req := require.New(b)
		for range b.N {
			req.False((&SQLiteDialect{}).FTSNeedsBackfill(db))
		}
	})
	b.Run("virtual", func(b *testing.B) {
		req := require.New(b)
		for range b.N {
			var gap bool
			err := db.QueryRow(sqliteFTSNeedsBackfillVirtualSQL).Scan(&gap)
			req.NoError(err)
			req.False(gap)
		}
	})
}
