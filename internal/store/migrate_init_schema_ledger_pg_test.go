package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitSchemaAddsVersionToLegacyLedgerPostgres(t *testing.T) {
	dbURL := skipUnlessPostgresInternal(t)
	assert := assert.New(t)
	require := require.New(t)
	st := newPGStoreInternal(t, dbURL)

	_, err := st.DB().Exec(`ALTER TABLE applied_migrations DROP COLUMN version`)
	require.NoError(err, "remove the version column from the legacy ledger")

	const name = "legacy_postgres_version_probe"
	const appliedAt = "2020-01-02 03:04:05+00"
	_, err = st.DB().Exec(
		`INSERT INTO applied_migrations (name, applied_at) VALUES ($1, $2)`,
		name, appliedAt)
	require.NoError(err, "seed legacy ledger row")

	require.NoError(st.InitSchema(), "upgrade the legacy ledger")
	var version int
	var gotAppliedAt time.Time
	require.NoError(st.DB().QueryRow(
		`SELECT version, applied_at FROM applied_migrations WHERE name = $1`, name).
		Scan(&version, &gotAppliedAt))
	assert.Equal(1, version, "legacy PostgreSQL rows must default to version 1")
	assert.Equal(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC), gotAppliedAt.UTC(),
		"legacy PostgreSQL timestamp must survive")
}
