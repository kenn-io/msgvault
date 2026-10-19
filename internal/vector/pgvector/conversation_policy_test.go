//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

func newPolicyBackend(t *testing.T) (*Backend, *store.Store, int64, int64, vector.GenerationID, context.Context) {
	t.Helper()
	_ = testDBURL(t)
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	src, err := st.GetOrCreateSource("beeper", "native-policy")
	require.NoError(t, err)
	room, err := st.EnsureConversation(src.ID, "room", "Room")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper", Subject: sql.NullString{String: "policyneedle", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.UpsertFTS(id, "policyneedle", "", "", "", ""))
	b, err := Open(ctx, Options{DB: st.DB(), PolicyStore: st, Dimension: 4})
	require.NoError(t, err)
	gen, err := b.CreateGeneration(ctx, "policy", 4, "")
	require.NoError(t, err)
	return b, st, src.ID, id, gen, ctx
}

func policyChunk(t *testing.T, st *store.Store, messageID int64, values []float32) vector.Chunk {
	t.Helper()
	var revision int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT embedding_revision FROM messages WHERE id=?`), messageID).Scan(&revision))
	return vector.Chunk{MessageID: messageID, Vector: values, EmbeddingRevision: revision}
}

func TestPostgresColdPolicyPublicationWithOneConnection(t *testing.T) {
	b, st, sourceID, id, gen, ctx := newPolicyBackend(t)
	st.DB().SetMaxOpenConns(1)
	chunks := []vector.Chunk{{MessageID: id, Vector: []float32{1, 0, 0, 0}}}
	require.ErrorIs(t, b.Upsert(ctx, gen, chunks), store.ErrConversationPolicyStale)
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Full))
	chunks = []vector.Chunk{policyChunk(t, st, id, []float32{1, 0, 0, 0})}
	admitted, err := st.CaptureConversationPolicyWork(ctx)
	require.NoError(t, err)
	require.NoError(t, b.Upsert(admitted, gen, chunks))
	scored, err := b.ScoreMessageChunks(ctx, gen, id, chunks[0].Vector)
	require.NoError(t, err)
	require.Len(t, scored, 1)
	scopes := []vector.DocumentScopePublication{{ScopeKey: "policy-scope", SourceSequence: 1, Documents: []vector.DocumentPublication{{Key: "policy-doc", Kind: "message", Revision: "one", SourceSequence: 1, Members: []int64{id}}}, Chunks: chunks}}
	require.NoError(t, b.PublishScopes(admitted, gen, scopes))
	fused, _, err := b.FusedSearch(ctx, vector.FusedRequest{Generation: gen, FTSTerms: []string{"policyneedle"}, KPerSignal: 10, Limit: 10, RRFK: 60})
	require.NoError(t, err)
	require.Len(t, fused, 1)

	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Cold))
	hits, err := b.Search(ctx, gen, []float32{1, 0, 0, 0}, 10, vector.Filter{})
	require.NoError(t, err)
	assert.Empty(t, hits)
	scored, err = b.ScoreMessageChunks(ctx, gen, id, chunks[0].Vector)
	require.NoError(t, err)
	assert.Empty(t, scored)
	fused, _, err = b.FusedSearch(ctx, vector.FusedRequest{Generation: gen, FTSTerms: []string{"policyneedle"}, KPerSignal: 10, Limit: 10, RRFK: 60})
	require.NoError(t, err)
	assert.Empty(t, fused)

	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Full))
	require.ErrorIs(t, b.Upsert(admitted, gen, chunks), store.ErrConversationPolicyStale)
	require.ErrorIs(t, b.PublishScopes(admitted, gen, scopes), store.ErrConversationPolicyStale)
	fresh, err := st.CaptureConversationPolicyWork(ctx)
	require.NoError(t, err)
	chunks = []vector.Chunk{policyChunk(t, st, id, []float32{1, 0, 0, 0})}
	require.NoError(t, b.Upsert(fresh, gen, chunks))
	n, err := b.PruneIneligibleEmbeddings(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "promotion preserves freshly published vectors")
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Cold))
	n, err = b.PruneIneligibleEmbeddings(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	record, err := b.GetDocument(ctx, gen, "policy-doc")
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, record.State)
	stats, err := b.Stats(ctx, gen)
	require.NoError(t, err)
	assert.Zero(t, stats.EmbeddingCount)
}

func TestPostgresLoadVectorRejectsColdConversationSeed(t *testing.T) {
	b, st, sourceID, id, gen, ctx := newPolicyBackend(t)
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

func TestPostgresContextualVectorConsumersHideUnstampedEmbeddings(t *testing.T) {
	b, st, sourceID, id, gen, ctx := newPolicyBackend(t)
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

	_, err = st.DB().ExecContext(ctx, `UPDATE messages SET embed_gen=NULL, embedding_revision=embedding_revision+1 WHERE id=$1`, id)
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

func TestPostgresActiveGenerationVectorsSurvivePartialSameModelRebuild(t *testing.T) {
	b, st, sourceID, firstID, active, ctx := newPolicyBackend(t)
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

	building, err := b.CreateGeneration(ctx, "policy", 4, "")
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

func TestPostgresColdPolicyTransitionSerializesNativePublication(t *testing.T) {
	rawURL := testDBURL(t)
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var database, schema string
	require.NoError(t, st.DB().QueryRowContext(ctx, `SELECT current_database(), current_schema()`).Scan(&database, &schema))
	parsed, err := url.Parse(rawURL)
	require.NoError(t, err)
	parsed.Path = "/" + database
	query := parsed.Query()
	query.Set("search_path", schema+",public")
	parsed.RawQuery = query.Encode()
	other, err := store.OpenPostgresContext(ctx, parsed.String())
	require.NoError(t, err)
	defer func() { require.NoError(t, other.Close()) }()
	src, err := st.GetOrCreateSource("beeper", "transition-policy")
	require.NoError(t, err)
	room, err := st.EnsureConversation(src.ID, "room", "Room")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper"})
	require.NoError(t, err)
	require.NoError(t, st.SetConversationPolicyOverride(ctx, src.ID, "room", conversationpolicy.Full))
	admitted, err := st.CaptureConversationPolicyWork(ctx)
	require.NoError(t, err)
	b, err := Open(ctx, Options{DB: st.DB(), PolicyStore: st, Dimension: 4})
	require.NoError(t, err)
	gen, err := b.CreateGeneration(ctx, "transition", 4, "")
	require.NoError(t, err)
	// Hold the generation row so actual Upsert pauses after acquiring policy R.
	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(ctx, `SELECT id FROM index_generations WHERE id=$1 FOR UPDATE`, int64(gen))
	require.NoError(t, err)
	published := make(chan error, 1)
	chunks := []vector.Chunk{policyChunk(t, st, id, []float32{1, 0, 0, 0})}
	go func() { published <- b.Upsert(admitted, gen, chunks) }()
	waitPolicyLock := func(granted bool, mode string) {
		t.Helper()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var found bool
			require.NoError(t, blocker.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND granted=$1 AND mode=$2)`, granted, mode).Scan(&found))
			if found {
				return
			}
			select {
			case <-ctx.Done():
				require.NoError(t, ctx.Err())
				return
			case <-ticker.C:
			}
		}
	}
	waitPolicyLock(true, "ShareLock")
	transitioned := make(chan error, 1)
	go func() {
		transitioned <- other.SetConversationPolicyOverride(ctx, src.ID, "room", conversationpolicy.Cold)
	}()
	waitPolicyLock(false, "ExclusiveLock")
	require.NoError(t, blocker.Commit())
	require.NoError(t, <-published)
	require.NoError(t, <-transitioned)
	n, err := b.PruneIneligibleEmbeddings(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	require.ErrorIs(t, b.Upsert(admitted, gen, chunks), store.ErrConversationPolicyStale)
}

func TestColdPolicyCleanupRetainsRepublishedPromotion(t *testing.T) {
	b, st, sourceID, id, gen, ctx := newPolicyBackend(t)
	st.DB().SetMaxOpenConns(1)
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Full))
	chunks := []vector.Chunk{policyChunk(t, st, id, []float32{1, 0, 0, 0})}
	require.NoError(t, b.Upsert(ctx, gen, chunks))
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Cold))
	require.NoError(t, st.SetConversationPolicyOverride(ctx, sourceID, "room", conversationpolicy.Full))
	chunks = []vector.Chunk{policyChunk(t, st, id, []float32{1, 0, 0, 0})}
	require.NoError(t, b.Upsert(ctx, gen, chunks))
	n, err := b.PruneIneligibleEmbeddings(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "cleanup retains the republished vector while the room is Full")
	stats, err := b.Stats(ctx, gen)
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.EmbeddingCount)
}

func TestColdPolicyStaleProgressCannotUndoReset(t *testing.T) {
	b, st, sourceID, _, gen, ctx := newPolicyBackend(t)
	st.DB().SetMaxOpenConns(1)
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
	b, st, sourceID, id, gen, ctx := newPolicyBackend(t)
	st.DB().SetMaxOpenConns(1)
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
		chunks = append(chunks, vector.Chunk{MessageID: doc.Members[0], Vector: []float32{1, 0, 0, 0}})
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
	b, st, sourceID, _, gen, ctx := newPolicyBackend(t)
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
	b, st, _, _, gen, ctx := newPolicyBackend(t)
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
	b, st, _, _, gen, ctx := newPolicyBackend(t)
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
	b, st, sourceID, _, gen, ctx := newPolicyBackend(t)
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
