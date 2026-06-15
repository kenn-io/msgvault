package store_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver for the PG sub-test
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: true}
}

func currentBackend() string {
	db := os.Getenv("MSGVAULT_TEST_DB")
	if strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
		return "postgres"
	}
	return "sqlite"
}

// longPadding is ~3000 tokens of generic non-matching text used to inflate
// the subject-hit document's length without introducing the search term.
var longPadding = strings.Repeat("alpha beta gamma delta epsilon ", 600)

// TestFTSRank_KnownDivergence pins the documented cross-backend ranking
// divergence between SQLite and PostgreSQL for adversarial document shapes,
// and serves as a regression net against either backend's scoring model
// silently changing.
//
// This is EXPECTED BEHAVIOR, not a bug. See the docs-site page
// /architecture/search-ranking/ ("Where Ordering Can Diverge"):
//
//   - SQLite's bm25() applies Okapi BM25 document-length normalization.
//     A long subject-hit document is penalised so heavily that a short
//     body-hit document outranks it, even though subject is weighted 10x.
//   - PostgreSQL's ts_rank() (called without a normalization flag) does
//     not apply document-length normalization. Field priority via
//     setweight() ('A' for subject vs default 'D' for body) wins
//     unconditionally for single-field hits.
//
// SQLiteDialect.FTSSearchClause's bm25 weights are a best-effort
// approximation of PG's field priority for normal-length emails; they are
// NOT a strict cross-backend parity contract. If either backend's score
// model changes in a way that flips the ordering recorded below, this
// test fails and the divergence claim in the docs-site search ranking page
// must be re-evaluated.
//
// Corpus shape (per the adversarial review of PR #337):
//   - subject-hit: subject = "zappa", body padded with ~3000 tokens
//   - body-hit:    subject = "alpha", body = "zappa" (short)
//
// Expected order under MATCH 'zappa':
//   - SQLite: body-hit ranks first (bm25 length normalization)
//   - PostgreSQL: subject-hit ranks first (ts_rank, no normalization)
//
// SQLite is exercised through the production Store.SearchMessages code
// path (the same path the TUI and HTTP API hit). PostgreSQL is exercised
// against ts_rank() over a constructed tsvector directly so this test
// isolates the scorer behavior from store/query glue; full PostgreSQL
// store search coverage lives in the regular PostgreSQL test suite.
func TestFTSRank_KnownDivergence(t *testing.T) {
	switch currentBackend() {
	case "sqlite":
		assertSQLiteBodyHitWins(t)
	case "postgres":
		assertPostgresSubjectHitWins(t)
	default:
		require.Failf(t, "unknown backend", "%q", currentBackend())
	}
}

func assertSQLiteBodyHitWins(t *testing.T) {
	t.Helper()
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("gmail", "divergence@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "divergence-thread", "Divergence Fixture")
	require.NoError(t, err, "EnsureConversation")

	mk := func(label, subject, body string) int64 {
		id, err := st.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: label,
			MessageType:     "email",
			Subject:         nullString(subject),
			Snippet:         nullString(body),
			SizeEstimate:    int64(len(subject) + len(body)),
		})
		require.NoErrorf(t, err, "UpsertMessage(%q)", label)
		require.NoErrorf(t, st.UpsertFTS(id, subject, body, "noreply@example.com", "", ""), "UpsertFTS(%q)", label)
		return id
	}

	subjectHitID := mk("adv-subject-hit", "zappa", longPadding)
	bodyHitID := mk("adv-body-hit", "alpha", "zappa")

	// Filler docs so avgdl reflects a realistic corpus rather than two
	// extreme outliers.
	for i := range 10 {
		mk(
			"adv-filler-"+string(rune('a'+i)),
			"filler subject "+string(rune('a'+i)),
			"filler body content with several distinct words for normalization",
		)
	}

	results, total, err := st.SearchMessages("zappa", 0, 20)
	require.NoError(t, err, "SearchMessages")
	require.Falsef(t, total < 2 || len(results) < 2,
		"got total=%d len=%d, want >= 2 (both adversarial docs must match)", total, len(results))

	gotFirst, gotSecond := results[0].ID, results[1].ID
	wantFirst, wantSecond := bodyHitID, subjectHitID
	if gotFirst != wantFirst || gotSecond != wantSecond {
		assert.Failf(t,
			"sqlite: documented divergence not reproduced",
			"  got order:  [%d, %d, ...]\n"+
				"  want order: [%d (body-hit), %d (subject-hit), ...]\n"+
				"  subject-hit id=%d, body-hit id=%d\n"+
				"BM25 length normalization should make the short body-hit "+
				"outrank the long subject-hit. If this is no longer true, "+
				"update the docs-site search ranking page to match the new behavior.",
			gotFirst, gotSecond, wantFirst, wantSecond,
			subjectHitID, bodyHitID,
		)
	}
}

func assertPostgresSubjectHitWins(t *testing.T) {
	t.Helper()

	// Connect directly. The PG scoring divergence we want to pin is a
	// property of ts_rank() itself, not of the store glue, so a raw query
	// against constructed tsvector values keeps this assertion focused.
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err, "sql.Open(pgx)")
	defer func() { _ = db.Close() }()

	// Mirror SQLiteDialect.FTSUpsert / FTSSearchClause for PG:
	// setweight('A') on subject, default 'D' on body, plainto_tsquery
	// with the 'simple' configuration, ordered by ts_rank DESC.
	const q = `
SELECT label, ts_rank(v, plainto_tsquery('simple', 'zappa')) AS score FROM (
    SELECT 'subject-hit' AS label,
        setweight(to_tsvector('simple', 'zappa'), 'A') ||
        to_tsvector('simple', $1) ||
        setweight(to_tsvector('simple', 'noreply@example.com'), 'B') AS v
    UNION ALL
    SELECT 'body-hit',
        setweight(to_tsvector('simple', 'alpha'), 'A') ||
        to_tsvector('simple', 'zappa') ||
        setweight(to_tsvector('simple', 'noreply@example.com'), 'B')
) t
WHERE v @@ plainto_tsquery('simple', 'zappa')
ORDER BY score DESC`

	rows, err := db.Query(q, longPadding)
	require.NoError(t, err, "query ts_rank")
	defer func() { _ = rows.Close() }()

	var order []string
	var scores []float64
	for rows.Next() {
		var label string
		var score float64
		require.NoError(t, rows.Scan(&label, &score), "scan")
		order = append(order, label)
		scores = append(scores, score)
	}
	require.NoError(t, rows.Err(), "rows.Err")

	require.GreaterOrEqualf(t, len(order), 2, "got %d rows, want >= 2", len(order))
	if order[0] != "subject-hit" || order[1] != "body-hit" {
		assert.Failf(t,
			"postgres: documented divergence not reproduced",
			"  got order:  %v (scores %v)\n"+
				"  want order: [subject-hit, body-hit] (subject-hit > body-hit)\n"+
				"ts_rank() called without a normalization flag must not apply "+
				"document-length normalization, so setweight('A') on subject "+
				"should beat default 'D' on body. If this is no longer true, "+
				"update the docs-site search ranking page to match the new behavior.",
			order, scores,
		)
	}
}
