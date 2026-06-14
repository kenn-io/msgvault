//go:build sqlite_vec && pgvector

package pgvector

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// parityDoc is one message in the shared cross-backend fixture. The body
// holds the searchable tokens; we deliberately avoid email-address
// tokens (the documented tokenizer divergence between FTS5 and
// to_tsvector('simple')) so the ordering assertion is clean. axis is the
// unit-vector axis the message's embedding points along, giving each
// backend an identical, well-separated ANN signal.
type parityDoc struct {
	id      int64
	subject string
	body    string
	axis    int
}

// parityCorpus is the fixture both backends index. Vectors are unit
// vectors on distinct axes so ANN distances are well separated and
// identical across backends; bodies use plain alphabetic tokens so the
// two tokenizers agree. The four-token shared word "report" lets an
// FTS-heavy query touch every doc, while axis-specific tokens let a
// query select a single doc.
var parityCorpus = []parityDoc{
	{1, "alpha summary", "report about alpha widgets", 0},
	{2, "bravo summary", "report concerning bravo gadgets", 1},
	{3, "charlie summary", "report on charlie sprockets", 2},
	{4, "delta summary", "report covering delta cogs", 3},
}

const parityDim = 4

// buildSqlitevecParity stands up an in-memory/temp sqlitevec FusedSearch
// backend seeded with the shared corpus, mirroring sqlitevec's
// fused_test.go fixture shape. Returns the backend, ctx, and the active
// generation.
func buildSqlitevecParity(t *testing.T) (*sqlitevec.Backend, context.Context, vector.GenerationID) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	require.NoError(t, sqlitevec.RegisterExtension(), "RegisterExtension")
	main, err := sql.Open(sqlitevec.DriverName(), mainPath)
	require.NoError(t, err, "open main")
	t.Cleanup(func() { _ = main.Close() })

	schema := `
CREATE TABLE messages (
    id INTEGER PRIMARY KEY,
    subject TEXT,
    source_id INTEGER,
    sender_id INTEGER,
    has_attachments INTEGER DEFAULT 0,
    size_estimate INTEGER,
    sent_at DATETIME,
    deleted_at DATETIME,
    deleted_from_source_at DATETIME
);
CREATE VIRTUAL TABLE messages_fts USING fts5(subject, body, content='', contentless_delete=1);
CREATE TABLE message_labels (
    message_id INTEGER NOT NULL,
    label_id INTEGER NOT NULL,
    PRIMARY KEY (message_id, label_id)
);
CREATE TABLE message_recipients (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL,
    recipient_type TEXT NOT NULL,
    participant_id INTEGER NOT NULL
);`
	_, err = main.Exec(schema)
	require.NoError(t, err, "schema")

	for _, d := range parityCorpus {
		_, err := main.Exec(`INSERT INTO messages (id, subject) VALUES (?, ?)`, d.id, d.subject)
		require.NoErrorf(t, err, "insert msg %d", d.id)
		_, err = main.Exec(
			`INSERT INTO messages_fts (rowid, subject, body) VALUES (?, ?, ?)`,
			d.id, d.subject, d.body)
		require.NoErrorf(t, err, "insert fts %d", d.id)
	}

	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      filepath.Join(dir, "vectors.db"),
		MainPath:  mainPath,
		Dimension: parityDim,
		MainDB:    main,
	})
	require.NoError(t, err, "sqlitevec.Open")
	t.Cleanup(func() { _ = b.Close() })

	gid, err := b.CreateGeneration(ctx, "m", parityDim, "")
	require.NoError(t, err, "CreateGeneration")
	chunks := make([]vector.Chunk, 0, len(parityCorpus))
	for _, d := range parityCorpus {
		chunks = append(chunks, vector.Chunk{MessageID: d.id, Vector: unitVec(parityDim, d.axis)})
	}
	require.NoError(t, b.Upsert(ctx, gid, chunks), "Upsert")
	require.NoError(t, b.ActivateGeneration(ctx, gid, true), "Activate")
	return b, ctx, gid
}

// buildPgvectorParity stands up a live pgvector FusedSearch backend
// seeded with the SAME shared corpus, reusing the pgvector fused
// fixture helpers.
func buildPgvectorParity(t *testing.T) (*fusedFixture, vector.GenerationID) {
	t.Helper()
	f := newFusedFixture(t)
	base := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	for _, d := range parityCorpus {
		f.seedMsg(t, d.id, d.subject, d.body, 10, base, false)
	}
	vecs := make(map[int64][]float32, len(parityCorpus))
	for _, d := range parityCorpus {
		vecs[d.id] = unitVec(parityDim, d.axis)
	}
	f.embedAll(t, vecs)
	return f, f.gen
}

func idSeq(hits []vector.FusedHit) []int64 {
	out := make([]int64, len(hits))
	for i, h := range hits {
		out[i] = h.MessageID
	}
	return out
}

// TestParity_HybridOrderingMatchesSqlitevec asserts the sqlitevec and
// pgvector FusedSearch backends return the SAME result ordering
// (sequence of message IDs) for a handful of hybrid queries built on an
// identical fixture. The fixture avoids email-address tokens so the
// documented FTS5-vs-tsquery tokenizer divergence does not affect the
// assertion.
//
// The queries are chosen so the final RRF ordering is unambiguous on
// both backends: ANN-heavy queries lean on the well-separated unit-vector
// distances, FTS-heavy queries select a single doc, and the mixed query
// pairs an ANN signal with a single-doc FTS hit. None of them depend on
// the relative ordering of multiple same-query FTS matches (where BM25
// and ts_rank_cd legitimately differ).
func TestParity_HybridOrderingMatchesSqlitevec(t *testing.T) {
	// Build the live pgvector side first; if MSGVAULT_TEST_DB is absent
	// newFusedFixture skips, keeping the sqlitevec-only build green.
	pf, pgGen := buildPgvectorParity(t)
	sb, sctx, sGen := buildSqlitevecParity(t)

	cases := []struct {
		name string
		req  func(gen vector.GenerationID) vector.FusedRequest
	}{
		{
			// ANN-heavy: query points along axis 0 → msg 1 closest, then
			// 2, 3, 4 by axis distance. No FTS signal, so the ordering is
			// purely the (identical) cosine distances.
			name: "ann_only_axis0",
			req: func(gen vector.GenerationID) vector.FusedRequest {
				return vector.FusedRequest{
					QueryVec:   unitVec(parityDim, 0),
					Generation: gen,
					KPerSignal: 10,
					Limit:      10,
					RRFK:       60,
				}
			},
		},
		{
			// ANN-heavy along a different axis: msg 3 closest.
			name: "ann_only_axis2",
			req: func(gen vector.GenerationID) vector.FusedRequest {
				return vector.FusedRequest{
					QueryVec:   unitVec(parityDim, 2),
					Generation: gen,
					KPerSignal: 10,
					Limit:      10,
					RRFK:       60,
				}
			},
		},
		{
			// FTS-heavy: a token unique to one doc selects exactly msg 2.
			// Single-doc match means tokenizer rank differences cannot
			// reorder anything.
			name: "fts_only_single_doc",
			req: func(gen vector.GenerationID) vector.FusedRequest {
				return vector.FusedRequest{
					FTSQuery:   "bravo",
					Generation: gen,
					KPerSignal: 10,
					Limit:      10,
					RRFK:       60,
				}
			},
		},
		{
			// Mixed: ANN along axis 0 (msg 1 closest) plus an FTS hit on a
			// token unique to msg 4. msg 1 wins ANN rank 1; msg 4 enters
			// only via FTS. The remaining ANN-only docs (2,3) trail. The
			// ordering is driven by deterministic, well-separated signals.
			name: "mixed_ann_axis0_fts_delta",
			req: func(gen vector.GenerationID) vector.FusedRequest {
				return vector.FusedRequest{
					QueryVec:   unitVec(parityDim, 0),
					FTSQuery:   "delta",
					Generation: gen,
					KPerSignal: 10,
					Limit:      10,
					RRFK:       60,
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sHits, _, err := sb.FusedSearch(sctx, c.req(sGen))
			require.NoError(t, err, "sqlitevec FusedSearch")
			pHits, _, err := pf.b.FusedSearch(pf.ctx, c.req(pgGen))
			require.NoError(t, err, "pgvector FusedSearch")

			sIDs := idSeq(sHits)
			pIDs := idSeq(pHits)
			require.NotEmpty(t, sIDs, "sqlitevec returned no hits")
			assert.Equalf(t, sIDs, pIDs,
				"ordering mismatch: sqlitevec=%v pgvector=%v", sIDs, pIDs)
		})
	}
}
