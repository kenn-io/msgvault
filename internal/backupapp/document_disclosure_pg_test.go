package backupapp

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocumentDisclosureTableExistsUsesPostgresCatalog(t *testing.T) {
	require := require.New(t)
	databaseURL := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(databaseURL, "postgres://") && !strings.HasPrefix(databaseURL, "postgresql://") {
		t.Skip("PG-only: requires MSGVAULT_TEST_DB pointing at PostgreSQL")
	}
	database, err := sql.Open("pgx", databaseURL)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })
	tx, err := database.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })

	require.True(documentDisclosureUsesPostgres(t.Context(), tx))
	exists, err := documentDisclosureTableExists(
		t.Context(), tx, "msgvault_document_disclosure_missing_table", true,
	)
	require.NoError(err)
	assert.False(t, exists)
	require.NoError(tx.Rollback())
}
