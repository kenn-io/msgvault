package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInsertCacheRowsAcceptsBatchesInDuckDB(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	db, err := sql.Open("duckdb", "")
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(db.Close()) })
	_, err = db.Exec(`CREATE TABLE cache_person_contacts(participant_id BIGINT, address_kind VARCHAR, normalized_value VARCHAR)`)
	requirements.NoError(err)

	rows := make([][]any, 205)
	for i := range rows {
		rows[i] = []any{int64(i), "email", fmt.Sprintf("person-%d@example.test", i)}
	}
	requirements.NoError(insertCacheRows(context.Background(), db, "cache_person_contacts", rows))

	var count, first, last int64
	requirements.NoError(db.QueryRow(`SELECT count(*), min(participant_id), max(participant_id) FROM cache_person_contacts`).Scan(&count, &first, &last))
	assertions.Equal(int64(205), count)
	assertions.Zero(first)
	assertions.Equal(int64(204), last)

	results, err := db.Query(`
		SELECT participant_id, address_kind, normalized_value
		FROM cache_person_contacts
		WHERE participant_id IN (0, 99, 100, 199, 200, 204)
		ORDER BY participant_id
	`)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(results.Close()) })
	var got [][]any
	for results.Next() {
		var participantID int64
		var addressKind, normalizedValue string
		requirements.NoError(results.Scan(&participantID, &addressKind, &normalizedValue))
		got = append(got, []any{participantID, addressKind, normalizedValue})
	}
	requirements.NoError(results.Err())
	assertions.Equal([][]any{
		{int64(0), "email", "person-0@example.test"},
		{int64(99), "email", "person-99@example.test"},
		{int64(100), "email", "person-100@example.test"},
		{int64(199), "email", "person-199@example.test"},
		{int64(200), "email", "person-200@example.test"},
		{int64(204), "email", "person-204@example.test"},
	}, got)
}
