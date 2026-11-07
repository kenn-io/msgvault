package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteUnicodeLowerRegisteredOnEveryConnection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "unicode-lower.db")
	st, err := OpenForTest(t.Context(), dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	db := st.DB()
	db.SetMaxOpenConns(4)
	conns := make([]*sql.Conn, 0, 4)
	for range 4 {
		conn, err := db.Conn(t.Context())
		require.NoError(err, "acquire pooled connection")
		conns = append(conns, conn)
	}
	t.Cleanup(func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	for i, conn := range conns {
		var got string
		err := conn.QueryRowContext(
			t.Context(),
			"SELECT msgvault_unicode_lower('ÉCOLE')").Scan(&got)
		require.NoErrorf(err, "unicode lower on connection %d", i)
		assert.Equalf("école", got, "unicode lower on connection %d", i)
	}

	readOnly, err := OpenReadOnly(dbPath)
	require.NoError(err, "open read-only store")
	t.Cleanup(func() { _ = readOnly.Close() })
	var readOnlyGot string
	err = readOnly.DB().QueryRowContext(t.Context(), "SELECT msgvault_unicode_lower('ÉCOLE')").Scan(&readOnlyGot)
	require.NoError(err, "unicode lower on read-only connection")
	assert.Equal("école", readOnlyGot, "unicode lower on read-only connection")
}
