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
// backend a well-separated ANN signal with the *same neighbor ordering*.
// (The backends use different distance metrics — sqlitevec L2 vs pgvector
// cosine — so the raw distance values differ; because these vectors are
// unit-normalized the two metrics are monotonic transforms of each other
// and rank neighbors identically. See the docs-site search ranking page.)
type parityDoc struct {
	id      int64
	subject string
	body    string
	axis    int
}

// parityCorpus is the fixture both backends index. Vectors are unit
// vectors on distinct axes so ANN distances are well separated and yield
// the same neighbor *ordering* across backends (the metrics themselves
// differ — sqlitevec L2 vs pgvector cosine — but agree on ordering for
// unit-normalized vectors); bodies use plain alphabetic tokens so the
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
// backend seeded with the given corpus, mirroring sqlitevec's
// fused_test.go fixture shape. Returns the backend, ctx, and the active
// generation.
func buildSqlitevecParity(t *testing.T, corpus []parityDoc) (*sqlitevec.Backend, context.Context, vector.GenerationID) {
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

	for _, d := range corpus {
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
	chunks := make([]vector.Chunk, 0, len(corpus))
	for _, d := range corpus {
		chunks = append(chunks, vector.Chunk{MessageID: d.id, Vector: unitVec(parityDim, d.axis)})
	}
	require.NoError(t, b.Upsert(ctx, gid, chunks), "Upsert")
	require.NoError(t, b.ActivateGeneration(ctx, gid, true), "Activate")
	return b, ctx, gid
}

// buildPgvectorParity stands up a live pgvector FusedSearch backend
// seeded with the SAME corpus, reusing the pgvector fused fixture
// helpers.
func buildPgvectorParity(t *testing.T, corpus []parityDoc) (*fusedFixture, vector.GenerationID) {
	t.Helper()
	f := newFusedFixture(t)
	base := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	for _, d := range corpus {
		f.seedMsg(t, d.id, d.subject, d.body, 10, base, false)
	}
	vecs := make(map[int64][]float32, len(corpus))
	for _, d := range corpus {
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
	pf, pgGen := buildPgvectorParity(t, parityCorpus)
	sb, sctx, sGen := buildSqlitevecParity(t, parityCorpus)

	cases := []struct {
		name string
		req  func(gen vector.GenerationID) vector.FusedRequest
	}{
		{
			// ANN-heavy: query points along axis 0 → msg 1 closest, then
			// 2, 3, 4 by axis distance. No FTS signal, so the ordering is
			// purely the ANN distances. The two backends use different
			// metrics (sqlitevec L2 vs pgvector cosine) but agree on this
			// ordering because the fixture vectors are unit-normalized.
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
					FTSTerms:   []string{"bravo"},
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
					FTSTerms:   []string{"delta"},
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

// idSet collects the message ids of a hit list into a set, so two
// backends can be compared for membership equality regardless of order.
func idSet(hits []vector.FusedHit) map[int64]bool {
	out := make(map[int64]bool, len(hits))
	for _, h := range hits {
		out[h.MessageID] = true
	}
	return out
}

// ftsParityCorpus is a dedicated fixture for the FTS-term parity test.
// Bodies are chosen so the regression-prone query terms exercise three
// behaviors that used to diverge between FTS5 and PostgreSQL:
//   - a prefix-sensitive term ("invoic" must match "invoice"/"invoices")
//   - the literal word "or" (must be a lexeme, NOT a boolean operator)
//   - an ordinary stopword-like word riding along in the term set
//
// Only msg 1's body contains every term in {monthly, bill, or, invoice},
// so the AND-of-prefix FTS leg selects exactly it on both backends; the
// other docs round out the ANN pool on well-separated axes.
var ftsParityCorpus = []parityDoc{
	{1, "billing notice", "your monthly bill or invoice is ready", 0},
	{2, "shipping update", "the package shipped this morning", 1},
	{3, "newsletter", "weekly digest of company news", 2},
	{4, "invoices archive", "past invoices and receipts are attached", 3},
}

// TestParity_HybridFTSTermsMatchAcrossBackends is the cross-backend
// proof that the dialect-neutral FusedRequest.FTSTerms fix restores
// PG<->SQLite hybrid FTS parity. Before the fix, the hybrid engine
// hardcoded SQLite FTS5 syntax into the shared request and PG fed it to
// websearch_to_tsquery, which dropped the prefix `*` (PG exact-matched
// while SQLite prefix-matched) and reinterpreted "or" as a boolean
// operator. Now each backend renders FTSTerms via its own dialect's
// BuildFTSTerm, so both prefix-match the same lexeme set.
//
// We assert SET-equality (not exact order) of the hybrid top-K because
// BM25 and ts_rank_cd legitimately order multi-match FTS results
// differently; membership is the parity contract. A second case proves
// PG prefix matching now works ("invoic" matches "invoice"/"invoices").
func TestParity_HybridFTSTermsMatchAcrossBackends(t *testing.T) {
	pf, pgGen := buildPgvectorParity(t, ftsParityCorpus)
	sb, sctx, sGen := buildSqlitevecParity(t, ftsParityCorpus)

	cases := []struct {
		name string
		req  func(gen vector.GenerationID) vector.FusedRequest
	}{
		{
			// Prefix + "or" + stopword-laden term set, FTS-ONLY (no
			// QueryVec) so the assertion compares the FTS legs directly
			// across backends instead of letting the ANN leg return the
			// whole corpus and mask an FTS divergence. The AND-of-prefix
			// FTS leg selects msg 1 (the only doc with all four terms) on
			// both backends. Pre-fix, PG dropped msg 1 from the FTS leg
			// (no prefix match, "or" treated as a boolean operator), so
			// the id sets would differ.
			name: "fts_terms_prefix_or_stopword",
			req: func(gen vector.GenerationID) vector.FusedRequest {
				return vector.FusedRequest{
					FTSTerms:   []string{"monthly", "bill", "or", "invoice"},
					Generation: gen,
					KPerSignal: 10,
					Limit:      10,
					RRFK:       60,
				}
			},
		},
		{
			// Pure prefix proof: "invoic" must match both "invoice"
			// (msg 1) and "invoices" (msg 4) on BOTH backends. Pre-fix PG
			// exact-matched and found neither.
			name: "fts_prefix_invoic_matches_invoice_and_invoices",
			req: func(gen vector.GenerationID) vector.FusedRequest {
				return vector.FusedRequest{
					FTSTerms:   []string{"invoic"},
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
			require := require.New(t)
			assert := assert.New(t)
			sHits, _, err := sb.FusedSearch(sctx, c.req(sGen))
			require.NoError(err, "sqlitevec FusedSearch")
			pHits, _, err := pf.b.FusedSearch(pf.ctx, c.req(pgGen))
			require.NoError(err, "pgvector FusedSearch")

			sSet := idSet(sHits)
			pSet := idSet(pHits)
			require.NotEmpty(sSet, "sqlitevec returned no hits")
			assert.Equalf(sSet, pSet,
				"hybrid id-set mismatch: sqlitevec=%v pgvector=%v", idSeq(sHits), idSeq(pHits))
		})
	}

	// Explicit prefix membership: "invoic" must surface both the
	// "invoice" doc (1) and the "invoices" doc (4) on the PG backend,
	// which is exactly what regressed when the prefix `*` was dropped.
	pHits, _, err := pf.b.FusedSearch(pf.ctx, vector.FusedRequest{
		FTSTerms:   []string{"invoic"},
		Generation: pgGen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	require.NoError(t, err, "pgvector prefix FusedSearch")
	pSet := idSet(pHits)
	assert.Truef(t, pSet[1] && pSet[4],
		"pgvector prefix 'invoic' must match invoice(1) and invoices(4); got %v", idSeq(pHits))
}
