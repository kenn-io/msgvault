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
	"go.kenn.io/msgvault/internal/store"
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
	st, err := store.Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "synthetic-thread", "Project plan")
	require.NoError(err)
	var ids []int64
	for i, year := range []int{2024, 2026} {
		id := storetest.NewMessage(source.ID, conv).WithSourceMessageID(fmt.Sprintf("synthetic-%d", i)).WithSubject("Project plan").WithSnippet("stored preview").WithSentAt(time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)).Create(t, st)
		body := fmt.Sprintf("Reply %d contextneedle", i)
		require.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		require.NoError(st.UpsertFTS(id, "Project plan", body, "sender@example.com", "recipient@example.com", ""))
		_, err = st.DB().Exec(`INSERT INTO attachments(message_id,filename,mime_type,size,storage_path) VALUES (?,'plan.pdf','application/pdf',10,'synthetic')`, id)
		require.NoError(err)
		ids = append(ids, id)
	}
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(root, "vectors.db"), MainPath: dbPath, MainDB: st.DB(), Dimension: 4})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(backend.Close()) })
	vectorCfg := vector.Config{}
	fingerprint := vectorCfg.GenerationFingerprint()
	gen, err := backend.CreateGeneration(t.Context(), "synthetic", 4, fingerprint)
	require.NoError(err)
	require.NoError(backend.Upsert(t.Context(), gen, []vector.Chunk{{MessageID: ids[0], Vector: []float32{1, 0, 0, 0}}, {MessageID: ids[1], Vector: []float32{0.6, 0.8, 0, 0}}}))
	require.NoError(backend.ActivateGeneration(t.Context(), gen, true))
	engine := hybrid.NewEngine(backend, st.DB(), stubEmbedder{}, hybrid.Config{ExpectedFingerprint: fingerprint, RRFK: 60, KPerSignal: 10, SubjectBoost: 1})
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
	run := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), binary, append([]string{"--home", home}, args...)...) //nolint:gosec // G702: the opt-in test operator supplies the built CLI; argv is passed without a shell.
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home, "TMPDIR=" + root, "NO_COLOR=1"}
		var out, stderr bytes.Buffer
		command.Stdout = &out
		command.Stderr = &stderr
		require.NoError(command.Run(), "CLI %v: %s", args, stderr.String())
		return out.String()
	}
	decode := func(raw string, value any) {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		require.NoError(decoder.Decode(value))
	}
	// The stored preview lacks this term, so this proves --snippet reaches the daemon's include_snippet path.
	out := run("search", "contextneedle", "--snippet")
	assert.Contains(out, "contextneedle")
	// These searches cover filename and metadata propagation across the CLI, daemon and retrieval adapters.
	out = run("search", "filename:plan.pdf", "--json")
	var fts []map[string]any
	decode(out, &fts)
	require.Len(fts, 2)
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
	require.Len(ranked.Results, 2)
	assert.Equal(json.Number(strconv.FormatInt(ids[0], 10)), ranked.Results[0]["id"], "default relevance ranking is preserved")
	for _, mode := range []string{"vector", "hybrid"} {
		out = run("search", "project plan filename:plan.pdf", "--mode", mode, "--json")
		var semantic struct {
			Results []map[string]any `json:"results"`
		}
		decode(out, &semantic)
		require.Len(semantic.Results, 2)
		assert.Equal(json.Number(strconv.FormatInt(conv, 10)), semantic.Results[0]["conversation_id"])
		assert.Equal([]any{"plan.pdf"}, semantic.Results[0]["attachment_names"])
		// This boundary check proves after: reaches retrieval before ranking.
		out = run("search", "project plan after:2026-01-01", "--mode", mode, "--json")
		decode(out, &semantic)
		require.Len(semantic.Results, 1)
		assert.Equal(json.Number(strconv.FormatInt(ids[1], 10)), semantic.Results[0]["id"])
	}
}
