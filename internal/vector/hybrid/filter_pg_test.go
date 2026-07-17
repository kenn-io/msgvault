//go:build pgvector

package hybrid

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

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

// openFilterPGTestDB connects to MSGVAULT_TEST_DB on a per-test schema
// and creates the participants + labels tables BuildFilter resolves
// against. Skips when MSGVAULT_TEST_DB is unset or not a postgres DSN.
func openFilterPGTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("hybrid BuildFilter pgvector tests require MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "hybrid_filter_test_" + hex.EncodeToString(buf)

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
		CREATE TABLE participants (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email_address TEXT
		);
		CREATE TABLE labels (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			name TEXT
		);`)
	require.NoError(t, err, "create participants/labels")

	_, err = db.Exec(`INSERT INTO participants (email_address) VALUES
		('alice@example.com'), ('bob@example.com'),
		('carol@other.com'), ('dave.work@example.com')`)
	require.NoError(t, err, "seed participants")
	_, err = db.Exec(`INSERT INTO labels (name) VALUES ('INBOX'), ('Work'), ('Archive')`)
	require.NoError(t, err, "seed labels")

	return db
}

// TestBuildFilter_PostgresRebindsLookups proves BuildFilter's
// participant/label resolution runs on pgx when threaded with the
// PostgreSQL dialect's Rebind. resolveParticipantIDs / resolveLabelIDs
// emit `?` placeholders, which pgx rejects (SQLSTATE 42601); the rebind
// rewrites them to $N. This guards the serve/MCP PG path too, since both
// route through Engine.BuildFilter -> the package-level BuildFilter.
func TestBuildFilter_PostgresRebindsLookups(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	db := openFilterPGTestDB(t)
	ctx := context.Background()
	rebind := (&store.PostgreSQLDialect{}).Rebind

	// passes-with: addresses and labels resolve via substring LIKE on pgx.
	q := search.Parse(`from:example.com to:alice label:work`)
	f, err := BuildFilter(ctx, db, rebind, q)
	require.NoError(
		err, "BuildFilter must succeed with the dialect rebind on pgx")

	require.Len(f.SenderGroups, 1, "one from: token -> one group")
	assert.Len(f.SenderGroups[0], 3, "from:example.com matches alice/bob/dave.work")
	require.Len(f.ToGroups, 1, "one to: token -> one group")
	assert.Len(f.ToGroups[0], 1, "to:alice matches exactly one participant")
	require.Len(f.LabelGroups, 1, "one label: token -> one group")
	assert.Len(f.LabelGroups[0], 1, "label:work matches Work case-insensitively")

	// fails-without: the bare ? placeholders error on pgx, proving the
	// rebind is load-bearing. pgx parses `LIKE ? ESCAPE '\'` as an
	// operator expression rather than a parameter, so the exact SQLSTATE
	// depends on the surrounding clause; what matters is that it is a
	// Postgres error (carries a SQLSTATE) the rebind eliminates.
	identity := func(s string) string { return s }
	_, err = BuildFilter(ctx, db, identity, search.Parse(`from:alice`))
	require.Error(err, "un-rebound ? placeholders must fail on pgx")
	assert.Contains(err.Error(), "SQLSTATE",
		"expected a pgx server error for bare ? placeholders; got %v", err)
}
