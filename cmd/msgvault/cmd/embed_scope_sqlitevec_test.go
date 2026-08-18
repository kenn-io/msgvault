//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// fakeEmbeddingsServer answers OpenAI-compatible /embeddings requests with
// one deterministic dim-4 vector per input, recording how many inputs it was
// asked to embed.
func fakeEmbeddingsServer(t *testing.T, embedded *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		*embedded += len(req.Input)
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		payload := struct {
			Data  []item `json:"data"`
			Model string `json:"model"`
		}{Model: "test-model"}
		for i := range req.Input {
			payload.Data = append(payload.Data, item{Embedding: []float32{1, 0, 0, 0}, Index: i})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedTwoAccountMainDB builds a real main DB (via InitSchema) with one
// message+body in each of two accounts.
func seedTwoAccountMainDB(t *testing.T, dataDir string) {
	t.Helper()
	s, err := store.Open(filepath.Join(dataDir, "msgvault.db"))
	require.NoError(t, err, "open main db")
	defer func() { require.NoError(t, s.Close()) }()
	require.NoError(t, s.InitSchema(), "InitSchema")
	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'a@example.com'), (2, 'gmail', 'b@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread'), (2, 2, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, subject) VALUES
	(1, 1, 1, 'a1', 'email', 'hello from a'),
	(2, 2, 2, 'b1', 'email', 'hello from b');
INSERT INTO message_bodies (message_id, body_text) VALUES (1, 'body a1'), (2, 'body b1');
`)
	require.NoError(t, err, "seed corpus")
}

func embedGenByID(t *testing.T, dataDir string) map[int64]sql.NullInt64 {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "msgvault.db"))
	require.NoError(t, err, "open main db handle")
	defer func() { require.NoError(t, db.Close()) }()
	rows, err := db.Query(`SELECT id, embed_gen FROM messages ORDER BY id`)
	require.NoError(t, err, "read embed_gen")
	defer func() { _ = rows.Close() }()
	out := map[int64]sql.NullInt64{}
	for rows.Next() {
		var id int64
		var gen sql.NullInt64
		require.NoError(t, rows.Scan(&id, &gen))
		out[id] = gen
	}
	require.NoError(t, rows.Err())
	return out
}

// TestRunEmbed_AccountScopedBuild drives the full runEmbed path (real
// sqlitevec backend, real store, fake embeddings endpoint): a build scoped
// to one account embeds and stamps only that account's messages and
// activates, and a follow-up run scoped differently is refused by the
// fingerprint mismatch with the scope visible in the error.
func TestRunEmbed_AccountScopedBuildActivatesScopedGeneration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var embedded int
	srv := fakeEmbeddingsServer(t, &embedded)

	dataDir := t.TempDir()
	seedTwoAccountMainDB(t, dataDir)

	oldCfg := cfg
	c := &config.Config{}
	c.Vector.Enabled = true
	c.Vector.DBPath = filepath.Join(dataDir, "vectors.db")
	c.Vector.Embeddings.Endpoint = srv.URL
	c.Vector.Embeddings.Model = "test-model"
	c.Vector.Embeddings.Dimension = 4
	c.Data.DataDir = dataDir
	cfg = c

	oldRebuild, oldYes := embedFullRebuild, embedYes
	oldAccounts, oldCollections := embedAccounts, embedCollections
	oldBackstop := embedBackstop
	t.Cleanup(func() {
		cfg = oldCfg
		embedFullRebuild, embedYes = oldRebuild, oldYes
		embedAccounts, embedCollections = oldAccounts, oldCollections
		embedBackstop = oldBackstop
	})
	embedYes = true
	embedBackstop = false

	newCmd := func() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
		cmd.SetOut(out)
		cmd.SetErr(errOut)
		return cmd, out, errOut
	}

	// Run 1: full rebuild scoped to account A.
	embedFullRebuild = true
	embedAccounts = []string{"a@example.com"}
	cmd, out, errOut := newCmd()
	require.NoError(runEmbeddingsBuildLocal(cmd), "scoped full rebuild")
	assert.Contains(errOut.String(), "Embedding scope: src-1", "scope printed to stderr")
	assert.Contains(errOut.String(), "add equivalent accounts to [vector.embed.scope]", "one-off scope warns how to persist its generation")
	assert.Contains(out.String(), "Generation 1 activated.", "scoped generation activates")
	assert.Equal(1, embedded, "only account A's message reached the embedding endpoint")

	gens := embedGenByID(t, dataDir)
	require.True(gens[1].Valid, "account A message stamped")
	assert.Equal(int64(1), gens[1].Int64)
	assert.False(gens[2].Valid, "account B message keeps embed_gen NULL")

	// Run 2: same archive, scoped to account B instead — the fingerprint
	// difference must refuse to top up the src-1 generation.
	embedFullRebuild = false
	embedAccounts = []string{"b@example.com"}
	cmd, _, _ = newCmd()
	err := runEmbeddingsBuildLocal(cmd)
	require.Error(err, "a different account scope must not resume the src-1 generation")
	assert.Contains(err.Error(), "src-", "stored vs configured scope is visible in the error")
	assert.Contains(err.Error(), "full-rebuild", "error points at --full-rebuild")
}

// TestRunEmbed_AccountScopedRebuildRefusesEmptyScope guards the empty-scope
// activation hazard: a full rebuild scoped to an account that exists but has
// no live messages (added but never synced) would immediately reach
// remaining == 0 and activate an empty generation, retiring the working
// index. The run must fail instead of activating.
func TestRunEmbed_AccountScopedRebuildRefusesEmptyScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var embedded int
	srv := fakeEmbeddingsServer(t, &embedded)

	dataDir := t.TempDir()
	seedTwoAccountMainDB(t, dataDir)
	s, err := store.Open(filepath.Join(dataDir, "msgvault.db"))
	require.NoError(err, "open main db")
	_, err = s.DB().Exec(`INSERT INTO sources (id, source_type, identifier) VALUES (3, 'gmail', 'c@example.com')`)
	require.NoError(err, "seed empty account")
	require.NoError(s.Close())

	oldCfg := cfg
	c := &config.Config{}
	c.Vector.Enabled = true
	c.Vector.DBPath = filepath.Join(dataDir, "vectors.db")
	c.Vector.Embeddings.Endpoint = srv.URL
	c.Vector.Embeddings.Model = "test-model"
	c.Vector.Embeddings.Dimension = 4
	c.Data.DataDir = dataDir
	cfg = c

	oldRebuild, oldYes := embedFullRebuild, embedYes
	oldAccounts, oldCollections := embedAccounts, embedCollections
	oldBackstop := embedBackstop
	t.Cleanup(func() {
		cfg = oldCfg
		embedFullRebuild, embedYes = oldRebuild, oldYes
		embedAccounts, embedCollections = oldAccounts, oldCollections
		embedBackstop = oldBackstop
	})
	embedYes = true
	embedBackstop = false
	embedFullRebuild = true
	embedAccounts = []string{"c@example.com"}

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)

	err = runEmbeddingsBuildLocal(cmd)
	require.Error(err, "an empty account scope must not activate")
	assert.Contains(err.Error(), "0 live messages")
	assert.NotContains(out.String(), "activated", "no generation may activate")
	assert.Zero(embedded, "no message text may reach the embedding endpoint")
}
