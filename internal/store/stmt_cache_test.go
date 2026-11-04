package store_test

import (
	"path/filepath"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSQLiteReusesCompiledStatements(t *testing.T) {
	testutil.SkipIfPostgres(t, "observes SQLite statement compilation")

	for _, tt := range []struct {
		name string
		open func(string) (*store.Store, error)
	}{
		{"Open", store.Open},
		{"OpenForTest", store.OpenForTest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			st, err := tt.open(filepath.Join(t.TempDir(), "test.db"))
			require.NoError(err)
			t.Cleanup(func() { require.NoError(st.Close()) })

			conn, err := st.DB().Conn(t.Context())
			require.NoError(err)
			t.Cleanup(func() { require.NoError(conn.Close()) })
			_, err = conn.ExecContext(t.Context(), "CREATE TABLE probe(v INTEGER)")
			require.NoError(err)

			compiles := 0
			require.NoError(conn.Raw(func(driverConn any) error {
				sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
				require.True(ok)
				sqliteConn.RegisterAuthorizer(func(action int, table, _, _ string) int {
					if action == sqlite3.SQLITE_INSERT && table == "probe" {
						compiles++
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			}))

			for i := range 3 {
				_, err = conn.ExecContext(t.Context(), "INSERT INTO probe(v) VALUES (?)", i)
				require.NoError(err)
			}
			assert.Equal(t, 1, compiles, "repeated parameterized writes reuse the compiled statement")
		})
	}
}
