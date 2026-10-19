//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

func newPolicyBackend(t *testing.T) (*Backend, *store.Store, int64, int64, vector.GenerationID) {
	t.Helper()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	st, err := store.OpenForTest(mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	src, err := st.GetOrCreateSource("beeper", "native-policy")
	require.NoError(t, err)
	room, err := st.EnsureConversation(src.ID, "room", "Room")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper", Subject: sql.NullString{String: "policyneedle", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.UpsertFTS(id, "policyneedle", "", "", "", ""))
	b, err := Open(t.Context(), Options{PolicyStore: st, Path: filepath.Join(dir, "vectors.db"), MainPath: mainPath, MainDB: st.DB(), Dimension: 4})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	gen, err := b.CreateGeneration(t.Context(), "policy-test", 4, "")
	require.NoError(t, err)
	return b, st, src.ID, id, gen
}

func policyChunk(t *testing.T, st *store.Store, messageID int64, values []float32) vector.Chunk {
	t.Helper()
	var revision int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT embedding_revision FROM messages WHERE id=?`), messageID).Scan(&revision))
	return vector.Chunk{MessageID: messageID, Vector: values, EmbeddingRevision: revision}
}

func TestBackendColdPolicySearchUpsertAndPrune(t *testing.T) {
	b, st, sourceID, id, gen := newPolicyBackend(t)
	require.ErrorIs(t, b.Upsert(t.Context(), gen, []vector.Chunk{{MessageID: id, Vector: []float32{1, 0, 0, 0}}}), store.ErrConversationPolicyStale)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), sourceID, "room", conversationpolicy.Full))
	admitted, err := st.CaptureConversationPolicyWork(t.Context())
	require.NoError(t, err)
	chunks := []vector.Chunk{policyChunk(t, st, id, []float32{1, 0, 0, 0})}
	require.NoError(t, b.Upsert(admitted, gen, chunks))
	scopes := []vector.DocumentScopePublication{{ScopeKey: "policy-scope", SourceSequence: 1, Documents: []vector.DocumentPublication{{Key: "policy-doc", Kind: "message", Revision: "one", SourceSequence: 1, Members: []int64{id}}}, Chunks: chunks}}
	require.NoError(t, b.PublishScopes(admitted, gen, scopes))
	fused, _, err := b.FusedSearch(t.Context(), vector.FusedRequest{Generation: gen, FTSTerms: []string{"policyneedle"}, KPerSignal: 10, Limit: 10, RRFK: 60})
	require.NoError(t, err)
	require.Len(t, fused, 1)

	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), sourceID, "room", conversationpolicy.Cold))
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), sourceID, "room", conversationpolicy.Full))
	require.ErrorIs(t, b.Upsert(admitted, gen, chunks), store.ErrConversationPolicyStale)
	require.ErrorIs(t, b.PublishScopes(admitted, gen, scopes), store.ErrConversationPolicyStale)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), sourceID, "room", conversationpolicy.Cold))
	hits, err := b.Search(t.Context(), gen, []float32{1, 0, 0, 0}, 10, vector.Filter{})
	require.NoError(t, err)
	assert.Empty(t, hits, "cold vectors are hidden before physical cleanup")
	fused, _, err = b.FusedSearch(t.Context(), vector.FusedRequest{Generation: gen, FTSTerms: []string{"policyneedle"}, KPerSignal: 10, Limit: 10, RRFK: 60})
	require.NoError(t, err)
	assert.Empty(t, fused)

	pruner, ok := any(b).(interface {
		PruneIneligibleEmbeddings(ctx context.Context) (int64, error)
	})
	require.True(t, ok, "native backend must reconcile current policy")
	n, err := pruner.PruneIneligibleEmbeddings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	record, err := b.GetDocument(t.Context(), gen, "policy-doc")
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, record.State)
	assert.Empty(t, record.Members)
	stats, err := b.Stats(t.Context(), gen)
	require.NoError(t, err)
	assert.Zero(t, stats.EmbeddingCount)
}

func TestLoadVectorRejectsColdConversationSeed(t *testing.T) {
	b, st, sourceID, id, gen := newPolicyBackend(t)
	ctx := t.Context()
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Full))
	chunks := []vector.Chunk{policyChunk(t, st, id, []float32{1, 0, 0, 0})}
	require.NoError(t, b.Upsert(ctx, gen, chunks))
	require.NoError(t, b.ActivateGeneration(ctx, gen, true))
	got, err := b.LoadVector(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, chunks[0].Vector, got)

	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Cold))
	_, err = b.LoadVector(ctx, id)
	require.ErrorIs(t, err, store.ErrConversationPolicyStale)
}

func TestContextualVectorConsumersHideUnstampedEmbeddings(t *testing.T) {
	b, st, sourceID, id, gen := newPolicyBackend(t)
	ctx := t.Context()
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Full))
	chunks := []vector.Chunk{policyChunk(t, st, id, []float32{1, 0, 0, 0})}
	require.NoError(t, b.Upsert(ctx, gen, chunks))
	require.NoError(t, st.SetEmbedGen(ctx, []int64{id}, int64(gen)))
	require.NoError(t, b.ActivateGeneration(ctx, gen, true))

	loaded, err := b.LoadVector(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, chunks[0].Vector, loaded)
	hits, err := b.Search(ctx, gen, chunks[0].Vector, 10, vector.Filter{})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	chunksScored, err := b.ScoreMessageChunks(ctx, gen, id, chunks[0].Vector)
	require.NoError(t, err)
	assert.Len(t, chunksScored, 1)

	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE messages SET embed_gen=NULL, embedding_revision=embedding_revision+1 WHERE id=?`), id)
	require.NoError(t, err)
	_, err = b.LoadVector(ctx, id)
	require.Error(t, err, "a stale embedding cannot seed a contextual query")
	hits, err = b.Search(ctx, gen, chunks[0].Vector, 10, vector.Filter{})
	require.NoError(t, err)
	assert.Empty(t, hits, "an unstamped embedding cannot appear in vector search")
	chunksScored, err = b.ScoreMessageChunks(ctx, gen, id, chunks[0].Vector)
	require.NoError(t, err)
	assert.Empty(t, chunksScored)

	fused, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		Generation: gen, QueryVec: chunks[0].Vector, FTSTerms: []string{"policyneedle"},
		KPerSignal: 10, Limit: 10, RRFK: 60,
	})
	require.NoError(t, err)
	require.Len(t, fused, 1)
	assert.True(t, math.IsNaN(fused[0].VectorScore), "the lexical leg remains available without a current vector")
	fused, _, err = b.FusedSearch(ctx, vector.FusedRequest{
		Generation: gen, QueryVec: chunks[0].Vector, KPerSignal: 10, Limit: 10, RRFK: 60,
	})
	require.NoError(t, err)
	assert.Empty(t, fused, "a vector-only fused query excludes stale embeddings")
}

func TestActiveGenerationVectorsSurvivePartialSameModelRebuild(t *testing.T) {
	b, st, sourceID, firstID, active := newPolicyBackend(t)
	ctx := t.Context()
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Full))
	room, err := st.EnsureConversation(sourceID, "room", "Room")
	require.NoError(t, err)
	secondID, err := st.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: room, SourceMessageID: "second", MessageType: "beeper", Subject: sql.NullString{String: "second policy message", Valid: true}})
	require.NoError(t, err)
	firstChunks := []vector.Chunk{policyChunk(t, st, firstID, []float32{1, 0, 0, 0})}
	secondChunks := []vector.Chunk{policyChunk(t, st, secondID, []float32{0, 1, 0, 0})}
	require.NoError(t, b.Upsert(ctx, active, firstChunks))
	require.NoError(t, b.Upsert(ctx, active, secondChunks))
	require.NoError(t, st.SetEmbedGen(ctx, []int64{firstID, secondID}, int64(active)))
	require.NoError(t, b.ActivateGeneration(ctx, active, true))

	building, err := b.CreateGeneration(ctx, "policy-test", 4, "")
	require.NoError(t, err)
	require.NoError(t, b.Upsert(ctx, building, firstChunks))
	require.NoError(t, st.SetEmbedGen(ctx, []int64{firstID}, int64(building)))

	loaded, err := b.LoadVector(ctx, firstID)
	require.NoError(t, err)
	assert.Equal(t, firstChunks[0].Vector, loaded)
	hits, err := b.Search(ctx, active, []float32{1, 0, 0, 0}, 10, vector.Filter{})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.ElementsMatch(t, []int64{firstID, secondID}, []int64{hits[0].MessageID, hits[1].MessageID})
}

func TestColdPolicyCleanupRetainsRepublishedPromotion(t *testing.T) {
	b, st, sourceID, id, gen := newPolicyBackend(t)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), sourceID, "room", conversationpolicy.Full))
	chunks := []vector.Chunk{policyChunk(t, st, id, []float32{1, 0, 0, 0})}
	require.NoError(t, b.Upsert(t.Context(), gen, chunks))
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), sourceID, "room", conversationpolicy.Cold))
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), sourceID, "room", conversationpolicy.Full))
	chunks = []vector.Chunk{policyChunk(t, st, id, []float32{1, 0, 0, 0})}
	require.NoError(t, b.Upsert(t.Context(), gen, chunks))
	n, err := b.PruneIneligibleEmbeddings(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n, "cleanup retains the republished vector while the room is Full")
	stats, err := b.Stats(t.Context(), gen)
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.EmbeddingCount)
}

func TestBackendColdPolicyAcceleratorPrune(t *testing.T) {
	b, st, sourceID, id, gen := newPolicyBackend(t)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), sourceID, "room", conversationpolicy.Full))
	chunks := []vector.Chunk{policyChunk(t, st, id, []float32{1, 0, 0, 0})}
	require.NoError(t, b.Upsert(t.Context(), gen, chunks))
	installReadyFlatAccelerator(t, b, gen, 4)
	require.NoError(t, b.ActivateGeneration(t.Context(), gen, true))
	second, err := b.CreateGeneration(t.Context(), "second", 4, "")
	require.NoError(t, err)
	require.NoError(t, b.Upsert(t.Context(), second, chunks))
	require.NoError(t, b.ActivateGeneration(t.Context(), second, true))
	third, err := b.CreateGeneration(t.Context(), "third", 4, "")
	require.NoError(t, err)
	require.NoError(t, b.Upsert(t.Context(), third, chunks))
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), sourceID, "room", conversationpolicy.Cold))
	n, err := b.PruneIneligibleEmbeddings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
	for _, generation := range []vector.GenerationID{gen, second, third} {
		var count int64
		require.NoError(t, b.db.QueryRow(`SELECT message_count FROM index_generations WHERE id=?`, int64(generation)).Scan(&count))
		assert.Zero(t, count)
	}
	var count int64
	require.NoError(t, b.db.QueryRow(`SELECT COUNT(*) FROM `+acceleratorTableName(gen)).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, b.db.QueryRow(`SELECT COUNT(*) FROM vectors_vec_d4`).Scan(&count))
	assert.Zero(t, count)
}

func TestColdPolicyStaleProgressCannotUndoReset(t *testing.T) {
	b, st, sourceID, _, gen := newPolicyBackend(t)
	ctx := t.Context()
	wm := embed.NewWatermark(b.db, st.Rebind)
	old, err := st.CaptureConversationPolicyWork(ctx)
	require.NoError(t, err)
	require.NoError(t, b.SetDocumentReconcileCursor(old, gen, "source:99"))
	require.NoError(t, wm.SetWatermark(old, gen, 99))
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Full))
	_, err = b.PruneIneligibleEmbeddings(ctx)
	require.NoError(t, err)
	require.ErrorIs(t, wm.SetWatermark(old, gen, 100), store.ErrConversationPolicyStale)
	watermark, err := wm.GetWatermark(ctx, gen)
	require.NoError(t, err)
	assert.Zero(t, watermark)
	require.ErrorIs(t, b.SetDocumentReconcileCursor(old, gen, "done:99"), store.ErrConversationPolicyStale)
	require.ErrorIs(t, b.SetDocumentJournalCursor(old, gen, "99|scope"), store.ErrConversationPolicyStale)
	require.ErrorIs(t, b.AdvanceDocumentChangeWatermark(old, gen, 99), store.ErrConversationPolicyStale)
	progress, err := b.GetDocumentProgress(ctx, gen)
	require.NoError(t, err)
	assert.Empty(t, progress.ReconcileCursor)
	assert.Empty(t, progress.JournalCursor)
	assert.Zero(t, progress.ChangeSequence)
	fresh, err := st.CaptureConversationPolicyWork(ctx)
	require.NoError(t, err)
	require.NoError(t, b.SetDocumentReconcileCursor(fresh, gen, "source:1"))
	require.NoError(t, b.SetDocumentJournalCursor(fresh, gen, "1|scope"))
	require.NoError(t, b.AdvanceDocumentChangeWatermark(fresh, gen, 1))
}

func TestColdPolicyLedgerBatchReleasesForPromotion(t *testing.T) {
	b, st, sourceID, id, gen := newPolicyBackend(t)
	ctx := t.Context()
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Full))
	docs := make([]vector.DocumentPublication, 1001)
	tx, err := st.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	for i := range docs {
		var member int64
		require.NoError(t, tx.QueryRowContext(ctx, st.Rebind(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type) SELECT source_id,conversation_id,?,'email' FROM messages WHERE id=? RETURNING id`), fmt.Sprintf("member-%04d", i), id).Scan(&member))
		docs[i] = vector.DocumentPublication{Key: fmt.Sprintf("doc-%04d", i), Kind: "message", Revision: "one", SourceSequence: 1, Members: []int64{member}}
	}
	require.NoError(t, tx.Commit())
	var chunks []vector.Chunk
	var ids []int64
	for _, doc := range docs {
		ids = append(ids, doc.Members[0])
		chunks = append(chunks, policyChunk(t, st, doc.Members[0], []float32{1, 0, 0, 0}))
	}
	require.NoError(t, b.PublishScopes(ctx, gen, []vector.DocumentScopePublication{{ScopeKey: "zero-chunks", SourceSequence: 1, Documents: docs, Chunks: chunks}}))
	require.NoError(t, b.Delete(ctx, gen, ids))
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Cold))
	_, _, err = b.pruneEmbeddingBatch(ctx, true, true)
	require.NoError(t, err)
	var current, members int
	require.NoError(t, b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embedding_documents WHERE state='current'`).Scan(&current))
	require.NoError(t, b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embedding_document_members`).Scan(&members))
	assert.Equal(t, 501, current, "one native batch must bound ledger mutation even with zero chunks")
	assert.Equal(t, 501, members)
	// Promotion must obtain W after the actual batch returns, before the next.
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Full))
	_, err = b.PruneIneligibleEmbeddings(ctx)
	require.NoError(t, err)
	require.NoError(t, b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embedding_documents WHERE state='current'`).Scan(&current))
	assert.Equal(t, 501, current, "later batch must preserve promoted remaining documents")
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Cold))
	_, err = b.PruneIneligibleEmbeddings(ctx)
	require.NoError(t, err)
	require.NoError(t, b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embedding_document_members`).Scan(&members))
	assert.Zero(t, members, "public pruner must continue through ledger-only batches")
}

func TestPersonColdPolicyPublicationUsesOriginalSemanticVersion(t *testing.T) {
	b, st, sourceID, _, gen := newPolicyBackend(t)
	ctx := t.Context()
	st.DB().SetMaxOpenConns(1)
	participant, err := st.EnsureParticipant("person@example.invalid", "Synthetic Person", "example.invalid")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantWithDisplayNameContext(ctx, participant, new("Synthetic Person"))
	require.NoError(t, err)
	doc, err := st.LoadPersonSemanticDocumentContext(ctx, person.ID)
	require.NoError(t, err)
	old, err := st.CaptureConversationPolicyWork(ctx)
	require.NoError(t, err)
	publications := []vector.PersonEmbedding{{PersonID: person.ID, Revision: doc.Revision, Vector: []float32{1, 0, 0, 0}}}
	require.NoError(t, b.UpsertPersons(old, gen, publications))
	person, err = st.UpdatePersonDisplayNameContext(ctx, person.ID, person.Revision, new("Updated Person"))
	require.NoError(t, err)
	require.ErrorIs(t, b.UpsertPersons(old, gen, publications), store.ErrConversationPolicyStale)
	doc, err = st.LoadPersonSemanticDocumentContext(ctx, person.ID)
	require.NoError(t, err)
	publications[0].Revision = doc.Revision
	require.NoError(t, b.UpsertPersons(old, gen, publications))
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Full))
	require.ErrorIs(t, b.UpsertPersons(old, gen, publications), store.ErrConversationPolicyStale)
	fresh, err := st.CaptureConversationPolicyWork(ctx)
	require.NoError(t, err)
	require.NoError(t, b.UpsertPersons(fresh, gen, publications))
}

func TestPersonColdPolicyCleanupPreservesCurrentVersions(t *testing.T) {
	b, st, _, _, gen := newPolicyBackend(t)
	ctx := t.Context()
	st.DB().SetMaxOpenConns(1)
	id, err := st.EnsureParticipant("cleanup@example.invalid", "Cleanup Person", "example.invalid")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantWithDisplayNameContext(ctx, id, new("Cleanup Person"))
	require.NoError(t, err)
	doc, err := st.LoadPersonSemanticDocumentContext(ctx, person.ID)
	require.NoError(t, err)
	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{{PersonID: person.ID, Revision: doc.Revision, Vector: []float32{1, 0, 0, 0}}}))
	require.NoError(t, b.ActivateGeneration(ctx, gen, true))
	next, err := b.CreateGeneration(ctx, "next-person", 4, "")
	require.NoError(t, err)
	require.NoError(t, b.UpsertPersons(ctx, next, []vector.PersonEmbedding{{PersonID: person.ID, Revision: doc.Revision, Vector: []float32{1, 0, 0, 0}}}))
	require.NoError(t, b.ActivateGeneration(ctx, next, true))
	// An upgrade changes the semantic version even for otherwise unchanged text.
	_, err = b.db.ExecContext(ctx, st.Rebind(`INSERT INTO person_embeddings(generation_id,person_id,published_revision,dimension,embedded_at) VALUES (?,?,'legacy-version',4,0) ON CONFLICT(generation_id,person_id) DO UPDATE SET published_revision=excluded.published_revision`), int64(gen), person.ID)
	require.NoError(t, err)
	pruner, ok := any(b).(interface {
		PruneIneligiblePersons(ctx context.Context) (int64, error)
	})
	require.True(t, ok)
	n, err := pruner.PruneIneligiblePersons(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	var count int
	require.NoError(t, b.db.QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM person_embeddings WHERE generation_id=?`), int64(gen)).Scan(&count))
	assert.Zero(t, count, "retired generations are reconciled too")
	revisions, err := b.ListPersonRevisions(ctx, next)
	require.NoError(t, err)
	assert.Equal(t, map[int64]string{person.ID: doc.Revision}, revisions)
	n, err = pruner.PruneIneligiblePersons(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestPersonColdPolicyCleanupBoundsBatch(t *testing.T) {
	b, st, _, _, gen := newPolicyBackend(t)
	ctx := t.Context()
	st.DB().SetMaxOpenConns(1)
	// Legacy rejected-version markers for people deleted before policy activation.
	tx, err := b.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for id := int64(10000); id < 10501; id++ {
		_, err := tx.ExecContext(ctx, st.Rebind(`INSERT INTO person_embeddings(generation_id,person_id,published_revision,dimension,embedded_at) VALUES (?,?,'legacy',4,0)`), int64(gen), id)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	n, g, p, more, err := b.prunePersonPolicyBatch(ctx, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(500), n)
	assert.True(t, more)
	var count int
	require.NoError(t, b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_embeddings`).Scan(&count))
	assert.Equal(t, 1, count)
	n, _, _, more, err = b.prunePersonPolicyBatch(ctx, g, p)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.False(t, more)
}

func TestPersonColdPolicyCleanupRetainsCurrentRevisionAfterPromotion(t *testing.T) {
	b, st, sourceID, _, gen := newPolicyBackend(t)
	ctx := t.Context()
	st.DB().SetMaxOpenConns(1)
	id, err := st.EnsureParticipant("promotion@example.invalid", "Promotion Person", "example.invalid")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantWithDisplayNameContext(ctx, id, new("Promotion Person"))
	require.NoError(t, err)
	doc, err := st.LoadPersonSemanticDocumentContext(ctx, person.ID)
	require.NoError(t, err)
	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{{PersonID: person.ID, Revision: doc.Revision, Vector: []float32{1, 0, 0, 0}}}))
	_, err = st.UpdatePersonDisplayNameContext(ctx, person.ID, person.Revision, new("Surviving Full Person"))
	require.NoError(t, err)
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Full))
	doc, err = st.LoadPersonSemanticDocumentContext(ctx, person.ID)
	require.NoError(t, err)
	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{{PersonID: person.ID, Revision: doc.Revision, Vector: []float32{1, 0, 0, 0}}}))
	_, err = b.PruneIneligiblePersons(ctx)
	require.NoError(t, err)
	revisions, err := b.ListPersonRevisions(ctx, gen)
	require.NoError(t, err)
	assert.Equal(t, map[int64]string{person.ID: doc.Revision}, revisions)
}
