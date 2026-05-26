//go:build sqlite_vec

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// newVectorSearchTestEnv stands up the CLI prerequisites for a
// `msgvault search --mode=vector|hybrid` invocation:
//
//   - a real msgvault.db with InitSchema (so account lookups have a
//     sources table to query)
//   - a real vectors.db with sqlite-vec extension and one active
//     generation (so ResolveActive succeeds)
//   - cfg pointed at the temp dir, with vector enabled and the
//     embeddings endpoint pointed at the supplied test server
//
// Returns the *Store handle so callers can seed sources before invoking
// the CLI. Callers must defer the returned restore func.
func newVectorSearchTestEnv(t *testing.T, embedSrvURL string) (*store.Store, func()) {
	t.Helper()
	dir := t.TempDir()

	dbPath := filepath.Join(dir, "msgvault.db")
	s, err := store.Open(dbPath)
	requirepkg.NoError(t, err, "open store")
	requirepkg.NoError(t, s.InitSchema(), "init schema")

	requirepkg.NoError(t, sqlitevec.RegisterExtension(), "RegisterExtension")
	ctx := context.Background()
	vecPath := filepath.Join(dir, "vectors.db")
	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      vecPath,
		MainPath:  dbPath,
		Dimension: 4,
		MainDB:    s.DB(),
	})
	requirepkg.NoError(t, err, "sqlitevec.Open")
	// Build the vector config first so the generation can be seeded
	// with the same fingerprint the search command will compute at run
	// time. Otherwise ResolveActiveForFingerprint rejects the active
	// generation as stale before the search ever reaches the index.
	vecCfg := vector.Config{
		Enabled: true,
		Backend: "sqlite-vec",
		DBPath:  vecPath,
		Embeddings: vector.EmbeddingsConfig{
			Endpoint:  embedSrvURL + "/v1",
			Model:     "fake-model",
			Dimension: 4,
		},
		Search: vector.SearchConfig{
			RRFK:       60,
			KPerSignal: 10,
		},
	}
	gid, err := b.CreateGeneration(ctx, "fake-model", 4, vecCfg.GenerationFingerprint())
	if err != nil {
		_ = b.Close()
		requirepkg.NoError(t, err, "CreateGeneration")
	}
	if err := b.ActivateGeneration(ctx, gid); err != nil {
		_ = b.Close()
		requirepkg.NoError(t, err, "ActivateGeneration")
	}
	requirepkg.NoError(t, b.Close(), "close backend")

	savedCfg := cfg
	cfg = &config.Config{
		HomeDir: dir,
		Data:    config.DataConfig{DataDir: dir},
		Vector:  vecCfg,
	}

	restore := func() {
		_ = s.Close()
		cfg = savedCfg
		resetSearchFlags()
	}
	return s, restore
}

// fakeEmbedServer is a tiny stub /v1/embeddings server that returns the
// requested number of zero-vectors at the configured dimension.
func fakeEmbedServer(t *testing.T, dim int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, len(req.Input))
		for i := range req.Input {
			vec := make([]float32, dim)
			vec[0] = 1
			items[i] = item{Embedding: vec, Index: i}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  items,
			"model": "fake-model",
		})
	}))
}

// TestSearchCmd_VectorMode_UnknownAccount regression-guards the
// `--mode=vector --account` plumbing added in the earlier batch: an
// unknown account must surface a clear error rather than silently
// running the search across the whole corpus. The error is returned
// before the embedding endpoint is contacted, so the embed stub will
// never be hit if the wiring is correct.
func TestSearchCmd_VectorMode_UnknownAccount(t *testing.T) {
	srv := fakeEmbedServer(t, 4)
	defer srv.Close()

	_, restore := newVectorSearchTestEnv(t, srv.URL)
	defer restore()

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--mode", "vector",
		"--account", "nobody@nowhere.invalid",
		"hello",
	})
	err := root.Execute()
	requirepkg.Error(t, err, "expected error for unknown --account")
	assertpkg.ErrorContains(t, err, "no account found")
}

// TestSearchCmd_VectorMode_AccountScopingResolves verifies that a
// known --account passes the lookup and the command runs to completion.
// The active generation has zero vectors, so the search returns no
// results, but reaching the empty-result branch proves the SourceID
// was resolved and the engine ran without error.
func TestSearchCmd_VectorMode_AccountScopingResolves(t *testing.T) {
	srv := fakeEmbedServer(t, 4)
	defer srv.Close()

	s, restore := newVectorSearchTestEnv(t, srv.URL)
	defer restore()
	_, err := s.GetOrCreateSource("gmail", "alice@example.com")
	requirepkg.NoError(t, err, "seed source")

	done := captureStdout(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--mode", "vector",
		"--account", "alice@example.com",
		"hello",
	})
	err = root.Execute()
	out := done()
	requirepkg.NoError(t, err, "expected no error for known account (out=%s)", out)
	assertpkg.Contains(t, out, "No messages found", "expected 'No messages found' (empty index)")
}

// TestSearchCmd_VectorMode_CollectionScopingResolves verifies that
// --collection is plumbed through to filter.SourceIDs in the vector
// path. Earlier the vector branch only looked at --account directly
// and silently ignored --collection.
func TestSearchCmd_VectorMode_CollectionScopingResolves(t *testing.T) {
	require := requirepkg.New(t)
	srv := fakeEmbedServer(t, 4)
	defer srv.Close()

	s, restore := newVectorSearchTestEnv(t, srv.URL)
	defer restore()
	src, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "seed source")
	_, err = s.CreateCollection("alice-only", "", []int64{src.ID})
	require.NoError(err, "create collection")

	done := captureStdout(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--mode", "vector",
		"--collection", "alice-only",
		"hello",
	})
	err = root.Execute()
	out := done()
	require.NoError(err, "expected no error for known collection (out=%s)", out)
	assertpkg.Contains(t, out, "No messages found", "expected 'No messages found' (empty index)")
}

// TestSearchCmd_VectorMode_CollectionUnknown mirrors the FTS path's
// unknown-collection rejection.
func TestSearchCmd_VectorMode_CollectionUnknown(t *testing.T) {
	srv := fakeEmbedServer(t, 4)
	defer srv.Close()

	_, restore := newVectorSearchTestEnv(t, srv.URL)
	defer restore()

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--mode", "vector",
		"--collection", "does-not-exist",
		"hello",
	})
	err := root.Execute()
	requirepkg.Error(t, err, "expected error for unknown --collection")
	assertpkg.ErrorContains(t, err, "no collection")
}

// TestSearchCmd_HybridMode_UnknownAccount mirrors the vector test for
// mode=hybrid, since the account-lookup path is shared.
func TestSearchCmd_HybridMode_UnknownAccount(t *testing.T) {
	srv := fakeEmbedServer(t, 4)
	defer srv.Close()

	_, restore := newVectorSearchTestEnv(t, srv.URL)
	defer restore()

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--mode", "hybrid",
		"--account", "nobody@nowhere.invalid",
		"hello",
	})
	err := root.Execute()
	requirepkg.Error(t, err, "expected error for unknown --account")
	assertpkg.ErrorContains(t, err, "no account found")
}

// TestSearchCmd_VectorMode_UnscopedRunsMigrations regression-guards
// the upgrade path: a user upgrading to a build that adds the
// deleted_at column whose first command is an unscoped
// `search --mode=vector|hybrid` must not crash with
// "no such column: deleted_at". The unscoped path skips
// resolveSearchScope (which is what runs InitSchema for scoped
// queries) and runHybridSearch opens a raw sql.DB, so the dispatch
// itself must run the migrations. Verified directly: drop
// deleted_at, then assert the dispatch path restores it before
// runHybridSearch's raw sql.DB sees the schema.
func TestSearchCmd_VectorMode_UnscopedRunsMigrations(t *testing.T) {
	require := requirepkg.New(t)
	srv := fakeEmbedServer(t, 4)
	defer srv.Close()

	s, restore := newVectorSearchTestEnv(t, srv.URL)
	defer restore()

	_, err := s.DB().Exec(`ALTER TABLE messages DROP COLUMN deleted_at`)
	require.NoError(err, "drop deleted_at to simulate pre-migration DB")
	// Sanity: column is gone.
	var cnt int
	err = s.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'deleted_at'`,
	).Scan(&cnt)
	require.NoError(err, "pragma_table_info pre-dispatch")
	require.Zero(cnt, "setup: deleted_at still present")

	done := captureStdout(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--mode", "vector",
		"hello",
	})
	// Error from the engine itself is fine for this test — what we're
	// guarding is that the dispatch path runs the schema migration
	// before runHybridSearch opens its raw sql.DB. Other engine-level
	// errors (no vectors, missing fts in the test build) don't
	// invalidate the migration check.
	_ = root.Execute()
	_ = done()

	err = s.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'deleted_at'`,
	).Scan(&cnt)
	require.NoError(err, "pragma_table_info post-dispatch")
	require.Equal(1, cnt, "dispatch did not re-add deleted_at column — runHybridSearch would query a missing column on an upgraded DB")
}
