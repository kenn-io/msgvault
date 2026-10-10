//go:build sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
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

func TestEmbeddingStatusProjectionDuringRealWorker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(dir, "vectors.db")
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.ApplyDefaults()
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { _ = st.Close() }()
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "synthetic@example.test")
	require.NoError(err)
	cfg.Vector.Embed.Scope.SourceIDs = []int64{source.ID}
	conversation, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "synthetic-message", MessageType: "email", Subject: sql.NullString{String: "Synthetic subject", Valid: true}})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "Synthetic body", Valid: true}, sql.NullString{}))
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, MainPath: cfg.DatabaseDSN(), MainDB: st.DB(), Dimension: 4})
	require.NoError(err)
	defer func() { _ = backend.Close() }()
	withoutGeneration, err := readEmbeddingStatus(t.Context(), st, backend, cfg.Vector, 0, nil)
	require.NoError(err)
	assert.Equal(int64(1), withoutGeneration.Eligible)
	assert.Equal(int64(1), withoutGeneration.Pending)
	assert.Zero(withoutGeneration.Current)
	gen, err := backend.CreateGeneration(t.Context(), cfg.Vector.Embeddings.Model, 4, cfg.Vector.GenerationFingerprint())
	require.NoError(err)
	provider := &statusProvider{}
	worker := embed.NewWorker(embed.WorkerDeps{Backend: backend, VectorsDB: backend.DB(), MainDB: st.DB(), Store: st, Client: provider, BatchSize: 2, MaxConsecutiveFailures: 1, Recorder: st, Diagnostics: st})
	_, err = worker.RunOnce(t.Context(), gen, operations.PassScope{Key: "synthetic-status-pass", Trigger: operations.TriggerManual, StartedAt: time.Now().UTC()})
	require.NoError(err)
	status, err := readEmbeddingStatus(t.Context(), st, backend, cfg.Vector, 0, nil)
	require.NoError(err)
	direct, err := st.MissingCount(t.Context(), int64(gen))
	require.NoError(err)
	assert.Equal(direct, status.Pending)
	assert.Equal(int64(1), status.Current)
	require.NotNil(status.Diagnostics)
	require.NotNil(status.Diagnostics.FinishedAt)
	_, err = worker.RunOnce(t.Context(), gen, operations.PassScope{Key: "synthetic-empty-pass", Trigger: operations.TriggerManual, StartedAt: time.Now().UTC()})
	require.NoError(err)
	diagnostics, err := st.ReadEmbeddingDiagnostics(t.Context(), int64(gen))
	require.NoError(err)
	assert.Equal(status.Diagnostics, diagnostics, "a successful pass without work preserves the latest diagnostic snapshot")

	require.NoError(backend.ActivateGeneration(t.Context(), gen, false))
	broader := cfg.Vector
	broader.Embed.Scope.SourceIDs = nil
	status, err = readEmbeddingStatus(t.Context(), st, backend, broader, 0, nil)
	require.NoError(err)
	assert.Equal(gen, status.Generation.ID)
	require.NotNil(status.ActiveGeneration)
	assert.Equal(gen, status.ActiveGeneration.ID)
	assert.Equal(int64(1), status.Current)
	assert.Nil(status.Diagnostics, "a scoped build's diagnostics can't supply rates for broader coverage")

	require.NoError(st.ResetEmbedGen(t.Context(), []int64{id}))
	provider.err = errors.New("synthetic provider failure")
	_, err = worker.RunBackstop(t.Context(), gen, operations.PassScope{Key: "synthetic-failed-pass", Trigger: operations.TriggerManual, StartedAt: time.Now().UTC()})
	require.Error(err)
	failed, err := readEmbeddingStatus(t.Context(), st, backend, cfg.Vector, 0, nil)
	require.NoError(err)
	assert.Equal(int64(1), failed.Failed)
	require.NotNil(failed.Diagnostics)
	scopedOutcome, err := readEmbeddingStatus(t.Context(), st, backend, broader, 0, nil)
	require.NoError(err)
	assert.Equal(int64(1), scopedOutcome.Failed)
	assert.Nil(scopedOutcome.Diagnostics)
	excludedFailure, err := readEmbeddingStatus(t.Context(), st, backend, cfg.Vector, source.ID+100, nil)
	require.NoError(err)
	assert.Zero(excludedFailure.Eligible)
	assert.Equal(int64(1), excludedFailure.Failed)
	_, err = st.DB().Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	require.NoError(err)
	_, err = worker.RunBackstop(t.Context(), gen, operations.PassScope{Key: "synthetic-empty-after-failure", Trigger: operations.TriggerManual, StartedAt: time.Now().UTC()})
	require.NoError(err)
	empty, err := readEmbeddingStatus(t.Context(), st, backend, cfg.Vector, 0, nil)
	require.NoError(err)
	assert.Zero(empty.Failed)
	assert.Equal(failed.Diagnostics, empty.Diagnostics)
	require.NoError(st.SaveEmbeddingDiagnostics(t.Context(), *diagnostics))
	_, err = st.DB().Exec(`CREATE TRIGGER fail_diagnostic_write BEFORE INSERT ON embedding_diagnostics BEGIN SELECT RAISE(ABORT, 'synthetic diagnostic persistence failure'); END`)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE messages SET deleted_at = NULL WHERE id = ?`, id)
	require.NoError(err)
	_, err = worker.RunBackstop(t.Context(), gen, operations.PassScope{Key: "synthetic-failure-without-sample", Trigger: operations.TriggerManual, StartedAt: time.Now().UTC()})
	require.Error(err)
	withoutSample, err := readEmbeddingStatus(t.Context(), st, backend, cfg.Vector, 0, nil)
	require.NoError(err)
	assert.Equal(int64(1), withoutSample.Failed)
	assert.Equal(diagnostics, withoutSample.Diagnostics)

	require.NoError(backend.RetireGeneration(t.Context(), gen, true))
	retired, err := readEmbeddingStatus(t.Context(), st, backend, cfg.Vector, 0, nil)
	require.NoError(err)
	assert.Equal("not_initialized", retired.Generation.State)
	assert.Equal(int64(1), retired.Pending)
	assert.Equal(int64(1), retired.Failed)
	assert.Nil(retired.Diagnostics)

	require.NoError(backend.Close())
	backend, err = sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, MainPath: cfg.DatabaseDSN(), MainDB: st.DB(), Dimension: 4})
	require.NoError(err)
	retained, err := st.ReadEmbeddingDiagnostics(t.Context(), int64(gen))
	require.NoError(err)
	assert.Equal(diagnostics, retained, "retired generations keep diagnostics while their rows exist")
	require.NoError(backend.Close())
	require.NoError(os.Remove(cfg.Vector.DBPath))
	backend, err = sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, MainPath: cfg.DatabaseDSN(), MainDB: st.DB(), Dimension: 4})
	require.NoError(err)
	cleared, err := st.ReadEmbeddingDiagnostics(t.Context(), int64(gen))
	require.NoError(err)
	assert.Nil(cleared, "writable Open removes old diagnostics before generation IDs can be reused")
	recreated, err := backend.CreateGeneration(t.Context(), cfg.Vector.Embeddings.Model, 4, cfg.Vector.GenerationFingerprint())
	require.NoError(err)
	assert.Equal(gen, recreated)
	replacement, err := readEmbeddingStatus(t.Context(), st, backend, cfg.Vector, 0, nil)
	require.NoError(err)
	assert.Nil(replacement.Diagnostics)
	assert.Nil(replacement.MessagesPerMinute)
	_, err = st.DB().Exec(`DROP TABLE embedding_diagnostics`)
	require.NoError(err)
	require.NoError(backend.Close())
	backend, err = sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, MainPath: cfg.DatabaseDSN(), MainDB: st.DB(), Dimension: 4})
	require.NoError(err, "older archives without diagnostics still open")
}

func TestEmbeddingCoverageCacheReusesRecentCounts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = st.Close() }()
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "synthetic@example.test")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic")
	require.NoError(err)
	addMessage := func(sourceMessageID string) {
		_, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: sourceMessageID, MessageType: "email"})
		require.NoError(err)
	}
	addMessage("first")
	cache := &embeddingCoverageCache{}
	eligible, _, _, err := cache.counts(t.Context(), st, 1, nil, nil)
	require.NoError(err)
	assert.Equal(int64(1), eligible)
	addMessage("second")
	eligible, _, _, err = cache.counts(t.Context(), st, 1, nil, nil)
	require.NoError(err)
	assert.Equal(int64(1), eligible, "a poll within the TTL reuses the counts")
	eligible, _, _, err = cache.counts(t.Context(), st, 1, nil, []int64{source.ID})
	require.NoError(err)
	assert.Equal(int64(2), eligible, "a different scope is counted fresh")
	cache.expires = time.Time{}
	eligible, _, _, err = cache.counts(t.Context(), st, 1, nil, nil)
	require.NoError(err)
	assert.Equal(int64(2), eligible, "expired counts are read again")
}
