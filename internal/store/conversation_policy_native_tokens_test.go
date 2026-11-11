//go:build sqlite_vec

package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func TestVisualColdPolicyNativeTokensWithOneConnection(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	var visuals visual.Backend
	if f.Store.IsPostgreSQL() {
		if !pgvector.Available() {
			t.Skip("PostgreSQL vector cleanup requires the pgvector-tagged test profile")
		}
		backend, err := pgvector.Open(ctx, pgvector.Options{DB: f.Store.DB(), PolicyStore: f.Store, Dimension: 768})
		require.NoError(t, err)
		visuals = backend.Visual()
	} else {
		backend, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainDB: f.Store.DB(), PolicyStore: f.Store, Dimension: 768})
		require.NoError(t, err)
		t.Cleanup(func() { _ = backend.Close() })
		visuals = backend.Visual()
	}
	f.Store.DB().SetMaxOpenConns(1)
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	original, err := f.Store.CaptureConversationPolicyWork(ctx)
	require.NoError(t, err)
	visualGen := createVisualGeneration(t, f)
	owner, attachment, fence := createVisualOwner(t, f, "native-policy-visual")
	visualClaim := claimVisualOwner(t, f, visualGen.ID, owner, "revision", fence)
	token, err := f.Store.PrepareVisualPublication(original, store.PreparedVisualPublication{Claim: visualClaim, RepresentativeAttachmentID: attachment, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics})
	require.NoError(t, err)
	imageVector := make([]float32, 1024)
	imageVector[0] = 1
	require.NoError(t, visuals.PutUnpublished(original, visual.VectorToken(token), imageVector))
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	require.ErrorIs(t, visuals.PutUnpublished(ctx, visual.VectorToken(token), imageVector), store.ErrConversationPolicyStale)
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	require.ErrorIs(t, visuals.PutUnpublished(original, visual.VectorToken(token), imageVector), store.ErrConversationPolicyStale)
	// A content edit revokes pending visual tokens even with a fresh policy context.
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET subject='changed context',content_changed_at=? WHERE id=?`), time.Now().UTC(), owner.MessageID)
	require.NoError(t, err)
	require.ErrorIs(t, visuals.PutUnpublished(ctx, visual.VectorToken(token), imageVector), store.ErrConversationPolicyStale)
}
