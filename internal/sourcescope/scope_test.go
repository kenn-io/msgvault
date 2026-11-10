package sourcescope_test

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestIncomingSenderPredicate(t *testing.T) {
	requirementsForTest := require.New(t)
	db, err := sql.Open("sqlite3", ":memory:")
	requirementsForTest.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE messages (id INTEGER, source_id INTEGER);
		CREATE TABLE message_recipients (message_id INTEGER, recipient_type TEXT, email_address TEXT);
		INSERT INTO messages VALUES (1,1),(2,1),(3,1),(4,1),(5,1),(6,2),(7,1);
		INSERT INTO message_recipients VALUES (1,'from','CODES@example.test'),(2,'from','other@example.test'),
		(3,'from',NULL),(4,'from','codes@example.test'),(4,'from','other@example.test'),
		(6,'from','any@example.test'),(7,'from','first last@example.test')`)
	requirementsForTest.NoError(err)
	ctx := sourcescope.WithIncomingSenders(t.Context(), map[int64][]string{1: {"codes@example.test", "first last@example.test"}, 2: nil})
	predicate, args := sourcescope.IncomingPredicate(ctx, "m.id", "m.source_id", func(s string) string { return "lower(" + s + ")" })
	rows, err := db.QueryContext(ctx, "SELECT m.id FROM messages m WHERE "+predicate+" ORDER BY m.id", args...)
	requirementsForTest.NoError(err)
	defer func() { _ = rows.Close() }()
	var ids []int
	for rows.Next() {
		var id int
		requirementsForTest.NoError(rows.Scan(&id))
		ids = append(ids, id)
	}
	requirementsForTest.NoError(rows.Err())
	assert.Equal(t, []int{1, 6, 7}, ids)
}
