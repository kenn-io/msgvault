package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
)

func TestEmbeddingDiagnosticsRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	now := time.Now().UTC()
	d := vector.EmbeddingDiagnostics{GenerationFingerprint: "synthetic:4", GenerationID: 7, RunID: 1, StartedAt: now, UpdatedAt: now,
		RecentBatches: make([]vector.EmbeddingBatch, 2)}
	require.NoError(st.SaveEmbeddingDiagnostics(t.Context(), d))
	got, err := st.ReadEmbeddingDiagnostics(t.Context(), 7)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(d, *got)
	missing, err := st.ReadEmbeddingDiagnostics(t.Context(), 99)
	require.NoError(err)
	assert.Nil(missing)
	for _, snapshot := range []string{`{"version":2,"diagnostics":{}}`, `{"generation_id":7}`} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind("UPDATE embedding_diagnostics SET snapshot = ? WHERE generation_id = ?"), snapshot, 7)
		require.NoError(err)
		got, err := st.ReadEmbeddingDiagnostics(t.Context(), 7)
		require.ErrorContains(err, "unsupported embedding diagnostics version")
		assert.Nil(got)
	}
}
