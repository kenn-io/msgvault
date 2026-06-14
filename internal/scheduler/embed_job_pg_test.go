//go:build pgvector

package scheduler

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// openPGPendingDB stands up an isolated per-test schema on
// MSGVAULT_TEST_DB containing just the pending_embeddings table the
// activation gate's pendingCount queries. Returns a pgx *sql.DB scoped to
// the schema; the schema is dropped on cleanup. Skips when
// MSGVAULT_TEST_DB is unset or not a postgres DSN.
func openPGPendingDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("pgvector scheduler tests require MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "sched_pending_test_" + hex.EncodeToString(buf)

	setup, err := sql.Open("pgx", url)
	require.NoError(t, err, "open setup")
	defer func() { _ = setup.Close() }()
	_, err = setup.Exec("CREATE SCHEMA " + schemaName)
	require.NoError(t, err, "create schema")

	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	testURL := url + sep + "search_path=" + schemaName + ",public"

	db, err := sql.Open("pgx", testURL)
	require.NoError(t, err, "open")
	t.Cleanup(func() {
		_ = db.Close()
		cleanup, err := sql.Open("pgx", url)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		_, _ = cleanup.Exec("DROP SCHEMA " + schemaName + " CASCADE")
	})

	_, err = db.Exec(`
		CREATE TABLE pending_embeddings (
			generation_id BIGINT NOT NULL,
			message_id    BIGINT NOT NULL,
			PRIMARY KEY (generation_id, message_id)
		)`)
	require.NoError(t, err, "create pending_embeddings")
	return db
}

// TestEmbedJobPG_ActivatesBuildingWhenDrained drives the activation gate
// against live PG: with the pgvector VectorsDB handle, pendingCount must
// rebind its ? placeholder to $N so pgx accepts the count. The building
// generation has zero pending rows, so the gate passes and the daemon
// activates it.
//
// Before EmbedJob.Rebind was wired, pendingCount issued a bare `?` against
// the pgx handle and failed with SQLSTATE 42601 ("syntax error"), which
// the activation path swallows as a warn — the building generation would
// never auto-activate on PG. This test fails-without / passes-with the fix.
func TestEmbedJobPG_ActivatesBuildingWhenDrained(t *testing.T) {
	db := openPGPendingDB(t)
	building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  building,
	}
	runner := &fakeRunner{}
	job := &EmbedJob{
		Worker:      runner,
		Backend:     backend,
		VectorsDB:   db,
		Rebind:      (&store.PostgreSQLDialect{}).Rebind,
		Fingerprint: "m:768",
	}

	job.Run(context.Background())

	assert.Equal(t, []vector.GenerationID{77}, backend.activations(),
		"building generation must auto-activate when its PG pending queue is drained")
}

// TestEmbedJobPG_DoesNotActivateWhilePending is the inverse: with a
// pending row present, pendingCount (rebound to $N) returns > 0 and the
// gate must NOT activate. This also exercises the rebound query on pgx —
// a non-rebinding pendingCount errors out before it can read the count.
func TestEmbedJobPG_DoesNotActivateWhilePending(t *testing.T) {
	db := openPGPendingDB(t)
	_, err := db.Exec(`INSERT INTO pending_embeddings (generation_id, message_id) VALUES (77, 1)`)
	require.NoError(t, err, "seed pending")

	building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  building,
	}
	runner := &fakeRunner{}
	job := &EmbedJob{
		Worker:      runner,
		Backend:     backend,
		VectorsDB:   db,
		Rebind:      (&store.PostgreSQLDialect{}).Rebind,
		Fingerprint: "m:768",
	}

	job.Run(context.Background())

	assert.Empty(t, backend.activations(),
		"building generation must not activate while PG pending rows remain")
}
