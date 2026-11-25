//go:build sqlite_vec

package cmd

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
	"go.kenn.io/msgvault/internal/vector/visual"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVisualColdPolicyRuntimeRetiresBeforeDeletingAbandonedPublication(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite native runtime fixture")
	f := storetest.New(t)
	ctx := t.Context()
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainDB: f.Store.DB(), PolicyStore: f.Store, Dimension: 4})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	seedVisualCleanupPublication(t, f, backend)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Vector.Multimodal.Enabled = true
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Model: voyage.DefaultModel, Dimension: 1024, Media: media.Policy{MaxBytes: 20 << 20, MaxPixels: 16_000_000, AllowStill: true, AllowVideo: true}})
	require.NoError(t, err)
	manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityQueryText)
	require.NoError(t, err)
	cfg.Vector.Multimodal.CapabilitiesFile = filepath.Join(t.TempDir(), "capabilities.json")
	require.NoError(t, writeVisualCapabilityManifest(cfg.Vector.Multimodal.CapabilitiesFile, manifest))
	ctx = testInvocationContext(ctx, cfg, invocationOptions{})
	_, err = newVisualRuntime(ctx, cfg.Vector, f.Store, backend, unavailableVisualCredentialOpener{}, visualRuntimeCredential{APIKey: "synthetic-test-key"})
	require.NoError(t, err)
	var count int
	require.NoError(t, backend.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM visual_vectors`).Scan(&count))
	assert.Zero(t, count, "factory retirement must delete the formerly current token")
}

func seedVisualCleanupPublication(t *testing.T, f *storetest.Fixture, backend *sqlitevec.Backend) (store.VisualGeneration, store.VisualOwner, string) {
	t.Helper()
	ctx := t.Context()
	generation, err := f.Store.EnsureVisualGeneration(ctx, store.VisualGenerationSpec{Fingerprint: "abandoned", Model: "voyage-multimodal-3.5", Dimension: 1024})
	require.NoError(t, err)
	consumer, _, err := f.Store.RegisterAttachmentChangeConsumer(ctx, "visual/abandoned")
	require.NoError(t, err)
	require.NoError(t, f.Store.CompleteAttachmentChangeReconciliation(ctx, consumer.ConsumerKey, consumer.BaselineSequence))
	message := f.CreateMessage("abandoned-visual")
	hash := strings.Repeat("c", 64)
	require.NoError(t, f.Store.UpsertAttachmentRecord(ctx, message, store.AttachmentWrite{Filename: "image.png", MIMEType: "image/png", ContentHash: hash, StoragePath: hash[:2] + "/" + hash, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "image"}))
	var attachment int64
	require.NoError(t, f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT id FROM attachments WHERE message_id=?`), message).Scan(&attachment))
	changes, err := f.Store.ListAttachmentChanges(ctx, consumer.ConsumerKey, 10)
	require.NoError(t, err)
	require.NotEmpty(t, changes)
	owner := store.VisualOwner{MessageID: message, BlobHash: hash, MediaInputKey: "original"}
	claim, acquired, err := f.Store.ClaimVisualWork(ctx, store.VisualClaimRequest{GenerationID: generation.ID, Owner: owner, ProposedRevision: "original", LeaseOwner: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, SourceFence: changes[len(changes)-1].Sequence})
	require.NoError(t, err)
	require.True(t, acquired)
	token, err := f.Store.PrepareVisualPublication(ctx, store.PreparedVisualPublication{Claim: claim, RepresentativeAttachmentID: attachment, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics})
	require.NoError(t, err)
	image := make([]float32, 1024)
	image[0] = 1
	require.NoError(t, backend.Visual().PutUnpublished(ctx, visual.VectorToken(token), image))
	require.NoError(t, f.Store.CommitVisualPublication(ctx, claim, token))
	return generation, owner, token
}

// Retirement can commit while deleting the abandoned generation's vectors
// fails. A later start has a new fingerprint, so it must still finish the old
// generation's cleanup and release its journal consumer.
func TestVisualRuntimeFinishesRetiredGenerationCleanupForOldFingerprint(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite native runtime fixture")
	f := storetest.New(t)
	ctx := t.Context()
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainDB: f.Store.DB(), PolicyStore: f.Store, Dimension: 4})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	generation, _, _ := seedVisualCleanupPublication(t, f, backend)
	// Simulate the failed earlier start: retired, vectors and consumer left.
	require.NoError(t, f.Store.RetireVisualGeneration(ctx, generation.ID))
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Vector.Multimodal.Enabled = true
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Model: voyage.DefaultModel, Dimension: 1024, Media: media.Policy{MaxBytes: 20 << 20, MaxPixels: 16_000_000, AllowStill: true, AllowVideo: true}})
	require.NoError(t, err)
	manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityQueryText)
	require.NoError(t, err)
	cfg.Vector.Multimodal.CapabilitiesFile = filepath.Join(t.TempDir(), "capabilities.json")
	require.NoError(t, writeVisualCapabilityManifest(cfg.Vector.Multimodal.CapabilitiesFile, manifest))
	ctx = testInvocationContext(ctx, cfg, invocationOptions{})
	_, err = newVisualRuntime(ctx, cfg.Vector, f.Store, backend, unavailableVisualCredentialOpener{}, visualRuntimeCredential{APIKey: "synthetic-test-key"})
	require.NoError(t, err)
	var count int
	require.NoError(t, backend.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM visual_vectors`).Scan(&count))
	assert.Zero(t, count, "the abandoned generation's vectors are deleted")
	_, err = f.Store.GetAttachmentChangeConsumer(ctx, "visual/abandoned")
	assert.Error(t, err, "the abandoned consumer no longer pins journal pruning")
}
