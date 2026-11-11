//go:build sqlite_vec

package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func mediaCleanupBackends(t *testing.T, f *storetest.Fixture) (visual.Backend, *sql.DB) {
	t.Helper()
	if f.Store.IsPostgreSQL() {
		if !pgvector.Available() {
			t.Skip("PostgreSQL vector cleanup requires the pgvector-tagged test profile")
		}
		b, err := pgvector.Open(t.Context(), pgvector.Options{DB: f.Store.DB(), PolicyStore: f.Store, Dimension: 768})
		require.NoError(t, err)
		return b.Visual(), b.DB()
	}
	b, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainDB: f.Store.DB(), PolicyStore: f.Store, Dimension: 768})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	return b.Visual(), b.DB()
}

func TestVisualColdPolicyCleanupAllGenerations(t *testing.T) {
	f := storetest.New(t)
	images, native := mediaCleanupBackends(t, f)
	f.Store.DB().SetMaxOpenConns(1)
	native.SetMaxOpenConns(1)
	ctx := t.Context()
	image := make([]float32, 1024)
	image[0] = 1
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
	visualPruner, ok := images.(interface {
		PruneIneligibleVisuals(ctx context.Context) (int64, error)
	})
	require.True(t, ok, "visual backend must reconcile all generations")
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
	n, err := visualPruner.PruneIneligibleVisuals(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	var count int
	require.NoError(t, native.QueryRowContext(ctx, `SELECT COUNT(*) FROM visual_vectors`).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	require.ErrorIs(t, images.PutUnpublished(ctx, visual.VectorToken(pendingVisual), image), store.ErrConversationPolicyStale)
}

func TestVisualColdPolicyDeletionPreservesCurrentAndAllowsAbort(t *testing.T) {
	f := storetest.New(t)
	images, native := mediaCleanupBackends(t, f)
	f.Store.DB().SetMaxOpenConns(1)
	native.SetMaxOpenConns(1)
	ctx := t.Context()
	count := func(table string) int {
		var n int
		require.NoError(t, native.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n))
		return n
	}
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
