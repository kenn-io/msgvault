//go:build sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func TestConversationPolicyCleanupRunsWithEmbeddingsDisabled(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite native cleanup fixture")
	f, spec, ctx := documentVectorCommandFixture(t)
	cfg := invocationFromContext(ctx).cfg
	require.NoError(t, f.Store.DB().QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&cfg.Data.DatabaseURL))
	cfg.Vector.Enabled = false
	cfg.Attachments.Documents.Index.Embeddings.Enabled = false
	cfg.Vector.Multimodal.Enabled = false
	cfg.Vector.DBPath = filepath.Join(t.TempDir(), "vectors.db")
	t.Setenv("SYNTHETIC_EMBEDDING_KEY", "")
	message := f.CreateMessage("cold-cleanup")
	native, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: cfg.Vector.DBPath, MainDB: f.Store.DB(), PolicyStore: f.Store, Dimension: 3})
	require.NoError(t, err)
	generation, err := native.CreateGeneration(ctx, "synthetic", 3, "")
	require.NoError(t, err)
	require.NoError(t, native.Upsert(ctx, generation, []vector.Chunk{{MessageID: message, Vector: []float32{1, 0, 0}}}))
	require.NoError(t, native.Close())
	doc, _, err := f.Store.EnsureDocumentVectorGeneration(ctx, spec)
	require.NoError(t, err)
	require.NoError(t, f.Store.CheckpointDocumentVectorBuildForFingerprint(ctx, doc.ID, doc.Fingerprint, 100, false, store.DocumentVectorUsageDelta{}, time.Now().UTC()))
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	lanes, closeLanes, err := openConversationPolicyCleanupLanes(ctx, f.Store)
	require.NoError(t, err)
	defer func() { _ = closeLanes() }()
	require.NoError(t, reconcileConversationPolicyCleanup(ctx, f.Store, lanes))
	_, pending, err := f.Store.ConversationPolicyCleanupPending(ctx)
	require.NoError(t, err)
	assert.False(t, pending)
	cursor, err := f.Store.GetDocumentVectorBuildCursor(ctx, doc.ID)
	require.NoError(t, err)
	assert.Zero(t, cursor)
	require.NoError(t, f.Store.CheckpointDocumentVectorBuildForFingerprint(ctx, doc.ID, doc.Fingerprint, 100, false, store.DocumentVectorUsageDelta{}, time.Now().UTC()))
	require.NoError(t, reconcileConversationPolicyCleanup(ctx, f.Store, conversationPolicyCleanupLanes{}))
	cursor, err = f.Store.GetDocumentVectorBuildCursor(ctx, doc.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(100), cursor, "acknowledged idle check must not rescan/reset work")

	native, err = sqlitevec.Open(ctx, sqlitevec.Options{Path: cfg.Vector.DBPath, MainDB: f.Store.DB(), PolicyStore: f.Store, ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = native.Close() }()
	var count int
	require.NoError(t, native.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings`).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, native.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM index_generations`).Scan(&count))
	assert.Equal(t, 1, count, "cleanup creates no generation")
}

func TestConversationPolicyCleanupMissingFileDoesNotCreateNativeStore(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite native cleanup fixture")
	f, spec, ctx := documentVectorCommandFixture(t)
	cfg := invocationFromContext(ctx).cfg
	require.NoError(t, f.Store.DB().QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&cfg.Data.DatabaseURL))
	cfg.Vector.Enabled = false
	cfg.Vector.DBPath = filepath.Join(t.TempDir(), "absent.db")
	doc, _, err := f.Store.EnsureDocumentVectorGeneration(ctx, spec)
	require.NoError(t, err)
	require.NoError(t, f.Store.CheckpointDocumentVectorBuildForFingerprint(ctx, doc.ID, doc.Fingerprint, 100, false, store.DocumentVectorUsageDelta{}, time.Now().UTC()))
	native, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: cfg.Vector.DBPath, MainDB: f.Store.DB(), PolicyStore: f.Store, Dimension: 3})
	require.NoError(t, err)
	visualGen, owner, oldToken := seedVisualCleanupPublication(t, f, native)
	require.NoError(t, native.Close())
	require.NoError(t, os.Remove(cfg.Vector.DBPath))
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET embed_gen=73 WHERE id=?`), owner.MessageID)
	require.NoError(t, err)

	lanes, closeLanes, err := openConversationPolicyCleanupLanes(ctx, f.Store)
	require.NoError(t, err)
	defer func() { _ = closeLanes() }()
	require.NoError(t, reconcileConversationPolicyCleanup(ctx, f.Store, lanes))
	_, err = os.Stat(cfg.Vector.DBPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	cursor, err := f.Store.GetDocumentVectorBuildCursor(ctx, doc.ID)
	require.NoError(t, err)
	assert.Zero(t, cursor)
	_, pending, err := f.Store.ConversationPolicyCleanupPending(ctx)
	require.NoError(t, err)
	assert.False(t, pending)
	publication, err := f.Store.GetVisualPublication(ctx, visualGen.ID, owner)
	require.NoError(t, err)
	assert.Empty(t, publication.CurrentVectorToken)
	var stamped int
	require.NoError(t, f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE embed_gen IS NOT NULL`).Scan(&stamped))
	assert.Zero(t, stamped)
	claim, acquired, err := f.Store.ClaimVisualWork(ctx, store.VisualClaimRequest{GenerationID: visualGen.ID, Owner: owner, ProposedRevision: "fresh", LeaseOwner: "fresh", Now: time.Now().UTC(), LeaseDuration: time.Minute, SourceFence: publication.SourceFence})
	require.NoError(t, err)
	require.True(t, acquired)
	token, err := f.Store.PrepareVisualPublication(ctx, store.PreparedVisualPublication{Claim: claim, RepresentativeAttachmentID: publication.RepresentativeAttachmentID, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics})
	require.NoError(t, err)
	assert.NotEqual(t, oldToken, token)
	_, err = os.Stat(cfg.Vector.DBPath)
	assert.ErrorIs(t, err, os.ErrNotExist, "main repair and reclaim cannot create native storage")
}

type cleanupEmbeddingHook struct {
	vector.IneligibleEmbeddingPruner

	after func() error
}

func (p cleanupEmbeddingHook) PruneIneligibleEmbeddings(ctx context.Context) (int64, error) {
	n, err := p.IneligibleEmbeddingPruner.PruneIneligibleEmbeddings(ctx)
	if err == nil {
		err = p.after()
	}
	return n, err
}

func TestConversationPolicyCleanupRetriesAndDoesNotAckNewerRevision(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite native cleanup fixture")
	f, _, ctx := documentVectorCommandFixture(t)
	cfg := invocationFromContext(ctx).cfg
	require.NoError(t, f.Store.DB().QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&cfg.Data.DatabaseURL))
	cfg.Vector.Enabled = false
	cfg.Vector.DBPath = filepath.Join(t.TempDir(), "vectors.db")
	message := f.CreateMessage("retry-cleanup")
	native, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: cfg.Vector.DBPath, MainDB: f.Store.DB(), PolicyStore: f.Store, Dimension: 3})
	require.NoError(t, err)
	defer func() { _ = native.Close() }()
	generation, err := native.CreateGeneration(ctx, "synthetic", 3, "")
	require.NoError(t, err)
	require.NoError(t, native.Upsert(ctx, generation, []vector.Chunk{{MessageID: message, Vector: []float32{1, 0, 0}}}))
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	_, err = native.DB().ExecContext(ctx, `CREATE TRIGGER fail_policy_cleanup BEFORE DELETE ON embeddings BEGIN SELECT RAISE(FAIL,'injected cleanup failure'); END`)
	require.NoError(t, err)
	lanes, closeLanes, err := openConversationPolicyCleanupLanes(ctx, f.Store)
	require.NoError(t, err)
	defer func() { _ = closeLanes() }()
	require.ErrorContains(t, reconcileConversationPolicyCleanup(ctx, f.Store, lanes), "injected cleanup failure")
	target, pending, err := f.Store.ConversationPolicyCleanupPending(ctx)
	require.NoError(t, err)
	assert.True(t, pending)
	_, err = native.DB().ExecContext(ctx, `DROP TRIGGER fail_policy_cleanup`)
	require.NoError(t, err)
	original := lanes.Embeddings
	lanes.Embeddings = cleanupEmbeddingHook{IneligibleEmbeddingPruner: original, after: func() error {
		return f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full)
	}}
	require.NoError(t, reconcileConversationPolicyCleanup(ctx, f.Store, lanes))
	newer, pending, err := f.Store.ConversationPolicyCleanupPending(ctx)
	require.NoError(t, err)
	assert.Greater(t, newer, target)
	assert.True(t, pending, "old pass must not acknowledge newer transition")
	lanes.Embeddings = original
	require.NoError(t, reconcileConversationPolicyCleanup(ctx, f.Store, lanes))
	_, pending, err = f.Store.ConversationPolicyCleanupPending(ctx)
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestConversationPolicyCleanupDocumentRetirementMissingFile(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite missing native file")
	f, spec, ctx := documentVectorCommandFixture(t)
	cfg := invocationFromContext(ctx).cfg
	cfg.Vector.Enabled = false
	cfg.Vector.DBPath = filepath.Join(t.TempDir(), "absent.db")
	generation, _, err := f.Store.EnsureDocumentVectorGeneration(ctx, spec)
	require.NoError(t, err)
	_, err = f.Store.RetireDocumentVectorGeneration(ctx, generation.ID, time.Now().UTC())
	require.NoError(t, err)
	result, err := runConfiguredDocumentVectorGeneration(ctx, f.Store, generation.ID, 1)
	require.NoError(t, err)
	assert.True(t, result.Purged)
	_, err = os.Stat(cfg.Vector.DBPath)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestConversationPolicyCleanupUnavailableAndAppearingNativeStayPending(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite missing/corrupt native file")
	f, _, ctx := documentVectorCommandFixture(t)
	cfg := invocationFromContext(ctx).cfg
	cfg.Vector.Enabled = false
	cfg.Vector.DBPath = filepath.Join(t.TempDir(), "vectors.db")
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	require.ErrorContains(t, reconcileConversationPolicyCleanup(ctx, f.Store, conversationPolicyCleanupLanes{}), "unavailable")
	lanes, closeLanes, err := openConversationPolicyCleanupLanes(ctx, f.Store)
	require.NoError(t, err)
	defer func() { _ = closeLanes() }()
	require.NoError(t, os.WriteFile(cfg.Vector.DBPath, []byte("corrupt synthetic native store"), 0600))
	require.ErrorContains(t, reconcileConversationPolicyCleanup(ctx, f.Store, lanes), "appeared")
	_, _, err = openConversationPolicyCleanupLanes(ctx, f.Store)
	require.Error(t, err)
	_, pending, err := f.Store.ConversationPolicyCleanupPending(ctx)
	require.NoError(t, err)
	assert.True(t, pending)
}

func TestConversationPolicyCleanupMissingHintPreservesNewPublication(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite missing-file hint")
	f, _, ctx := documentVectorCommandFixture(t)
	cfg := invocationFromContext(ctx).cfg
	vectorPath := filepath.Join(t.TempDir(), "vectors.db")
	cfg.Vector.DBPath = conversationPolicySQLiteFileURI(vectorPath) + "?_busy_timeout=5000"
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	lanes, closeLanes, err := openConversationPolicyCleanupLanes(ctx, f.Store)
	require.NoError(t, err)
	defer func() { _ = closeLanes() }()
	require.True(t, lanes.nativeMissing)
	assert.Equal(t, vectorPath, lanes.sqlitePath)
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	native, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: cfg.Vector.DBPath, MainDB: f.Store.DB(), PolicyStore: f.Store})
	require.NoError(t, err)
	defer func() { _ = native.Close() }()
	generation, owner, token := seedVisualCleanupPublication(t, f, native)
	require.ErrorContains(t, reconcileConversationPolicyCleanup(ctx, f.Store, lanes), "appeared")
	publication, err := f.Store.GetVisualPublication(ctx, generation.ID, owner)
	require.NoError(t, err)
	assert.Equal(t, token, publication.CurrentVectorToken)
	var count int
	require.NoError(t, native.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM visual_vectors WHERE vector_token=?`, token).Scan(&count))
	assert.Equal(t, 1, count)
	_, pending, err := f.Store.ConversationPolicyCleanupPending(ctx)
	require.NoError(t, err)
	assert.True(t, pending)
}

func TestConversationPolicyCleanupOpensExistingSQLiteURI(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite URI cleanup fixture")
	f, _, ctx := documentVectorCommandFixture(t)
	cfg := invocationFromContext(ctx).cfg
	vectorPath := filepath.Join(t.TempDir(), "vectors.db")
	cfg.Vector.DBPath = conversationPolicySQLiteFileURI(vectorPath) + "?_busy_timeout=5000"
	native, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: cfg.Vector.DBPath, MainDB: f.Store.DB(), PolicyStore: f.Store})
	require.NoError(t, err)
	require.NoError(t, native.Close())

	lanes, closeLanes, err := openConversationPolicyCleanupLanes(ctx, f.Store)
	require.NoError(t, err)
	defer func() { _ = closeLanes() }()
	assert.False(t, lanes.nativeMissing)
	assert.NotNil(t, lanes.Embeddings)
}

func conversationPolicySQLiteFileURI(path string) string {
	uriPath := filepath.ToSlash(path)
	if len(uriPath) >= 3 && uriPath[1] == ':' && uriPath[2] == '/' {
		uriPath = "/" + uriPath
	}
	return (&url.URL{Scheme: "file", Path: uriPath}).String()
}

func TestConversationPolicyRuntimeCleanupWithoutVectorClient(t *testing.T) {
	testutil.SkipIfPostgres(t, "native SQLite file; PostgreSQL runtime companion covers pgvector")
	st, cfg, ctx, room := policyRuntimeFixture(t)
	cfg.Vector.DBPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
	now := time.Now().UTC()
	message, err := st.UpsertMessage(&store.Message{SourceID: 1, ConversationID: room, SourceMessageID: "native-own", MessageType: "beeper", IsFromMe: true, SentAt: sql.NullTime{Time: now, Valid: true}})
	require.NoError(t, err)
	require.NoError(t, initializeConversationPolicy(ctx, st, cfg, now))
	native, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: cfg.Vector.DBPath, MainDB: st.DB(), PolicyStore: st, Dimension: 3})
	require.NoError(t, err)
	generation, err := native.CreateGeneration(ctx, "synthetic", 3, "")
	require.NoError(t, err)
	require.NoError(t, native.Upsert(ctx, generation, []vector.Chunk{{MessageID: message, Vector: []float32{1, 0, 0}}}))
	require.NoError(t, native.Close())
	ticks := make(chan time.Time)
	observed := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runConversationPolicyRuntime(ctx, st, currentConversationPolicySettingsSource(invocationFromContext(ctx)), ticks, func(err error) { observed <- err })
	}()
	ticks <- now.Add(24*time.Hour + time.Minute)
	select {
	case err := <-observed:
		require.NoError(t, err)
	case <-time.After(60 * time.Second):
		require.FailNow(t, "policy pass did not finish")
	}
	cancel()
	<-done
	native, err = sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, MainDB: st.DB(), PolicyStore: st, ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = native.Close() }()
	var count int
	require.NoError(t, native.DB().QueryRow(`SELECT COUNT(*) FROM embeddings`).Scan(&count))
	assert.Zero(t, count, "periodic production loop reaches native pruning with generation disabled and no credentials")
	_, pending, err := st.ConversationPolicyCleanupPending(t.Context())
	require.NoError(t, err)
	assert.False(t, pending)
}
