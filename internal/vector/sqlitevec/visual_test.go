//go:build sqlite_vec

package sqlitevec

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func TestVisualBackendPublishesOnlyCurrentLiveTokens(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversation(source.ID, "visual-backend", "Visual backend")
	require.NoError(t, err)
	f := &storetest.Fixture{T: t, Store: st, Source: source, ConvID: conversationID}
	generation, owner, token := publishVisualVectorOwner(t, f)
	backend, err := Open(t.Context(), Options{
		Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 768,
		MainDB: f.Store.DB(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	visualBackend := backend.Visual()

	query := unitVec(visualDimension, 0)
	require.NoError(t, visualBackend.PutUnpublished(t.Context(), visual.VectorToken(token), query))
	require.NoError(t, f.Store.ConsentVisualGeneration(t.Context(), generation.ID, "synthetic-policy-fingerprint"))
	highWater, err := f.Store.AttachmentChangeHighWater(t.Context())
	require.NoError(t, err)
	require.NoError(t, f.Store.AdvanceVisualGenerationSourceFence(t.Context(), generation.ID, highWater))
	_, activateErr := f.Store.ActivateVisualGeneration(t.Context(), generation.ID, highWater)
	require.NoError(t, activateErr)
	hits, err := visualBackend.Search(t.Context(), visual.SearchRequest{
		GenerationID: visual.GenerationID(generation.ID), Vector: query, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, visual.VectorToken(token), hits[0].Token)
	assert.Equal(t, 1, hits[0].Rank)
	matchingParticipant := f.EnsureParticipant("matching-visual@example.test", "Matching", "example.test")
	otherParticipant := f.EnsureParticipant("other-visual@example.test", "Other", "example.test")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sender_id = ? WHERE id = ?`), matchingParticipant, owner.MessageID)
	require.NoError(t, err)
	personRequest := visual.SearchRequest{
		GenerationID: visual.GenerationID(generation.ID), Vector: query, Limit: 10,
		Person: &personscope.Scope{
			ParticipantIDs: []int64{matchingParticipant},
			Directions:     []personscope.Direction{personscope.FromPerson},
		},
	}
	hits, err = visualBackend.Search(t.Context(), personRequest)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	personRequest.Person.ParticipantIDs = []int64{otherParticipant}
	hits, err = visualBackend.Search(t.Context(), personRequest)
	require.NoError(t, err)
	assert.Empty(t, hits, "person scope must filter before the nearest-neighbor result window")

	loaded, err := visualBackend.LoadOwnerVector(t.Context(), visual.GenerationID(generation.ID), visual.Owner{
		MessageID: owner.MessageID, BlobHash: owner.BlobHash, MediaInputKey: owner.MediaInputKey,
	})
	require.NoError(t, err)
	assert.Equal(t, query, loaded)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), owner.MessageID)
	require.NoError(t, err)
	hits, err = visualBackend.Search(t.Context(), visual.SearchRequest{
		GenerationID: visual.GenerationID(generation.ID), Vector: query, Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, hits)
}

func TestVisualBackendRejectsWrongDimension(t *testing.T) {
	backend, err := Open(t.Context(), Options{Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 768})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	assert.Error(t, backend.Visual().PutUnpublished(t.Context(), "token", []float32{1}))
}

func publishVisualVectorOwner(t *testing.T, f *storetest.Fixture) (store.VisualGeneration, store.VisualOwner, string) {
	t.Helper()
	consumer, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), "visual-backend-test")
	require.NoError(t, err)
	require.NoError(t, f.Store.CompleteAttachmentChangeReconciliation(t.Context(), consumer.ConsumerKey, consumer.BaselineSequence))
	messageID := f.CreateMessage("visual-backend")
	hash := strings.Repeat("b", 64)
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "image.png", MIMEType: "image/png", ContentHash: hash,
		StoragePath: hash[:2] + "/" + hash, Size: 10, SourcePartKey: "part:1",
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}))
	var attachmentID int64
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM attachments WHERE message_id = ?`), messageID).Scan(&attachmentID))
	highWater, err := f.Store.AttachmentChangeHighWater(t.Context())
	require.NoError(t, err)
	generation, err := f.Store.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{
		Fingerprint: "visual-backend-v1", Model: "voyage-multimodal-3.5", Dimension: visualDimension,
	})
	require.NoError(t, err)
	owner := store.VisualOwner{MessageID: messageID, BlobHash: hash, MediaInputKey: store.VisualOriginalMediaInputKey}
	claim, acquired, err := f.Store.ClaimVisualWork(t.Context(), store.VisualClaimRequest{
		GenerationID: generation.ID, Owner: owner, ProposedRevision: "revision",
		LeaseOwner: "test", Now: time.Now().UTC(), LeaseDuration: time.Minute, SourceFence: highWater,
	})
	require.NoError(t, err)
	require.True(t, acquired)
	token, err := f.Store.PrepareVisualPublication(t.Context(), store.PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(t, err)
	require.NoError(t, f.Store.CommitVisualPublication(t.Context(), claim, token))
	return generation, owner, token
}

func TestOpenWithoutTextDimensionSupportsMultimodalOnly(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversation(source.ID, "visual-only", "Visual only")
	require.NoError(t, err)
	f := &storetest.Fixture{T: t, Store: st, Source: source, ConvID: conversationID}
	generation, _, token := publishVisualVectorOwner(t, f)

	// A multimodal-only configuration has no text embedding dimension:
	// Open must still migrate and serve the visual backend.
	backend, err := Open(t.Context(), Options{
		Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 0,
		MainDB: f.Store.DB(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	visualBackend := backend.Visual()

	query := unitVec(visualDimension, 0)
	require.NoError(t, visualBackend.PutUnpublished(t.Context(), visual.VectorToken(token), query))
	require.NoError(t, f.Store.ConsentVisualGeneration(t.Context(), generation.ID, "synthetic-policy-fingerprint"))
	highWater, err := f.Store.AttachmentChangeHighWater(t.Context())
	require.NoError(t, err)
	require.NoError(t, f.Store.AdvanceVisualGenerationSourceFence(t.Context(), generation.ID, highWater))
	_, activateErr := f.Store.ActivateVisualGeneration(t.Context(), generation.ID, highWater)
	require.NoError(t, activateErr)
	hits, err := visualBackend.Search(t.Context(), visual.SearchRequest{
		GenerationID: visual.GenerationID(generation.ID), Vector: query, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, visual.VectorToken(token), hits[0].Token)
}

func TestVisualSearchWidensPastDeadVectorClusters(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversation(source.ID, "visual-widen", "Visual widen")
	require.NoError(t, err)
	f := &storetest.Fixture{T: t, Store: st, Source: source, ConvID: conversationID}
	generation, _, token := publishVisualVectorOwner(t, f)
	backend, err := Open(t.Context(), Options{
		Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 0,
		MainDB: f.Store.DB(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	visualBackend := backend.Visual()

	// 70 unpublished (dead) vectors sit exactly on the query point, filling
	// the first bounded nearest-neighbor batch entirely; the one live vector
	// is farther away. The search must widen k progressively instead of
	// returning empty or streaming the whole store up front.
	query := unitVec(visualDimension, 0)
	for i := range 70 {
		dead := make([]float32, visualDimension)
		copy(dead, query)
		require.NoError(t, visualBackend.PutUnpublished(t.Context(),
			visual.VectorToken(fmt.Sprintf("dead-token-%02d", i)), dead))
	}
	liveVector := make([]float32, visualDimension)
	liveVector[0], liveVector[1] = 0.8, 0.6
	require.NoError(t, visualBackend.PutUnpublished(t.Context(), visual.VectorToken(token), liveVector))
	require.NoError(t, f.Store.ConsentVisualGeneration(t.Context(), generation.ID, "synthetic-policy-fingerprint"))
	highWater, err := f.Store.AttachmentChangeHighWater(t.Context())
	require.NoError(t, err)
	require.NoError(t, f.Store.AdvanceVisualGenerationSourceFence(t.Context(), generation.ID, highWater))
	_, activateErr := f.Store.ActivateVisualGeneration(t.Context(), generation.ID, highWater)
	require.NoError(t, activateErr)

	hits, err := visualBackend.Search(t.Context(), visual.SearchRequest{
		GenerationID: visual.GenerationID(generation.ID), Vector: query, Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, visual.VectorToken(token), hits[0].Token)
}

func TestVisualPersonFilterCoversLegacyFromRecipientSenders(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversation(source.ID, "visual-legacy-sender", "Legacy sender")
	require.NoError(t, err)
	f := &storetest.Fixture{T: t, Store: st, Source: source, ConvID: conversationID}
	generation, owner, token := publishVisualVectorOwner(t, f)

	// Legacy import shape: sender_id is NULL and the sender exists only as
	// a 'from' recipient row.
	participantID, err := st.EnsureParticipant("legacy-sender@example.com", "Legacy Sender", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET sender_id = NULL WHERE id = ?`), owner.MessageID)
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO message_recipients (message_id, participant_id, recipient_type)
		VALUES (?, ?, 'from')`), owner.MessageID, participantID)
	require.NoError(t, err)

	backend, err := Open(t.Context(), Options{
		Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 0,
		MainDB: st.DB(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	visualBackend := backend.Visual()
	query := unitVec(visualDimension, 0)
	require.NoError(t, visualBackend.PutUnpublished(t.Context(), visual.VectorToken(token), query))
	require.NoError(t, st.ConsentVisualGeneration(t.Context(), generation.ID, "synthetic-policy-fingerprint"))
	highWater, err := st.AttachmentChangeHighWater(t.Context())
	require.NoError(t, err)
	require.NoError(t, st.AdvanceVisualGenerationSourceFence(t.Context(), generation.ID, highWater))
	_, activateErr := st.ActivateVisualGeneration(t.Context(), generation.ID, highWater)
	require.NoError(t, activateErr)

	hits, err := visualBackend.Search(t.Context(), visual.SearchRequest{
		GenerationID: visual.GenerationID(generation.ID), Vector: query, Limit: 10,
		Person: &personscope.Scope{
			ParticipantIDs: person.ParticipantIDs,
			Directions:     []personscope.Direction{personscope.FromPerson},
		},
	})
	require.NoError(t, err)
	require.Len(t, hits, 1, "the from-recipient fallback must match legacy senders")
	assert.Equal(t, visual.VectorToken(token), hits[0].Token)
}
