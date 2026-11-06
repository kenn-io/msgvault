//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// Only the external embedding provider is substituted; the built CLI uses production retrieval and HTTP.
func TestSearchDiscoverabilityBuiltCLI(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	binary := os.Getenv("MSGVAULT_E2E_BINARY")
	if binary == "" {
		t.Skip("set MSGVAULT_E2E_BINARY to the make build binary for end-to-end CLI verification")
	}
	binary, err := filepath.Abs(binary)
	require.NoError(err)
	root := t.TempDir()
	dbPath := filepath.Join(root, "msgvault.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "synthetic-thread", "Project plan")
	require.NoError(err)
	var ids []int64
	for i, fixture := range []struct {
		year     int
		filename string
	}{{2024, "plan.pdf"}, {2026, "plan.pdf"}, {2025, "notes.txt"}} {
		id := storetest.NewMessage(source.ID, conv).WithSourceMessageID(fmt.Sprintf("synthetic-%d", i)).WithSubject("Project plan").WithSnippet("stored preview").WithSentAt(time.Date(fixture.year, 1, 1, 0, 0, 0, 0, time.UTC)).Create(t, st)
		body := fmt.Sprintf("Reply %d contextneedle", i)
		require.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		require.NoError(st.UpsertFTS(id, "Project plan", body, "sender@example.com", "recipient@example.com", ""))
		_, err = st.DB().Exec(`INSERT INTO attachments(message_id,filename,mime_type,size,storage_path) VALUES (?,?,'application/octet-stream',10,'synthetic')`, id, fixture.filename)
		require.NoError(err)
		ids = append(ids, id)
	}
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(root, "vectors.db"), MainPath: dbPath, MainDB: st.DB(), Dimension: 4})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(backend.Close()) })
	vectorCfg := vector.Config{}
	vectorCfg.Search.MaxPageSizeHybrid = new(vector.DefaultMaxPageSizeHybrid)
	fingerprint := vectorCfg.GenerationFingerprint()
	gen, err := backend.CreateGeneration(t.Context(), "synthetic", 4, fingerprint)
	require.NoError(err)
	require.NoError(backend.Upsert(t.Context(), gen, []vector.Chunk{
		{MessageID: ids[0], Vector: []float32{0.8, 0.6, 0, 0}},
		{MessageID: ids[1], Vector: []float32{0.6, 0.8, 0, 0}},
		{MessageID: ids[2], Vector: []float32{1, 0, 0, 0}},
	}))
	require.NoError(backend.ActivateGeneration(t.Context(), gen, true))
	engine := hybrid.NewEngine(backend, st.DB(), stubEmbedder{}, hybrid.Config{ExpectedFingerprint: fingerprint, RRFK: 60, KPerSignal: 100, SubjectBoost: 1})
	cfg := &config.Config{HomeDir: root}
	cfg.Data.DataDir = root
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}, Engine: query.NewSQLiteEngine(st.DB()), Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	server.SetVectorFeatures(engine, nil, backend, vectorCfg)
	httpServer := httptest.NewServer(server.Router())
	t.Cleanup(httpServer.Close)
	t.Cleanup(func() { require.NoError(server.Shutdown(context.Background())) })
	home := filepath.Join(root, "cli-home")
	require.NoError(os.MkdirAll(home, 0700))
	require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte("[remote]\nurl = "+strconv.Quote(httpServer.URL)+"\nallow_insecure = true\n"), 0600))
	runWithStderr := func(args ...string) (string, string) {
		t.Helper()
		command := exec.CommandContext(t.Context(), binary, append([]string{"--home", home}, args...)...) //nolint:gosec // G702: the opt-in test operator supplies the built CLI; argv is passed without a shell.
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home, "TMPDIR=" + root, "NO_COLOR=1"}
		var out, stderr bytes.Buffer
		command.Stdout = &out
		command.Stderr = &stderr
		require.NoError(command.Run(), "CLI %v: %s", args, stderr.String())
		return out.String(), stderr.String()
	}
	run := func(args ...string) string {
		t.Helper()
		out, _ := runWithStderr(args...)
		return out
	}
	decode := func(raw string, value any) {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		require.NoError(decoder.Decode(value))
	}
	command := exec.CommandContext(t.Context(), binary, "--home", home, "search", "from:@a.example OR from:@b.example", "--json") //nolint:gosec // The binary path is supplied by the test operator.
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home, "TMPDIR=" + root, "NO_COLOR=1"}
	var jsonOut, warningOut bytes.Buffer
	command.Stdout, command.Stderr = &jsonOut, &warningOut
	require.NoError(command.Run())
	var warningResults []map[string]any
	decode(jsonOut.String(), &warningResults)
	assert.Contains(warningOut.String(), `OR was matched as a literal word`)
	// The stored preview lacks this term, so this proves --snippet reaches the daemon's include_snippet path.
	out := run("search", "contextneedle", "--snippet")
	assert.Contains(out, "contextneedle")
	out = run("search", "contextneedle | missingterm", "--snippet")
	assert.Contains(out, "contextneedle")
	// These searches cover filename and metadata propagation across the CLI, daemon and retrieval adapters.
	out = run("search", "filename:plan.pdf", "--json")
	var fts []map[string]any
	decode(out, &fts)
	require.Len(fts, 2)
	for _, result := range fts {
		assert.NotEqual(json.Number(strconv.FormatInt(ids[2], 10)), result["id"], "exclude the message whose attachment does not match")
	}
	assert.Equal([]any{"plan.pdf"}, fts[0]["attachment_names"])
	// Isolated command roots don't prove the packaged alias and thread command can read through the daemon.
	out = run("show", strconv.FormatInt(ids[0], 10), "--body-only")
	assert.Contains(out, "Reply 0")
	out = run("show-thread", strconv.FormatInt(ids[0], 10))
	assert.Contains(out, "Reply 0")
	// The full pipeline must preserve relevance unless the user requests another sort.
	out = run("search", "project plan", "--mode", "vector", "--json")
	var ranked struct {
		Results []map[string]any `json:"results"`
	}
	decode(out, &ranked)
	require.Len(ranked.Results, 3)
	assert.Equal(json.Number(strconv.FormatInt(ids[2], 10)), ranked.Results[0]["id"], "the filename decoy ranks first without filtering")
	for _, mode := range []string{"vector", "hybrid"} {
		out = run("search", "project plan filename:plan.pdf", "--mode", mode, "--json")
		var semantic struct {
			Results []map[string]any `json:"results"`
		}
		decode(out, &semantic)
		require.Len(semantic.Results, 2)
		for _, result := range semantic.Results {
			assert.NotEqual(json.Number(strconv.FormatInt(ids[2], 10)), result["id"], "%s excludes the message whose attachment does not match", mode)
		}
		assert.Equal(json.Number(strconv.FormatInt(conv, 10)), semantic.Results[0]["conversation_id"])
		assert.Equal([]any{"plan.pdf"}, semantic.Results[0]["attachment_names"])
		// This boundary check proves after: reaches retrieval before ranking.
		out = run("search", "project plan after:2026-01-01", "--mode", mode, "--json")
		decode(out, &semantic)
		require.Len(semantic.Results, 1)
		assert.Equal(json.Number(strconv.FormatInt(ids[1], 10)), semantic.Results[0]["id"])
	}
	// A final semantic page gives no more-results hint.
	var page struct {
		Results       []map[string]any    `json:"results"`
		PoolSaturated bool                `json:"pool_saturated"`
		Hints         []map[string]string `json:"hints"`
	}
	decode(run("search", "project plan filename:plan.pdf", "--mode", "vector", "--limit", "2", "--json"), &page)
	assert.Len(page.Results, 2)
	assert.Empty(page.Hints)
	// Exactly full pages and truncated pages differ by one measured row.
	for i := range 101 {
		subject := "paginationneedle"
		if i < 51 {
			subject += " overflowboundary"
		}
		if i < 50 {
			subject += " exactboundary"
		}
		year := 2026
		if i >= 50 {
			year = 2027
		}
		message := storetest.NewMessage(source.ID, conv).WithSubject(subject).WithSentAt(time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)).Build()
		domain := fmt.Sprintf("sender-%02d.example.com", i%51)
		address := "sender@" + domain
		sender, err := st.EnsureParticipant(address, "", domain)
		require.NoError(err)
		message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := st.UpsertMessage(message)
		require.NoError(err)
		require.NoError(st.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
		label := fmt.Sprintf("label-%02d", i%51)
		labelID, err := st.EnsureLabel(source.ID, label, label, "user")
		require.NoError(err)
		require.NoError(st.ReplaceMessageLabels(id, []int64{labelID}))
		require.NoError(st.UpsertFTS(id, subject, "", address, "", ""))
		require.NoError(backend.Upsert(t.Context(), gen, []vector.Chunk{{MessageID: id, Vector: []float32{1, 0, 0, 0}}}))
	}
	for _, tc := range []struct{ query, offset, hint string }{
		{"overflowboundary", "0", "--offset 50"},
		{"exactboundary", "0", ""},
		{"paginationneedle", "50", "--offset 100"},
	} {
		out, stderr := runWithStderr("search", tc.query, "--offset", tc.offset, "--json")
		var rows []map[string]any
		decode(out, &rows)
		assert.Len(rows, 50)
		if tc.hint == "" {
			assert.NotContains(stderr, "Hint:")
		} else {
			assert.Contains(stderr, "Hint: more messages match; use "+tc.hint+" for the next page or raise --limit")
		}
	}
	for _, list := range []string{"senders", "domains", "labels"} {
		for _, exact := range []bool{false, true} {
			args := []string{"list-" + list, "--json"}
			if exact {
				args = append(args, "--before", "2027-01-01")
			}
			out, stderr := runWithStderr(args...)
			var rows []map[string]any
			decode(out, &rows)
			assert.Len(rows, 50)
			if exact {
				assert.NotContains(stderr, "Hint:")
			} else {
				assert.Contains(stderr, "Hint: more "+list+" exist; raise -n")
			}
		}
	}
	for _, args := range [][]string{{"search", "overflowboundary"}, {"list-senders"}, {"list-domains"}, {"list-labels"}} {
		command := exec.CommandContext(t.Context(), binary, append([]string{"--home", home}, args...)...) //nolint:gosec // The opt-in test operator supplies the built CLI.
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home, "TMPDIR=" + root, "NO_COLOR=1"}
		combined, err := command.CombinedOutput()
		require.NoError(err, string(combined))
		footer := bytes.Index(combined, []byte("Showing 50 results"))
		hint := bytes.Index(combined, []byte("Hint:"))
		require.GreaterOrEqual(footer, 0)
		assert.Greater(hint, footer, "the hint follows the results footer")
	}
	// Default semantic cap still reports a measured match beyond the returned page.
	for _, mode := range []string{"vector", "hybrid"} {
		for _, boundary := range []string{"overflowboundary", "exactboundary"} {
			out, stderr := runWithStderr("search", boundary+" subject:"+boundary, "--mode", mode)
			assert.Contains(out, "Showing 50 results")
			if boundary == "overflowboundary" {
				assert.Contains(stderr, "raise vector.search.max_page_size_hybrid above 50 on the server and restart it, then raise --limit")
			} else {
				assert.NotContains(stderr, "Hint:")
			}
		}
	}
	// Hybrid fusion can exhaust its candidate pool before the requested page fills.
	for i := range 312 {
		id := storetest.NewMessage(source.ID, conv).WithSubject("poolfixture").WithSourceMessageID(fmt.Sprintf("pool-%d", i)).Create(t, st)
		require.NoError(st.UpsertFTS(id, "poolfixture", "", "sender@example.com", "", ""))
		require.NoError(backend.Upsert(t.Context(), gen, []vector.Chunk{{MessageID: id, Vector: []float32{1, 0, 0, 0}}}))
	}
	raised := vectorCfg
	raised.Search.MaxPageSizeHybrid = new(100)
	server.SetVectorFeatures(engine, nil, backend, raised)
	// An extra ranked row can be returned by raising --limit alone.
	decode(run("search", "differentword subject:poolfixture", "--mode", "hybrid", "--limit", "10", "--json"), &page)
	assert.True(page.PoolSaturated)
	require.Len(page.Hints, 1)
	assert.Equal("more_available", page.Hints[0]["code"])
	assert.Equal("more messages match; raise --limit", page.Hints[0]["message"])
	// At both caps, widening the pool and the page is needed to return more.
	_, stderr := runWithStderr("search", "differentword subject:poolfixture", "--mode", "hybrid", "--limit", "100")
	assert.Equal(2, strings.Count(stderr, "Hint:"))
	assert.Contains(stderr, "Hint: semantic result limit reached; raise vector.search.max_page_size_hybrid above 100")
	assert.Contains(stderr, "\nHint: hybrid candidate pool may omit matches; raise vector.search.k_per_signal")
	page.Hints = nil
	decode(run("search", "differentword subject:poolfixture", "--mode", "hybrid", "--limit", "100", "--json"), &page)
	assert.Len(page.Results, 100)
	require.Len(page.Hints, 2)
	assert.Equal("ranking_limit_reached", page.Hints[0]["code"])
	assert.Contains(page.Hints[0]["message"], "vector.search.max_page_size_hybrid above 100")
	assert.Equal("candidate_pool_saturated", page.Hints[1]["code"])
	assert.Contains(page.Hints[1]["message"], "vector.search.k_per_signal")
	// A wider page alone exposes the pool limit.
	raised.Search.MaxPageSizeHybrid = new(200)
	server.SetVectorFeatures(engine, nil, backend, raised)
	page.Hints = nil
	decode(run("search", "differentword subject:poolfixture", "--mode", "hybrid", "--limit", "200", "--json"), &page)
	require.Len(page.Hints, 1)
	assert.Equal("candidate_pool_saturated", page.Hints[0]["code"])
	raised.Search.KPerSignal = 200
	engine = hybrid.NewEngine(backend, st.DB(), stubEmbedder{}, hybrid.Config{ExpectedFingerprint: fingerprint, RRFK: 60, KPerSignal: raised.Search.KPerSignal, SubjectBoost: 1})
	server.SetVectorFeatures(engine, nil, backend, raised)
	page.Hints = nil
	decode(run("search", "differentword subject:poolfixture", "--mode", "hybrid", "--limit", "200", "--json"), &page)
	assert.Len(page.Results, 200)
}
