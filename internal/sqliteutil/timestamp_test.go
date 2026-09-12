package sqliteutil

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimestampKeySQLPreservesExactInstants(t *testing.T) {
	requirements := require.New(t)
	db, err := sql.Open(DriverName(), ":memory:")
	requirements.NoError(err)
	defer func() { _ = db.Close() }()
	for _, tc := range []struct {
		name           string
		earlier, later any
		equal          bool
	}{
		{"nanoseconds", "2026-01-01T00:00:00.000000100Z", "2026-01-01T00:00:00.000000200Z", false},
		{"offset equivalence", "2026-01-01 00:00:00.000000100+00:00", "2026-01-01T02:00:00.000000100+02:00", true},
		{"before Unix epoch", "1600-01-01", "1700-01-01", false},
		{"after UnixNano range", "2300-01-01T00:00:00Z", "9999-12-31T23:59:59Z", false},
		{"date and minute", "2026-01-01", "2026-01-01T00:00", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			var less, equal bool
			err := db.QueryRow(`SELECT msgvault_timestamp_key(?) < msgvault_timestamp_key(?), msgvault_timestamp_key(?) = msgvault_timestamp_key(?)`, tc.earlier, tc.later, tc.earlier, tc.later).Scan(&less, &equal)
			requirements.NoError(err)
			assert.Equal(t, !tc.equal, less)
			assert.Equal(t, tc.equal, equal)
		})
	}
	for _, value := range []any{nil, "invalid", "0001-01-01T00:00:00Z"} {
		var key any
		requirements.NoError(db.QueryRow(`SELECT msgvault_timestamp_key(?)`, value).Scan(&key))
		assert.Nil(t, key)
	}
}
