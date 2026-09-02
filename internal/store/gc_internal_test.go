package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGCRefusesPostgreSQLBeforeDatabaseAccess(t *testing.T) {
	st := &Store{dialect: &PostgreSQLDialect{}}

	_, err := st.ExecuteGCContext(t.Context(), GCPlan{})
	require.ErrorIs(t, err, ErrGCUnsupported)
	require.ErrorIs(t, st.VacuumContext(t.Context()), ErrGCUnsupported)
}
