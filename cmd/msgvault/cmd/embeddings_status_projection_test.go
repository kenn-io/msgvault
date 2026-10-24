//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

type statusProvider struct{ err error }

func (p *statusProvider) Embed(_ context.Context, text []string) ([][]float32, error) {
	if p.err != nil {
		return nil, p.err
	}
	result := make([][]float32, len(text))
	for i := range result {
		result[i] = []float32{1, 0, 0, 0}
	}
	return result, nil
}

type embeddingProjectionFixture struct {
	cfg        *config.Config
	source     embeddingStatusSource
	provider   *statusProvider
	worker     *embed.Worker
	generation vector.GenerationID
	messageID  int64
	sourceID   int64
}

func newEmbeddingProjectionFixture(t *testing.T) *embeddingProjectionFixture {
	t.Helper()
	require := require.New(t)
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(dir, "vectors.db")
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.ApplyDefaults()
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	source, err := st.GetOrCreateSource("gmail", "synthetic@example.com")
	require.NoError(err)
	cfg.Vector.Embed.Scope.SourceIDs = []int64{source.ID}
	conversation, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "synthetic-message", MessageType: "email", Subject: sql.NullString{String: "Synthetic subject", Valid: true}})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "Synthetic body", Valid: true}, sql.NullString{}))
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, MainPath: cfg.DatabaseDSN(), MainDB: st.DB(), Dimension: 4})
	require.NoError(err)
	t.Cleanup(func() { _ = backend.Close() })
	provider := &statusProvider{}
	worker := embed.NewWorker(embed.WorkerDeps{Backend: backend, VectorsDB: backend.DB(), MainDB: st.DB(), Store: st, Client: provider, BatchSize: 2, MaxConsecutiveFailures: 1, Recorder: st, Diagnostics: st})
	return &embeddingProjectionFixture{
		cfg: cfg, source: embeddingStatusSource{st: st, backend: backend, cfg: cfg.Vector, coverage: newEmbeddingCoverageCache(t.Context(), nil)},
		provider: provider, worker: worker, messageID: id, sourceID: source.ID,
	}
}

func (f *embeddingProjectionFixture) build(t *testing.T) {
	t.Helper()
	gen, err := f.source.backend.CreateGeneration(t.Context(), f.cfg.Vector.Embeddings.Model, 4, f.cfg.Vector.GenerationFingerprint())
	require.NoError(t, err)
	f.generation = gen
	_, err = f.worker.RunOnce(t.Context(), gen, operations.PassScope{Key: "synthetic-status-pass", Trigger: operations.TriggerManual, StartedAt: time.Now().UTC()})
	require.NoError(t, err)
}

func (f *embeddingProjectionFixture) fail(t *testing.T) {
	t.Helper()
	require := require.New(t)
	f.build(t)
	require.NoError(f.source.backend.ActivateGeneration(t.Context(), f.generation, false))
	require.NoError(f.source.st.ResetEmbedGen(t.Context(), []int64{f.messageID}))
	f.provider.err = errors.New("synthetic provider failure")
	_, err := f.worker.RunBackstop(t.Context(), f.generation, operations.PassScope{Key: "synthetic-failed-pass", Trigger: operations.TriggerManual, StartedAt: time.Now().UTC()})
	require.Error(err)
}

func (f *embeddingProjectionFixture) read(t *testing.T, sourceID int64) vector.EmbeddingStatus {
	t.Helper()
	f.source.coverage = newEmbeddingCoverageCache(t.Context(), nil)
	status, err := f.source.read(t.Context(), sourceID)
	require.NoError(t, err)
	return status
}

func TestEmbeddingStatusProjectionValidatesSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEmbeddingProjectionFixture(t)
	empty, err := f.source.st.GetOrCreateSource("gmail", "empty@example.com")
	require.NoError(err)
	_, err = f.source.read(t.Context(), empty.ID+100)
	require.ErrorIs(err, vector.ErrEmbeddingStatusUnknownSource)
	_, err = f.source.read(t.Context(), empty.ID)
	require.ErrorIs(err, vector.ErrEmbeddingStatusSourceOutOfScope)
	f.source.cfg.Embed.Scope.SourceIDs = []int64{f.sourceID, empty.ID}
	status := f.read(t, empty.ID)
	assert.Zero(status.Eligible, "an existing source within scope can have no eligible messages")
	assert.Zero(status.Pending)
}

func TestEmbeddingStatusProjectionPreservesCoverageAndScopesDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name        string
		query       string
		fail        bool
		filter      bool
		retire      bool
		broader     bool
		replacement bool
	}{
		{name: "broader coverage", broader: true},
		{name: "reused generation fingerprint", replacement: true},
		{name: "source filter preserves run-wide failures", fail: true, filter: true},
		{name: "retired generation", fail: true, retire: true},
		{name: "unreadable diagnostics", fail: true, query: "ALTER TABLE embedding_diagnostics RENAME TO unavailable_diagnostics"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newEmbeddingProjectionFixture(t)
			if tc.fail {
				f.fail(t)
			} else {
				f.build(t)
			}
			before := f.read(t, 0)
			require.NotNil(before.Diagnostics)
			var sourceID int64
			switch {
			case tc.broader:
				require.NotNil(before.Diagnostics.FinishedAt)
				require.NoError(f.source.backend.ActivateGeneration(t.Context(), f.generation, false))
				f.source.cfg.Embed.Scope.SourceIDs = nil
			case tc.replacement:
				completedSnapshot := *before.Diagnostics
				replacementConfig := f.cfg.Vector
				replacementConfig.Embeddings.DocumentPrefix = "replacement: "
				replacement, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(f.cfg.Data.DataDir, "replacement-vectors.db"), MainPath: f.cfg.DatabaseDSN(), MainDB: f.source.st.DB(), Dimension: 4})
				require.NoError(err)
				t.Cleanup(func() { _ = replacement.Close() })
				replacementGen, err := replacement.CreateGeneration(t.Context(), replacementConfig.Embeddings.Model, 4, replacementConfig.GenerationFingerprint())
				require.NoError(err)
				require.Equal(f.generation, replacementGen, "a replaced vectors database can reuse a generation ID")
				require.NoError(f.source.st.SaveEmbeddingDiagnostics(t.Context(), completedSnapshot))
				f.source.backend, f.source.cfg = replacement, replacementConfig
			case tc.filter:
				f.source.cfg.Embed.Scope.SourceIDs = nil
				empty, err := f.source.st.GetOrCreateSource("gmail", "empty@example.com")
				require.NoError(err)
				sourceID = empty.ID
			case tc.retire:
				require.NoError(f.source.backend.RetireGeneration(t.Context(), f.generation, true))
			default:
				_, err := f.source.st.DB().ExecContext(t.Context(), tc.query)
				require.NoError(err)
			}
			var logs bytes.Buffer
			f.source.coverage = newEmbeddingCoverageCache(t.Context(), slog.New(slog.NewTextHandler(&logs, nil)))
			status, err := f.source.read(t.Context(), sourceID)
			require.NoError(err, "optional diagnostics cannot discard coverage and run status")
			if tc.broader || tc.replacement {
				assert.Nil(status.Diagnostics, "diagnostics must describe the coverage scope and generation fingerprint")
				return
			}
			assert.Equal(int64(1), status.Failed)
			if sourceID == 0 {
				assert.Equal(int64(1), status.Pending)
				assert.Nil(status.Diagnostics)
				if tc.retire {
					assert.Equal("not_initialized", status.Generation.State)
				} else {
					assert.Equal(f.generation, status.Generation.ID)
					assert.Contains(logs.String(), "read embedding diagnostics")
				}
			}
		})
	}
}

func TestEmbeddingCoverageCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(dir, "msgvault.db"))
	source, err := st.GetOrCreateSource("gmail", "synthetic@example.com")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic")
	require.NoError(err)
	addMessage := func(sourceMessageID string) {
		_, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: sourceMessageID, MessageType: "email"})
		require.NoError(err)
	}
	eligible := func(cache *embeddingCoverageCache, ctx context.Context, sourceIDs []int64) int64 {
		t.Helper()
		got, err := cache.counts(ctx, st, 1, nil, sourceIDs)
		require.NoError(err)
		return got.eligible
	}
	expire := func(cache *embeddingCoverageCache, age time.Duration) {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		countedAt := time.Now().Add(-age)
		for key, entry := range cache.entries {
			entry.countedAt = countedAt
			cache.entries[key] = entry
		}
	}
	scoped := []int64{source.ID}
	addMessage("first")
	cache := newEmbeddingCoverageCache(t.Context(), nil)
	assert.Equal(int64(1), eligible(cache, t.Context(), nil))
	addMessage("second")
	assert.Equal(int64(1), eligible(cache, t.Context(), nil), "a poll within the TTL reuses the counts")
	assert.Equal(int64(2), eligible(cache, t.Context(), scoped), "a different scope is counted fresh")
	assert.Equal(int64(1), eligible(cache, t.Context(), nil), "each scope keeps its own counts")

	// A refresh that finishes quickly answers the poll with fresh counts.
	addMessage("third")
	expire(cache, embeddingCoverageCacheTTL+time.Second)
	assert.Equal(int64(3), eligible(cache, t.Context(), scoped))

	// A request that gives up before its count finishes still fills the cache.
	slow := newEmbeddingCoverageCache(t.Context(), nil)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = slow.counts(cancelled, st, 1, nil, nil)
	require.ErrorIs(err, context.Canceled)
	require.Eventually(func() bool {
		slow.mu.Lock()
		defer slow.mu.Unlock()
		return len(slow.entries) == 1
	}, 10*time.Second, 10*time.Millisecond, "the abandoned request's count was cached")
	addMessage("fourth")
	assert.Equal(int64(3), eligible(slow, t.Context(), nil))

	// A failed refresh serves cached counts only within the stale limit.
	broken, err := store.Open(filepath.Join(dir, "msgvault.db"))
	require.NoError(err)
	failing := newEmbeddingCoverageCache(t.Context(), slog.New(slog.DiscardHandler))
	cached, err := failing.counts(t.Context(), broken, 1, nil, nil)
	require.NoError(err)
	require.NoError(broken.Close())
	expire(failing, embeddingCoverageStaleLimit-10*time.Second)
	stale, err := failing.counts(t.Context(), broken, 1, nil, nil)
	require.NoError(err, "a failed refresh can serve counts within the stale limit")
	assert.Equal(cached.eligible, stale.eligible)
	assert.Equal(cached.current, stale.current)
	assert.Equal(cached.pending, stale.pending)
	expire(failing, embeddingCoverageStaleLimit+time.Second)
	_, err = failing.counts(t.Context(), broken, 1, nil, nil)
	require.Error(err, "counts beyond the stale limit cannot hide a refresh failure")
}
