package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
)

func testOperationPassScope(key string) operations.PassScope {
	return operations.PassScope{
		Key: key, Trigger: operations.TriggerManual,
		StartedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	}
}

func visualOperationRuns(t *testing.T, st *store.Store) []operations.Run {
	t.Helper()
	snapshot, err := st.ListRuns(t.Context(), operations.Query{
		Kinds: []operations.Kind{operations.KindVisualEmbedding}, Limit: 100,
	})
	require.NoError(t, err)
	return snapshot.Runs
}
