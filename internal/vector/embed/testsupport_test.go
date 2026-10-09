//go:build sqlite_vec

package embed

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// testMainSchema is the minimal main-DB schema the worker reads, including
// the last_modified column + the database-maintained triggers that bump it on
// any message change or body insert/update — mirroring production schema.sql
// so the CAS round-trip and trigger behavior are exercised in tests.
const testMainSchema = `
CREATE TABLE messages (
    id INTEGER PRIMARY KEY,
    subject TEXT,
    source_id INTEGER,
    message_type TEXT,
    deleted_at DATETIME,
    deleted_from_source_at DATETIME,
    embed_gen INTEGER,
    last_modified DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE message_bodies (
    message_id INTEGER PRIMARY KEY,
    body_text TEXT,
    body_html TEXT
);
CREATE TRIGGER trg_messages_last_modified
AFTER UPDATE ON messages FOR EACH ROW
WHEN OLD.last_modified = NEW.last_modified
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;
CREATE TRIGGER trg_message_bodies_last_modified_upd
AFTER UPDATE ON message_bodies FOR EACH ROW
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
END;
CREATE TRIGGER trg_message_bodies_last_modified_ins
AFTER INSERT ON message_bodies FOR EACH ROW
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
END;`

// countMissing returns how many live messages still need embedding for
// gen (embed_gen IS NULL OR embed_gen <> gen) in the test main DB.
func countMissing(t *testing.T, db *sql.DB, gen int64) int {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM messages
		  WHERE (embed_gen IS NULL OR embed_gen <> ?)
		    AND deleted_at IS NULL AND deleted_from_source_at IS NULL`, gen).Scan(&n)
	require.NoError(t, err, "countMissing")
	return n
}

// readWatermark returns the persisted watermark for gen (0 if absent).
func readWatermark(t *testing.T, db *sql.DB, gen int64) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(`SELECT watermark_id FROM embed_watermark WHERE generation_id = ?`, gen).Scan(&id)
	if err == sql.ErrNoRows {
		return 0
	}
	require.NoError(t, err, "readWatermark")
	return id
}

// workerFixture bundles everything needed for an end-to-end worker test.
type workerFixture struct {
	MainDB      *sql.DB
	VectorsDB   *sql.DB
	Store       *store.Store
	Backend     vector.Backend
	BuildingGen vector.GenerationID
	FakeClient  *fakeEmbeddingClient
	Recorder    *testOperationRecorder
}

// newWorkerFixture creates a main DB with n messages (subject="msg N",
// body="body N", embed_gen NULL), opens a real sqlitevec backend, creates
// a building generation, and installs a fakeEmbeddingClient that returns a
// deterministic vector per input.
func newWorkerFixture(t *testing.T, n int) *workerFixture {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	require.NoError(t, sqlitevec.RegisterExtension(), "RegisterExtension")
	st, err := store.OpenForTest(mainPath)
	require.NoError(t, err, "open main")
	t.Cleanup(func() { _ = st.Close() })
	mainDB := st.DB()

	_, err = mainDB.Exec(testMainSchema)
	require.NoError(t, err, "schema")
	for i := 1; i <= n; i++ {
		_, err := mainDB.Exec(
			`INSERT INTO messages (id, subject) VALUES (?, ?)`, i, fmt.Sprintf("msg %d", i))
		require.NoError(t, err, "insert message")
		_, err = mainDB.Exec(
			`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`, i, fmt.Sprintf("body %d", i))
		require.NoError(t, err, "insert body")
	}

	vecPath := filepath.Join(dir, "vectors.db")
	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      vecPath,
		MainPath:  mainPath,
		Dimension: 4,
		MainDB:    mainDB,
	})
	require.NoError(t, err, "sqlitevec.Open")
	t.Cleanup(func() { _ = b.Close() })

	gid, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(t, err, "CreateGeneration")

	// The worker needs a *sql.DB for its VectorsDB field. Open a second
	// handle to vectors.db (SQLite handles concurrent file opens).
	vecDB, err := sql.Open(sqlitevec.DriverName(), vecPath)
	require.NoError(t, err, "open vectors.db handle")
	t.Cleanup(func() { _ = vecDB.Close() })

	fc := &fakeEmbeddingClient{dim: 4}
	return &workerFixture{
		MainDB:      mainDB,
		VectorsDB:   vecDB,
		Store:       st,
		Backend:     b,
		BuildingGen: gid,
		FakeClient:  fc,
		Recorder:    newTestOperationRecorder(),
	}
}

// fakeEmbeddingClient returns a deterministic vector per input; tests
// may force failures with FailNext(n) or run a callback inside Embed
// (after the scan, before Upsert/stamp) to perturb DB state for race or
// failure testing.
type fakeEmbeddingClient struct {
	dim       int
	failN     int
	calls     int
	preReturn func() // optional callback fired right before Embed returns success
	// LastInputs captures the most recent batch of inputs passed to Embed,
	// letting tests assert what text the worker actually sent to the
	// embedder (e.g. body_text vs HTML-stripped body_html).
	LastInputs []string

	// OnEmbed, if non-nil, replaces the default Embed behavior with
	// a caller-provided closure. Used by tests that need to vary
	// returned errors per call (e.g. fail multi-msg batches with
	// ErrPermanent4xx, succeed on singletons).
	OnEmbed func(inputs []string) ([][]float32, error)
}

// FailNext forces the next n Embed calls to return an error.
func (c *fakeEmbeddingClient) FailNext(n int) { c.failN = n }

// Embed returns one deterministic, non-zero vector per input.
func (c *fakeEmbeddingClient) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	c.calls++
	if c.OnEmbed != nil {
		out, err := c.OnEmbed(inputs)
		if err == nil {
			c.LastInputs = append(c.LastInputs[:0], inputs...)
		}
		return out, err
	}
	if c.failN > 0 {
		c.failN--
		return nil, fmt.Errorf("simulated embed failure (call %d)", c.calls)
	}
	c.LastInputs = append(c.LastInputs[:0], inputs...)
	out := make([][]float32, len(inputs))
	for i := range inputs {
		v := make([]float32, c.dim)
		// First component encodes input length mod dim — deterministic, non-zero.
		v[0] = float32(len(inputs[i])%c.dim + 1)
		out[i] = v
	}
	if c.preReturn != nil {
		c.preReturn()
	}
	return out, nil
}
