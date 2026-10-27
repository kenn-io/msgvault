//go:build sqlite_vec

package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/document"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
	"go.kenn.io/msgvault/internal/vector/visual"
	"path/filepath"
	"testing"
	"time"
)

func TestDocumentVisualColdPolicyNativeTokensWithOneConnection(t *testing.T) {
	f, gen := seedDocumentVectorGenerationWithChunks(t, 1)
	ctx := t.Context()
	var documents document.Backend
	var visuals visual.Backend
	if f.Store.IsPostgreSQL() {
		if !pgvector.Available() {
			t.Skip("PostgreSQL vector cleanup requires the pgvector-tagged test profile")
		}
		backend, err := pgvector.Open(ctx, pgvector.Options{DB: f.Store.DB(), PolicyStore: f.Store, Dimension: 768})
		require.NoError(t, err)
		documents, visuals = backend.DocumentBackend(), backend.Visual()
	} else {
		backend, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainDB: f.Store.DB(), PolicyStore: f.Store, Dimension: 768})
		require.NoError(t, err)
		t.Cleanup(func() { _ = backend.Close() })
		documents, visuals = backend.DocumentBackend(), backend.Visual()
	}
	f.Store.DB().SetMaxOpenConns(1)
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	original, err := f.Store.CaptureConversationPolicyWork(ctx)
	require.NoError(t, err)
	claim, err := f.Store.ClaimDocumentVectorChunk(original, gen.ID, 0, 10, "native-policy", time.Now().UTC(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	vector := make([]float32, 768)
	vector[0] = 1
	require.NoError(t, documents.PutUnpublished(original, document.GenerationID(gen.ID), 768, []document.Embedding{{Token: claim.Token, Vector: vector}}))
	visualGen := createVisualGeneration(t, f)
	owner, attachment, fence := createVisualOwner(t, f, "native-policy-visual")
	visualClaim := claimVisualOwner(t, f, visualGen.ID, owner, "revision", fence)
	token, err := f.Store.PrepareVisualPublication(original, store.PreparedVisualPublication{Claim: visualClaim, RepresentativeAttachmentID: attachment, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics})
	require.NoError(t, err)
	imageVector := make([]float32, 1024)
	imageVector[0] = 1
	require.NoError(t, visuals.PutUnpublished(original, visual.VectorToken(token), imageVector))
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	require.ErrorIs(t, documents.PutUnpublished(ctx, document.GenerationID(gen.ID), 768, []document.Embedding{{Token: claim.Token, Vector: vector}}), store.ErrConversationPolicyStale)
	require.ErrorIs(t, visuals.PutUnpublished(ctx, visual.VectorToken(token), imageVector), store.ErrConversationPolicyStale)
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	require.ErrorIs(t, documents.PutUnpublished(original, document.GenerationID(gen.ID), 768, []document.Embedding{{Token: claim.Token, Vector: vector}}), store.ErrConversationPolicyStale)
	require.ErrorIs(t, visuals.PutUnpublished(original, visual.VectorToken(token), imageVector), store.ErrConversationPolicyStale)
	// A content edit revokes pending visual tokens even with a fresh policy context.
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET subject='changed context',content_changed_at=? WHERE id=?`), time.Now().UTC(), owner.MessageID)
	require.NoError(t, err)
	require.ErrorIs(t, visuals.PutUnpublished(ctx, visual.VectorToken(token), imageVector), store.ErrConversationPolicyStale)
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	page, err := f.Store.ParkObsoleteDocumentVectorTokens(ctx, gen.ID, "", 10, time.Now().UTC().Add(2*time.Minute))
	require.NoError(t, err)
	require.Len(t, page.Tokens, 1)
	require.NoError(t, documents.DeleteTokens(ctx, document.GenerationID(gen.ID), []string{claim.Token}))
	finalized, err := f.Store.FinalizeObsoleteDocumentVectorToken(ctx, gen.ID, claim.Token, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, finalized)
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	fresh, err := f.Store.ClaimDocumentVectorChunk(ctx, gen.ID, 0, 10, "new-publication", time.Now().UTC(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, fresh)
	require.NotEqual(t, claim.Token, fresh.Token)
	require.NoError(t, documents.PutUnpublished(ctx, document.GenerationID(gen.ID), 768, []document.Embedding{{Token: fresh.Token, Vector: vector}}))
	require.ErrorIs(t, documents.PutUnpublished(ctx, document.GenerationID(gen.ID), 768, []document.Embedding{{Token: claim.Token, Vector: vector}}), store.ErrConversationPolicyStale)
	require.NoError(t, documents.DeleteTokens(ctx, document.GenerationID(gen.ID), []string{claim.Token}))
	hits, err := documents.Search(ctx, document.GenerationID(gen.ID), 768, vector, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, fresh.Token, hits[0].Token, "old token cleanup cannot delete a promoted publication")
}
