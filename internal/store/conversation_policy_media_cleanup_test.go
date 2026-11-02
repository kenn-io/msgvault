//go:build sqlite_vec

package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector/document"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func mediaCleanupBackends(t *testing.T, f *storetest.Fixture) (document.Backend, visual.Backend, *sql.DB) {
	t.Helper()
	if f.Store.IsPostgreSQL() {
		if !pgvector.Available() {
			t.Skip("PostgreSQL vector cleanup requires the pgvector-tagged test profile")
		}
		b, err := pgvector.Open(t.Context(), pgvector.Options{DB: f.Store.DB(), PolicyStore: f.Store, Dimension: 768})
		require.NoError(t, err)
		return b.DocumentBackend(), b.Visual(), b.DB()
	}
	b, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainDB: f.Store.DB(), PolicyStore: f.Store, Dimension: 768})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	return b.DocumentBackend(), b.Visual(), b.DB()
}

func TestDocumentVisualColdPolicyCleanupAllGenerations(t *testing.T) {
	f, first := seedDocumentVectorGenerationWithChunks(t, 1)
	docs, images, native := mediaCleanupBackends(t, f)
	f.Store.DB().SetMaxOpenConns(1)
	native.SetMaxOpenConns(1)
	ctx := t.Context()
	vector := make([]float32, 768)
	vector[0] = 1
	image := make([]float32, 1024)
	image[0] = 1
	old, err := f.Store.ClaimDocumentVectorChunk(ctx, first.ID, 0, 10, "old", time.Now().UTC(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, old)
	require.NoError(t, docs.PutUnpublished(ctx, document.GenerationID(first.ID), 768, []document.Embedding{{Token: old.Token, Vector: vector}}))
	require.NoError(t, f.Store.CommitDocumentVectorPublication(ctx, first.ID, old.Token, old.LeaseOwner, old.LeaseFence, time.Now().UTC()))
	_, err = f.Store.RetireDocumentVectorGeneration(ctx, first.ID, time.Now().UTC())
	require.NoError(t, err)
	spec := first.DocumentVectorGenerationSpec
	spec.Fingerprint = strings.Repeat("e", 64)
	current, _, err := f.Store.EnsureDocumentVectorGeneration(ctx, spec)
	require.NoError(t, err)
	pending, err := f.Store.ClaimDocumentVectorChunk(ctx, current.ID, 0, 10, "pending", time.Now().UTC(), time.Hour)
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.NoError(t, docs.PutUnpublished(ctx, document.GenerationID(current.ID), 768, []document.Embedding{{Token: pending.Token, Vector: vector}}))
	oldVisual := createVisualGeneration(t, f)
	owner, attachment, fence := createVisualOwner(t, f, "cleanup-visual")
	claim := claimVisualOwner(t, f, oldVisual.ID, owner, "old", fence)
	token, err := f.Store.PrepareVisualPublication(ctx, store.PreparedVisualPublication{Claim: claim, RepresentativeAttachmentID: attachment, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics})
	require.NoError(t, err)
	require.NoError(t, images.PutUnpublished(ctx, visual.VectorToken(token), image))
	require.NoError(t, f.Store.CommitVisualPublication(ctx, claim, token))
	require.NoError(t, f.Store.RetireVisualGeneration(ctx, oldVisual.ID))
	liveVisual, err := f.Store.EnsureVisualGeneration(ctx, store.VisualGenerationSpec{Fingerprint: "new-cleanup-generation", Model: "voyage-multimodal-3.5", Dimension: 1024})
	require.NoError(t, err)
	liveClaim := claimVisualOwner(t, f, liveVisual.ID, owner, "pending", fence)
	pendingVisual, err := f.Store.PrepareVisualPublication(ctx, store.PreparedVisualPublication{Claim: liveClaim, RepresentativeAttachmentID: attachment, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics})
	require.NoError(t, err)
	require.NoError(t, images.PutUnpublished(ctx, visual.VectorToken(pendingVisual), image))
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	docPruner, ok := docs.(interface {
		PruneIneligibleDocuments(ctx context.Context) (int64, error)
	})
	require.True(t, ok, "document backend must reconcile all generations")
	visualPruner, ok := images.(interface {
		PruneIneligibleVisuals(ctx context.Context) (int64, error)
	})
	require.True(t, ok, "visual backend must reconcile all generations")
	n, err := docPruner.PruneIneligibleDocuments(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	if f.Store.IsPostgreSQL() {
		_, err = native.ExecContext(ctx, `CREATE FUNCTION fail_visual_policy_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected visual deletion failure'; END $$`)
		require.NoError(t, err)
		_, err = native.ExecContext(ctx, `CREATE TRIGGER fail_visual_policy_delete BEFORE DELETE ON visual_vectors FOR EACH ROW EXECUTE FUNCTION fail_visual_policy_delete()`)
		require.NoError(t, err)
	} else {
		_, err = native.ExecContext(ctx, `CREATE TRIGGER fail_visual_policy_delete BEFORE DELETE ON visual_vectors BEGIN SELECT RAISE(FAIL,'injected visual deletion failure'); END`)
		require.NoError(t, err)
	}
	_, err = visualPruner.PruneIneligibleVisuals(ctx)
	require.ErrorContains(t, err, "injected visual deletion failure")
	drop := `DROP TRIGGER fail_visual_policy_delete`
	if f.Store.IsPostgreSQL() {
		drop += ` ON visual_vectors`
	}
	_, err = native.ExecContext(ctx, drop)
	require.NoError(t, err)
	n, err = visualPruner.PruneIneligibleVisuals(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	var count int
	require.NoError(t, native.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_vector_embeddings`).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, native.QueryRowContext(ctx, `SELECT COUNT(*) FROM visual_vectors`).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	fresh, err := f.Store.ClaimDocumentVectorChunk(ctx, current.ID, 0, 10, "fresh", time.Now().UTC(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, fresh)
	assert.NotEqual(t, pending.Token, fresh.Token)
	require.ErrorIs(t, docs.PutUnpublished(ctx, document.GenerationID(current.ID), 768, []document.Embedding{{Token: pending.Token, Vector: vector}}), store.ErrConversationPolicyStale)
	require.ErrorIs(t, images.PutUnpublished(ctx, visual.VectorToken(pendingVisual), image), store.ErrConversationPolicyStale)
	require.NoError(t, docs.PutUnpublished(ctx, document.GenerationID(current.ID), 768, []document.Embedding{{Token: fresh.Token, Vector: vector}}))
	n, err = docPruner.PruneIneligibleDocuments(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "fresh Full publication survives old cleanup retry")
}

func TestDocumentVisualColdPolicyDeletionPreservesCurrentAndAllowsAbort(t *testing.T) {
	f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
	docs, images, native := mediaCleanupBackends(t, f)
	f.Store.DB().SetMaxOpenConns(1)
	native.SetMaxOpenConns(1)
	ctx := t.Context()
	vector := make([]float32, 768)
	vector[0] = 1
	claim, err := f.Store.ClaimDocumentVectorChunk(ctx, generation.ID, 0, 10, "worker", time.Now().UTC(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	put := func() {
		require.NoError(t, docs.PutUnpublished(ctx, document.GenerationID(generation.ID), 768, []document.Embedding{{Token: claim.Token, Vector: vector}}))
	}
	count := func(table string) int {
		var n int
		require.NoError(t, native.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n))
		return n
	}
	put()
	require.NoError(t, docs.DeleteTokens(ctx, document.GenerationID(generation.ID), []string{claim.Token}))
	assert.Zero(t, count("document_vector_embeddings"), "unpublished abort remains deletable")
	put()
	require.NoError(t, f.Store.CommitDocumentVectorPublication(ctx, generation.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, time.Now().UTC()))
	require.NoError(t, docs.DeleteTokens(ctx, document.GenerationID(generation.ID), []string{claim.Token}))
	assert.Equal(t, 1, count("document_vector_embeddings"), "stale deletion hint cannot remove current publication")
	_, err = f.Store.RetireDocumentVectorGeneration(ctx, generation.ID, time.Now().UTC())
	require.NoError(t, err)
	require.NoError(t, docs.DeleteTokens(ctx, document.GenerationID(generation.ID), []string{claim.Token}))
	assert.Zero(t, count("document_vector_embeddings"))
	vg := createVisualGeneration(t, f)
	owner, attachment, fence := createVisualOwner(t, f, "restored-cleanup")
	vc := claimVisualOwner(t, f, vg.ID, owner, "same-revision", fence)
	prepared := store.PreparedVisualPublication{Claim: vc, RepresentativeAttachmentID: attachment, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}
	token, err := f.Store.PrepareVisualPublication(ctx, prepared)
	require.NoError(t, err)
	image := make([]float32, 1024)
	image[0] = 1
	require.NoError(t, images.PutUnpublished(ctx, visual.VectorToken(token), image))
	require.NoError(t, images.DeleteTokens(ctx, []visual.VectorToken{visual.VectorToken(token)}))
	assert.Zero(t, count("visual_vectors"), "unpublished rollback remains deletable")
	require.NoError(t, images.PutUnpublished(ctx, visual.VectorToken(token), image))
	require.NoError(t, f.Store.CommitVisualPublication(ctx, vc, token))
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	require.NoError(t, f.Store.TombstoneVisualOwner(ctx, vg.ID, owner, fence))
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	prepared.Claim = claimVisualOwner(t, f, vg.ID, owner, "same-revision", fence)
	require.NoError(t, f.Store.RestoreVisualPublication(ctx, prepared, token))
	require.NoError(t, images.DeleteTokens(ctx, []visual.VectorToken{visual.VectorToken(token)}))
	assert.Equal(t, 1, count("visual_vectors"), "pre-promotion cleanup hint cannot remove restored current publication")
	require.NoError(t, f.Store.RetireVisualGeneration(ctx, vg.ID))
	require.NoError(t, images.DeleteTokens(ctx, []visual.VectorToken{visual.VectorToken(token)}))
	assert.Zero(t, count("visual_vectors"))
}

func TestDocumentColdPolicyCleanupSharedSupportPromotionAndRetry(t *testing.T) {
	f, gen := seedDocumentVectorGenerationWithChunks(t, 1)
	docs, _, native := mediaCleanupBackends(t, f)
	ctx := t.Context()
	f.Store.DB().SetMaxOpenConns(1)
	native.SetMaxOpenConns(1)
	candidates, err := f.Store.ListDocumentVectorChunkCandidates(ctx, gen.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	hash := candidates[0].CanonicalBlobHash
	room, err := f.Store.EnsureConversation(f.Source.ID, "surviving-room", "Surviving room")
	require.NoError(t, err)
	f.ConvID = room
	message := f.CreateMessage("surviving-document")
	require.NoError(t, f.Store.UpsertAttachmentRecord(ctx, message, store.AttachmentWrite{Filename: "shared.pdf", MIMEType: "application/pdf", Size: 128, StoragePath: hash[:2] + "/" + hash, ContentHash: hash, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceMIMEDisposition, SourcePartKey: "mime:1"}))
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(ctx, singleAttachmentID(t, f, message), 2)
	require.NoError(t, err)
	require.True(t, eligible)
	claim, err := f.Store.ClaimDocumentVectorChunk(ctx, gen.ID, 0, 10, "worker", time.Now().UTC(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	v := make([]float32, 768)
	v[0] = 1
	require.NoError(t, docs.PutUnpublished(ctx, document.GenerationID(gen.ID), 768, []document.Embedding{{Token: claim.Token, Vector: v}}))
	require.NoError(t, f.Store.CommitDocumentVectorPublication(ctx, gen.ID, claim.Token, claim.LeaseOwner, claim.LeaseFence, time.Now().UTC()))
	pruner, ok := docs.(interface {
		PruneIneligibleDocuments(ctx context.Context) (int64, error)
	})
	require.True(t, ok, "document backend must reconcile all generations")
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	n, err := pruner.PruneIneligibleDocuments(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "surviving Full owner retains physical shared vector")
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "surviving-room", conversationpolicy.Cold))
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "surviving-room", conversationpolicy.Full))
	n, err = pruner.PruneIneligibleDocuments(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "cleanup retains the shared vector after the surviving room is promoted")
	var count int
	require.NoError(t, native.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_vector_embeddings`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "surviving-room", conversationpolicy.Cold))
	if f.Store.IsPostgreSQL() {
		_, err = native.ExecContext(ctx, `CREATE FUNCTION fail_policy_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected deletion failure'; END $$`)
		require.NoError(t, err)
		_, err = native.ExecContext(ctx, `CREATE TRIGGER fail_policy_delete BEFORE DELETE ON document_vector_embeddings FOR EACH ROW EXECUTE FUNCTION fail_policy_delete()`)
		require.NoError(t, err)
	} else {
		_, err = native.ExecContext(ctx, `CREATE TRIGGER fail_policy_delete BEFORE DELETE ON document_vector_embeddings BEGIN SELECT RAISE(FAIL,'injected deletion failure'); END`)
		require.NoError(t, err)
	}
	_, err = pruner.PruneIneligibleDocuments(ctx)
	require.ErrorContains(t, err, "injected deletion failure")
	require.NoError(t, native.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_vector_embeddings`).Scan(&count))
	assert.Equal(t, 1, count)
	drop := `DROP TRIGGER fail_policy_delete`
	if f.Store.IsPostgreSQL() {
		drop += ` ON document_vector_embeddings`
	}
	_, err = native.ExecContext(ctx, drop)
	require.NoError(t, err)
	n, err = pruner.PruneIneligibleDocuments(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "revocation survives failed native deletion and retries")
	require.NoError(t, f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM document_chunks`).Scan(&count))
	assert.Equal(t, 1, count, "cleanup keeps extracted text")
}
