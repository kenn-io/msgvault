//go:build fts5 && sqlite_vec

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

// seedRankingDivergenceArchive builds a small real archive in which BM25
// relevance and reverse-chronological order disagree, and in which one
// matching message has been deleted from its source account.
//
//	m1  2020-01-01  subject hit, short body  — most relevant, oldest
//	m2  2024-01-01  body hit, long body      — least relevant, newest
//	m3  2022-01-01  subject hit              — deleted from source
//
// BM25 weights subject ten times body (see store.SQLiteDialect.FTSSearchClause)
// and normalizes by document length, so the ranking is m1, m3, m2 while the
// date ordering is exactly the reverse. Production FTS search returns the
// former, minus m3.
func seedRankingDivergenceArchive(t *testing.T) *store.Store {
	t.Helper()
	return seedRankingDivergenceArchiveIn(t, t.TempDir())
}

func seedRankingDivergenceArchiveIn(t *testing.T, dataDir string) *store.Store {
	t.Helper()
	dbPath := filepath.Join(dataDir, "msgvault.db")
	s, err := store.Open(dbPath)
	require.NoError(t, err, "open store")
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.InitSchema(), "init schema")

	// A body long enough that BM25's length normalization can tell the two
	// documents apart, with the query term buried once inside it.
	filler := strings.Repeat("quarterly figures and other unrelated correspondence text ", 60)

	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type) VALUES
	(1, 1, 'thread-1', 'email_thread'),
	(2, 1, 'thread-2', 'email_thread'),
	(3, 1, 'thread-3', 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, subject, sent_at, size_estimate, deleted_from_source_at) VALUES
	(1, 1, 1, '<m1@example.com>', 'email', 'Lease renewal terms', '2020-01-01T00:00:00Z', 100, NULL),
	(2, 2, 1, '<m2@example.com>', 'email', 'Weekly digest',       '2024-01-01T00:00:00Z', 100, NULL),
	(3, 3, 1, '<m3@example.com>', 'email', 'Lease renewal notice','2022-01-01T00:00:00Z', 100, '2023-06-01T00:00:00Z');
`)
	require.NoError(t, err, "seed messages")

	for _, b := range []struct {
		id   int64
		body string
	}{
		{1, "Signed and returned."},
		{2, filler + " renewal " + filler},
		{3, "Notice served."},
	} {
		_, err = s.DB().Exec(
			`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`, b.id, b.body)
		require.NoError(t, err, "seed body %d", b.id)
	}

	// Index through the production backfill rather than writing messages_fts
	// by hand, so the test scores the documents production would have built.
	indexed, err := s.BackfillFTS(nil)
	require.NoError(t, err, "backfill FTS")
	require.EqualValues(t, 3, indexed, "every message must be indexed")
	return s
}

func keysOfAPIMessages(msgs []store.APIMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.SourceMessageID)
	}
	return out
}

func keysOfSummaries(msgs []query.MessageSummary) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.SourceMessageID)
	}
	return out
}

// TestRankedFTS_MatchesProductionRelevanceRanking is the regression for the
// mode=fts path having been scored against the wrong search.
//
// query.Engine.Search — what this command used to call — has no relevance
// component at all: it filters, then orders by sent_at DESC. Scoring that as a
// *ranking* measures the archive's date distribution, and reports it as
// retrieval quality. It also diverges from production on which messages are
// eligible at all: it leaves source-deleted messages in the result set, which
// no production search returns by default.
//
// The eval path must instead be the search production runs — the
// BM25-ranked store path behind /api/v1/search?mode=fts, whose messages_fts
// index and subject weighting are the same ones the hybrid engine's BM25 leg
// fuses, so fts and hybrid scores are comparable to each other.
func TestRankedFTS_MatchesProductionRelevanceRanking(t *testing.T) {
	s := seedRankingDivergenceArchive(t)
	ctx := t.Context()
	const topic = "renewal"

	// The two searches genuinely disagree here — without that this test
	// would pass no matter which one the eval called.
	production, _, err := s.SearchMessagesQueryContext(ctx, search.Parse(topic), 0, 10)
	require.NoError(t, err, "production store search")
	productionKeys := keysOfAPIMessages(production)
	require.Equal(t, []string{"<m1@example.com>", "<m2@example.com>"}, productionKeys,
		"production ranks the subject hit first and never returns the source-deleted message")

	qeng := query.NewEngine(s.DB(), s.IsPostgreSQL())
	legacy, err := qeng.Search(ctx, search.Parse(topic), 10, 0)
	require.NoError(t, err, "query engine search")
	legacyKeys := keysOfSummaries(legacy)
	require.Equal(t,
		[]string{"<m2@example.com>", "<m3@example.com>", "<m1@example.com>"}, legacyKeys,
		"the chronological path inverts the ranking and includes the source-deleted message")

	ev, diag := newTestEvaluator(t, s, "message")
	ev.limit = 10

	ranked, err := ev.rankedFTS(evalTestQuery(t, topic))
	require.NoError(t, err)

	assert.Equal(t, productionKeys, ranked,
		"the eval's fts mode must rank exactly as production search does")
	assert.NotEqual(t, legacyKeys, ranked,
		"and must no longer reproduce the chronological ordering")
	assert.NotContains(t, ranked, "<m3@example.com>",
		"a message deleted from its source is not something production retrieval returns")
	assert.Empty(t, diag.notes(), "a clean archive produces a clean run")
}

// TestRankedFTS_HonoursProductionAddressFilterSemantics pins the second
// divergence roborev flagged. The store path resolves from: as a substring
// match against the participant address (so `from:landlord` finds
// landlord@example.com); query.Engine.Search requires an exact address or an
// @domain pattern, and would have scored a flat zero for the same topic.
func TestRankedFTS_HonoursProductionAddressFilterSemantics(t *testing.T) {
	s := seedRankingDivergenceArchive(t)
	ctx := t.Context()
	_, err := s.DB().Exec(`
INSERT INTO participants (id, email_address) VALUES (1, 'landlord@example.com');
INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (1, 1, 'from');
`)
	require.NoError(t, err, "seed sender")

	const topic = "from:landlord renewal"

	production, _, err := s.SearchMessagesQueryContext(ctx, search.Parse(topic), 0, 10)
	require.NoError(t, err, "production store search")
	require.Equal(t, []string{"<m1@example.com>"}, keysOfAPIMessages(production),
		"production matches the address by substring")

	qeng := query.NewEngine(s.DB(), s.IsPostgreSQL())
	legacy, err := qeng.Search(ctx, search.Parse(topic), 10, 0)
	require.NoError(t, err, "query engine search")
	require.Empty(t, legacy, "the exact-match path finds nothing for the same topic")

	ev, _ := newTestEvaluator(t, s, "message")
	ev.limit = 10

	ranked, err := ev.rankedFTS(evalTestQuery(t, topic))
	require.NoError(t, err)
	assert.Equal(t, []string{"<m1@example.com>"}, ranked,
		"the eval must score the hits production returns, not zero")
}

// writeEvalFile writes one of the run's input files and returns its path.
func writeEvalFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600), "write %s", name)
	return path
}

// TestRunEval_EndToEnd_RanksByRelevanceAndSkipsAMalformedTopic drives the whole
// command against a real archive, so the two fixes are pinned where they
// actually have to hold: at the call sites, not just in the helpers.
//
//   - q1 is scored through the production relevance-ranked FTS path. Its one
//     judged message is the subject hit, which BM25 puts at rank 1 (MRR 1.0)
//     and the old chronological path put at rank 3 (MRR 0.333).
//   - q2 carries a malformed date filter. search.Parse drops it and leaves the
//     broader query `renewal` behind, which would have scored exactly like q1
//     under a question nobody asked. It must be skipped and reported instead.
func TestRunEval_EndToEnd_RanksByRelevanceAndSkipsAMalformedTopic(t *testing.T) {
	dir := t.TempDir()
	seedRankingDivergenceArchiveIn(t, dir)

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = config.NewDefaultConfig()
	cfg.Data.DataDir = dir

	savedFlags := [...]any{evalQrels, evalTopics, evalModes, evalDocKey, evalLimit, evalJSON}
	t.Cleanup(func() {
		evalQrels, _ = savedFlags[0].(string)
		evalTopics, _ = savedFlags[1].(string)
		evalModes, _ = savedFlags[2].(string)
		evalDocKey, _ = savedFlags[3].(string)
		evalLimit, _ = savedFlags[4].(int)
		evalJSON, _ = savedFlags[5].(bool)
	})
	evalQrels = writeEvalFile(t, dir, "qrels.txt",
		"q1 0 <m1@example.com> 1\nq2 0 <m1@example.com> 1\n")
	evalTopics = writeEvalFile(t, dir, "topics.tsv",
		"q1\trenewal\nq2\tbefore:invalid renewal\n")
	evalModes = "fts"
	evalDocKey = "message"
	evalLimit = 10
	evalJSON = true

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())

	done := captureStdout(t)
	err := runEval(cmd, nil)
	out := done()
	require.NoError(t, err, "eval run")

	var report struct {
		TopicsEvaluated int `json:"topics_evaluated"`
		Results         map[string]struct {
			MRR    float64 `json:"MRR"`
			Topics int     `json:"topics"`
		} `json:"results"`
		Diagnostics struct {
			SkippedCells []string `json:"skipped_cells"`
		} `json:"diagnostics"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report), "parse report: %s", out)

	assert.Equal(t, 1, report.TopicsEvaluated, "only the well-formed topic is scored")
	assert.Equal(t, 1, report.Results["fts"].Topics)
	assert.InDelta(t, 1.0, report.Results["fts"].MRR, 1e-9,
		"the relevance-ranked path puts the subject hit first; the chronological one scored 0.333 here")

	require.Len(t, report.Diagnostics.SkippedCells, 1, "the malformed topic must be reported, not dropped")
	assert.Contains(t, report.Diagnostics.SkippedCells[0], "topic q2")
	assert.Contains(t, report.Diagnostics.SkippedCells[0], "before:invalid renewal")
}
