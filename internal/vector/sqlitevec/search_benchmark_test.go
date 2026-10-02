//go:build fts5 && sqlite_vec

package sqlitevec

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// BenchmarkVectorRetrieval isolates local retrieval from query embedding and
// HTTP projection. It uses 100,000 synthetic messages with one 64-dimensional
// vector each and no accelerator. Setup and the first request are untimed.
func BenchmarkVectorRetrieval(b *testing.B) {
	requirements := require.New(b)
	const messages, dimension = 100_000, 64
	root := b.TempDir()
	mainPath := filepath.Join(root, "main.db")
	st, err := store.OpenForTest(mainPath)
	requirements.NoError(err)
	b.Cleanup(func() { require.NoError(b, st.Close()) })
	requirements.NoError(st.InitSchema())
	_, err = st.DB().Exec(`INSERT INTO sources (id, source_type, identifier)
		VALUES (1, 'gmail', 'archive@example.test')`)
	requirements.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type)
		VALUES (1, 1, 'synthetic-thread', 'email')`)
	requirements.NoError(err)
	_, err = st.DB().Exec(fmt.Sprintf(`WITH RECURSIVE ids(id) AS (
		SELECT 1 UNION ALL SELECT id + 1 FROM ids WHERE id < %d
	) INSERT INTO messages (id, source_id, source_message_id, conversation_id, subject, message_type)
		SELECT id, 1, 'message-' || id, 1, 'Synthetic correspondence', 'email' FROM ids`, messages))
	requirements.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO messages_fts (rowid, subject, body)
		SELECT id, subject, 'project update ' || CASE WHEN id <= 20 THEN 'quartz' ELSE '' END FROM messages`)
	requirements.NoError(err)
	backend, err := Open(b.Context(), Options{
		Path: filepath.Join(root, "vectors.db"), MainPath: mainPath,
		MainDB: st.DB(), Dimension: dimension, AcceleratorMode: "exact",
	})
	requirements.NoError(err)
	b.Cleanup(func() { require.NoError(b, backend.Close()) })
	generation, err := backend.CreateGeneration(b.Context(), "synthetic", dimension, "")
	requirements.NoError(err)
	for start := 0; start < messages; start += 1000 {
		chunks := make([]vector.Chunk, 1000)
		for i := range chunks {
			values := make([]float32, dimension)
			values[0], values[1] = 1, float32(start+i)/messages
			chunks[i] = vector.Chunk{MessageID: int64(start + i + 1), Vector: values}
		}
		requirements.NoError(backend.Upsert(b.Context(), generation, chunks))
	}
	query := unitVec(dimension, 0)
	for _, name := range []string{"Semantic", "HybridRare", "HybridCommon"} {
		b.Run(name, func(b *testing.B) {
			run := func() {
				if name == "Semantic" {
					hits, meta, err := backend.SearchWithMetadata(b.Context(), generation, query, 500, vector.Filter{})
					require.NoError(b, err)
					require.Len(b, hits, 500)
					require.Equal(b, "exact", meta.Accelerator)
					return
				}
				term := "quartz"
				if name == "HybridCommon" {
					term = "project"
				}
				hits, meta, err := backend.FusedSearch(b.Context(), vector.FusedRequest{
					Generation: generation, QueryVec: query, FTSTerms: []string{term},
					KPerSignal: 100, Limit: 50, RRFK: 60,
				})
				require.NoError(b, err)
				require.Len(b, hits, 50)
				require.True(b, meta.PoolSaturated)
			}
			run()
			b.ReportAllocs()
			for b.Loop() {
				run()
			}
		})
	}
}
