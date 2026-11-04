//go:build sqlite_vec && pgvector

package whoknows

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

func init() {
	openPostgresFixtureBackend = func(t *testing.T, st *store.Store) vector.Backend {
		t.Helper()
		b, err := pgvector.Open(t.Context(), pgvector.Options{DB: st.DB(), Dimension: embedDimension})
		require.NoError(t, err)
		return b
	}
}
