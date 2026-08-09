//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

func documentChunk(messageID int64, value float32) vector.Chunk {
	v := make([]float32, 768)
	for i := range v {
		v[i] = value
	}
	return vector.Chunk{
		MessageID:      messageID,
		ChunkIndex:     0,
		Vector:         v,
		SourceCharLen:  12,
		ChunkCharStart: 1,
		ChunkCharEnd:   11,
		SourceBasis:    vector.SourceBasisBody,
	}
}

func documentPublication(key, revision string, sequence int64, members ...int64) vector.DocumentPublication {
	return vector.DocumentPublication{
		Key:            key,
		Kind:           "chat-window",
		Revision:       revision,
		SourceSequence: sequence,
		Members:        members,
	}
}

func TestDocumentPublicationIgnoresStaleScopeSequenceSQLite(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	publisher := vector.DocumentPublisher(b)

	const scope = "chat:sequence:day"
	newer := documentPublication("chat:sequence:window", "revision-20", 20, 91)
	require.NoError(t, publisher.PublishScope(ctx, gen, scope, 20,
		[]vector.DocumentPublication{newer}, []vector.Chunk{documentChunk(91, 0.9)}))
	stale := documentPublication(newer.Key, "revision-10", 10, 91)
	require.NoError(t, publisher.PublishScope(ctx, gen, scope, 10,
		[]vector.DocumentPublication{stale}, []vector.Chunk{documentChunk(91, 0.1)}))

	got, err := publisher.GetDocument(ctx, gen, newer.Key)
	require.NoError(t, err)
	assert.Equal(t, "revision-20", got.PublishedRevision)
	assert.Equal(t, int64(20), got.SourceSequence)
	assert.Equal(t, vector.DocumentCurrent, got.State)
	_, err = b.DB().Exec(`UPDATE embedding_documents SET published_revision = 'tampered'
		WHERE generation_id = ? AND document_key = ?`, int64(gen), newer.Key)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishScope(ctx, gen, scope, 20,
		[]vector.DocumentPublication{newer}, []vector.Chunk{documentChunk(91, 0.9)}))
	got, err = publisher.GetDocument(ctx, gen, newer.Key)
	require.NoError(t, err)
	assert.Equal(t, "revision-20", got.PublishedRevision, "an equal snapshot must repair ledger drift")

	require.NoError(t, publisher.PublishScope(ctx, gen, scope, 30, nil, nil))
	require.NoError(t, publisher.PublishScope(ctx, gen, scope, 25,
		[]vector.DocumentPublication{documentPublication(newer.Key, "revision-25", 25, 91)},
		[]vector.Chunk{documentChunk(91, 0.5)}))
	got, err = publisher.GetDocument(ctx, gen, newer.Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, got.State)
	assert.Equal(t, int64(30), got.SourceSequence)

	const emptyScope = "chat:sequence:empty"
	require.NoError(t, publisher.PublishScope(ctx, gen, emptyScope, 40, nil, nil))
	require.NoError(t, publisher.PublishScope(ctx, gen, emptyScope, 39,
		[]vector.DocumentPublication{documentPublication("chat:sequence:late", "revision-39", 39, 92)},
		[]vector.Chunk{documentChunk(92, 0.4)}))
	_, err = publisher.GetDocument(ctx, gen, "chat:sequence:late")
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestDocumentPublicationPreservesMatchingVectorsSQLite(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	const scope = "chat:preserve:day"
	first := documentPublication("chat:preserve:first", "revision-a", 1, 91)
	second := documentPublication("chat:preserve:second", "revision-a", 1, 92)
	require.NoError(t, b.PublishScope(ctx, gen, scope, 1,
		[]vector.DocumentPublication{first, second},
		[]vector.Chunk{documentChunk(91, 0.1), documentChunk(92, 0.2)}))

	first.SourceSequence = 2
	first.PreserveVectors = true
	second.SourceSequence = 2
	second.Revision = "revision-b"
	require.NoError(t, b.PublishScope(ctx, gen, scope, 2,
		[]vector.DocumentPublication{first, second}, []vector.Chunk{documentChunk(92, 0.3)}))
	var firstVectors, secondVectors int
	require.NoError(t, b.DB().QueryRow(`SELECT COUNT(*) FROM embeddings
		WHERE generation_id = ? AND message_id = ?`, int64(gen), 91).Scan(&firstVectors))
	require.NoError(t, b.DB().QueryRow(`SELECT COUNT(*) FROM embeddings
		WHERE generation_id = ? AND message_id = ?`, int64(gen), 92).Scan(&secondVectors))
	assert.Equal(t, 1, firstVectors)
	assert.Equal(t, 1, secondVectors)

	first.SourceSequence = 3
	first.Revision = "wrong-revision"
	second.SourceSequence = 3
	err = b.PublishScope(ctx, gen, scope, 3,
		[]vector.DocumentPublication{first, second}, []vector.Chunk{documentChunk(92, 0.4)})
	require.ErrorIs(t, err, vector.ErrDocumentFenceChanged)
}

func TestDocumentSequenceFenceSQLite(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	const scope = "chat:fence:day"
	initial := documentPublication("chat:fence:window", "revision-a", 1, 91)
	require.NoError(t, b.PublishScope(ctx, gen, scope, 1,
		[]vector.DocumentPublication{initial}, []vector.Chunk{documentChunk(91, 0.1)}))

	fenced := initial
	fenced.SourceSequence = 3
	require.NoError(t, b.PublishScopes(ctx, gen, []vector.DocumentScopePublication{{
		ScopeKey: scope, SourceSequence: 3, Documents: []vector.DocumentPublication{fenced}, FenceOnly: true,
	}}))
	got, err := b.GetDocument(ctx, gen, initial.Key)
	require.NoError(t, err)
	assert.Equal(t, "revision-a", got.PublishedRevision)
	assert.Equal(t, int64(3), got.SourceSequence)
	var vectorRows int
	require.NoError(t, b.DB().QueryRow(`SELECT COUNT(*) FROM embeddings
		WHERE generation_id = ? AND message_id = 91`, int64(gen)).Scan(&vectorRows))
	assert.Equal(t, 1, vectorRows, "a fence-only publication must preserve vectors")

	stale := documentPublication(initial.Key, "revision-b", 2, 91)
	require.NoError(t, b.PublishScope(ctx, gen, scope, 2,
		[]vector.DocumentPublication{stale}, []vector.Chunk{documentChunk(91, 0.9)}))
	got, err = b.GetDocument(ctx, gen, initial.Key)
	require.NoError(t, err)
	assert.Equal(t, "revision-a", got.PublishedRevision, "the newer fence must reject delayed vectors")

	const racedScope = "chat:fence:raced"
	racedA := documentPublication("chat:fence:raced-window", "revision-a", 1, 92)
	require.NoError(t, b.PublishScope(ctx, gen, racedScope, 1,
		[]vector.DocumentPublication{racedA}, []vector.Chunk{documentChunk(92, 0.1)}))
	racedB := documentPublication(racedA.Key, "revision-b", 2, 92)
	require.NoError(t, b.PublishScope(ctx, gen, racedScope, 2,
		[]vector.DocumentPublication{racedB}, []vector.Chunk{documentChunk(92, 0.9)}))
	racedFence := racedA
	racedFence.SourceSequence = 3
	err = b.PublishScopes(ctx, gen, []vector.DocumentScopePublication{{
		ScopeKey: racedScope, SourceSequence: 3,
		Documents: []vector.DocumentPublication{racedFence}, FenceOnly: true,
	}})
	require.ErrorIs(t, err, vector.ErrDocumentFenceChanged)
	got, err = b.GetDocument(ctx, gen, racedA.Key)
	require.NoError(t, err)
	assert.Equal(t, "revision-b", got.PublishedRevision)
	assert.Equal(t, int64(2), got.SourceSequence, "a rejected fence must roll back its sequence claim")
}

func TestDocumentPublicationSQLite(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	publisher := vector.DocumentPublisher(b)

	first := documentPublication("chat:1:a", "r1", 9, 7, 8)
	firstChunk := documentChunk(7, 0.1)
	require.NoError(t, publisher.PublishScope(ctx, gen, "chat:1:day", 9, []vector.DocumentPublication{first}, []vector.Chunk{
		firstChunk, documentChunk(8, 0.2),
	}))
	assertScoredChunkBasisSQLite(t, b, gen, firstChunk, vector.SourceBasisBody)

	got, err := publisher.GetDocument(ctx, gen, first.Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentCurrent, got.State)
	assert.Equal(t, "r1", got.PublishedRevision)
	assert.Equal(t, int64(9), got.SourceSequence)
	assert.Equal(t, []int64{7, 8}, got.Members)
	assert.Equal(t, "chat:1:day", got.ScopeKey)
	require.Error(t, publisher.PublishScope(ctx, gen, "chat:1:day", 10, []vector.DocumentPublication{first}, []vector.Chunk{
		documentChunk(7, 0.1), documentChunk(8, 0.2),
	}), "per-document sequence must match the scope publication sequence")

	// Replaying the same complete publication is idempotent.
	require.NoError(t, publisher.PublishScope(ctx, gen, "chat:1:day", 9, []vector.DocumentPublication{first}, []vector.Chunk{
		documentChunk(7, 0.1), documentChunk(8, 0.2),
	}))
	docs, err := publisher.ListDocumentsForScope(ctx, gen, "chat:1:day")
	require.NoError(t, err)
	require.Len(t, docs, 1)
	assert.Equal(t, []int64{7, 8}, docs[0].Members)

	// A split replaces ownership and tombstones the obsolete key atomically.
	left := documentPublication("chat:1:left", "r2", 10, 7)
	right := documentPublication("chat:1:right", "r2", 10, 8)
	require.NoError(t, publisher.PublishScope(ctx, gen, "chat:1:day", 10, []vector.DocumentPublication{left, right}, []vector.Chunk{
		documentChunk(7, 0.3), documentChunk(8, 0.4),
	}))
	docs, err = publisher.ListDocumentsForScope(ctx, gen, "chat:1:day")
	require.NoError(t, err)
	require.Len(t, docs, 2)
	assert.Equal(t, []int64{7}, docs[0].Members)
	assert.Equal(t, []int64{8}, docs[1].Members)
	retired, err := publisher.GetDocument(ctx, gen, first.Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, retired.State)

	// A merge restores one owner per message.
	merged := documentPublication("chat:1:merged", "r3", 11, 7, 8)
	require.NoError(t, publisher.PublishScope(ctx, gen, "chat:1:day", 11, []vector.DocumentPublication{merged}, []vector.Chunk{
		documentChunk(7, 0.5), documentChunk(8, 0.6),
	}))
	for _, messageID := range []int64{7, 8} {
		var owners int
		require.NoError(t, b.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM embedding_document_members WHERE generation_id = ? AND message_id = ?`,
			int64(gen), messageID).Scan(&owners))
		assert.Equal(t, 1, owners)
	}
}

func TestDocumentPublicationReassignmentProtectsNewVectorsSQLite(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	publisher := vector.DocumentPublisher(b)

	old := documentPublication("old-anchor", "r1", 1, 21)
	require.NoError(t, publisher.PublishScope(ctx, gen, "old-scope", 1, []vector.DocumentPublication{old}, []vector.Chunk{documentChunk(21, 0.1)}))
	newDoc := documentPublication("new-anchor", "r2", 2, 21)
	require.NoError(t, publisher.PublishScope(ctx, gen, "new-scope", 2, []vector.DocumentPublication{newDoc}, []vector.Chunk{documentChunk(21, 0.9)}))

	// Tombstoning the stale old scope must not delete a vector whose owner has
	// already moved to a new document.
	require.NoError(t, publisher.PublishScope(ctx, gen, "old-scope", 12, nil, nil))
	require.NoError(t, publisher.PublishScope(ctx, gen, "old-scope", 12, nil, nil), "empty replay is idempotent")
	tombstone, err := publisher.GetDocument(ctx, gen, old.Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, tombstone.State)
	assert.Equal(t, int64(12), tombstone.SourceSequence)
	current, err := publisher.GetDocument(ctx, gen, newDoc.Key)
	require.NoError(t, err)
	assert.Equal(t, []int64{21}, current.Members)
	var vectorRows int
	require.NoError(t, b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = ?`,
		int64(gen), int64(21)).Scan(&vectorRows))
	assert.Equal(t, 1, vectorRows)
}

func TestDocumentPublicationRollbackAndLifecycleSQLite(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	publisher := vector.DocumentPublisher(b)
	original := documentPublication("stable", "r1", 1, 31)
	require.NoError(t, publisher.PublishScope(ctx, gen, "scope", 1, []vector.DocumentPublication{original}, []vector.Chunk{documentChunk(31, 0.1)}))

	// Duplicate chunk identity fails after transactional staging has started.
	replacement := documentPublication("replacement", "r2", 2, 31)
	err = publisher.PublishScope(ctx, gen, "scope", 2, []vector.DocumentPublication{replacement}, []vector.Chunk{
		documentChunk(31, 0.2), documentChunk(31, 0.3),
	})
	require.Error(t, err)
	got, getErr := publisher.GetDocument(ctx, gen, original.Key)
	require.NoError(t, getErr)
	assert.Equal(t, vector.DocumentCurrent, got.State)
	assert.Equal(t, "r1", got.PublishedRevision)

	require.NoError(t, b.RetireGeneration(ctx, gen, false))
	err = publisher.PublishScope(ctx, gen, "scope", 2, []vector.DocumentPublication{replacement}, []vector.Chunk{documentChunk(31, 0.2)})
	require.ErrorIs(t, err, vector.ErrGenerationRetired)
}

func TestDocumentScopeBatchPublicationSQLite(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	publisher := vector.DocumentPublisher(b)

	scopes := []vector.DocumentScopePublication{
		{ScopeKey: "scope:a", SourceSequence: 1,
			Documents: []vector.DocumentPublication{documentPublication("a", "r1", 1, 71)},
			Chunks:    []vector.Chunk{documentChunk(71, 0.1)}},
		{ScopeKey: "scope:b", SourceSequence: 2,
			Documents: []vector.DocumentPublication{documentPublication("b", "r1", 2, 72)},
			Chunks:    []vector.Chunk{documentChunk(72, 0.2)}},
	}
	require.NoError(t, publisher.PublishScopes(ctx, gen, scopes))
	for _, key := range []string{"a", "b"} {
		doc, getErr := publisher.GetDocument(ctx, gen, key)
		require.NoError(t, getErr)
		assert.Equal(t, vector.DocumentCurrent, doc.State)
	}
	var messageCount int64
	require.NoError(t, b.DB().QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = ?`, int64(gen)).Scan(&messageCount))
	assert.Equal(t, int64(2), messageCount)
	for _, tc := range []struct {
		name     string
		oldFirst bool
	}{
		{name: "new owner before old tombstone"},
		{name: "old tombstone before new owner", oldFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			moveBackend, moveCtx := newBackendForTest(t)
			moveGen, createErr := moveBackend.CreateGeneration(moveCtx, "model", 768, "model:768:context")
			require.NoError(t, createErr)
			movePublisher := vector.DocumentPublisher(moveBackend)
			require.NoError(t, movePublisher.PublishScope(moveCtx, moveGen, "scope:old", 1,
				[]vector.DocumentPublication{documentPublication("old", "r1", 1, 81)},
				[]vector.Chunk{documentChunk(81, 0.1)}))

			newOwner := vector.DocumentScopePublication{ScopeKey: "scope:new", SourceSequence: 2,
				Documents: []vector.DocumentPublication{documentPublication("moved", "r2", 2, 81)},
				Chunks:    []vector.Chunk{documentChunk(81, 0.9)}}
			oldTombstone := vector.DocumentScopePublication{ScopeKey: "scope:old", SourceSequence: 3}
			move := []vector.DocumentScopePublication{newOwner, oldTombstone}
			if tc.oldFirst {
				move[0], move[1] = move[1], move[0]
			}
			require.NoError(t, movePublisher.PublishScopes(moveCtx, moveGen, move))

			var owner string
			require.NoError(t, moveBackend.DB().QueryRowContext(moveCtx,
				`SELECT document_key FROM embedding_document_members WHERE generation_id = ? AND message_id = ?`,
				int64(moveGen), int64(81)).Scan(&owner))
			assert.Equal(t, "moved", owner)
			var vectorRows int
			require.NoError(t, moveBackend.DB().QueryRowContext(moveCtx,
				`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = ?`,
				int64(moveGen), int64(81)).Scan(&vectorRows))
			assert.Equal(t, 1, vectorRows)
		})
	}

	bad := []vector.DocumentScopePublication{
		{ScopeKey: "scope:c", SourceSequence: 3,
			Documents: []vector.DocumentPublication{documentPublication("c", "r1", 3, 73)},
			Chunks:    []vector.Chunk{documentChunk(73, 0.3)}},
		{ScopeKey: "scope:d", SourceSequence: 4,
			Documents: []vector.DocumentPublication{documentPublication("d", "r1", 4, 74)},
			Chunks:    []vector.Chunk{documentChunk(74, 0.4), documentChunk(74, 0.5)}},
	}
	require.Error(t, publisher.PublishScopes(ctx, gen, bad))
	_, err = publisher.GetDocument(ctx, gen, "c")
	require.Error(t, err)
	_, err = publisher.GetDocument(ctx, gen, "d")
	require.Error(t, err)
}

func TestDocumentPaginationAndWatermarksSQLite(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	publisher := vector.DocumentPublisher(b)

	for i, key := range []string{"a", "b", "c"} {
		messageID := int64(41 + i)
		doc := documentPublication(key, "r1", int64(i+1), messageID)
		require.NoError(t, publisher.PublishScope(ctx, gen, "scope:"+key, int64(i+1), []vector.DocumentPublication{doc}, []vector.Chunk{documentChunk(messageID, 0.1)}))
	}
	page, err := publisher.ListDocumentsAfter(ctx, gen, "a", 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "b", page[0].Key)

	progress, err := publisher.GetDocumentProgress(ctx, gen)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentProgress{}, progress)
	require.NoError(t, publisher.AdvanceDocumentChangeWatermark(ctx, gen, 12))
	require.NoError(t, publisher.AdvanceDocumentChangeWatermark(ctx, gen, 8))
	require.NoError(t, publisher.SetDocumentReconcileCursor(ctx, gen, "scope:b"))
	require.NoError(t, publisher.SetDocumentJournalCursor(ctx, gen, "12|chat:7:2026-08-08"))
	progress, err = publisher.GetDocumentProgress(ctx, gen)
	require.NoError(t, err)
	assert.Equal(t, int64(12), progress.ChangeSequence)
	assert.Equal(t, "scope:b", progress.ReconcileCursor)
	assert.Equal(t, "12|chat:7:2026-08-08", progress.JournalCursor)
	minimum, tracked, err := b.MinimumDocumentChangeWatermark(ctx)
	require.NoError(t, err)
	assert.True(t, tracked)
	assert.Equal(t, int64(12), minimum)
	require.NoError(t, publisher.SetDocumentJournalCursor(ctx, gen, ""))
	require.NoError(t, publisher.ResetDocumentReconcileCursor(ctx, gen))
	progress, err = publisher.GetDocumentProgress(ctx, gen)
	require.NoError(t, err)
	assert.Empty(t, progress.ReconcileCursor)
}

func TestDocumentJournalDisablesAfterLastContextualGenerationSQLite(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	st, err := store.OpenForTest(mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	b, err := Open(t.Context(), Options{
		Path: filepath.Join(dir, "vectors.db"), MainPath: mainPath,
		MainDB: st.DB(), Dimension: 768,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	seedJournal := func() {
		t.Helper()
		require.NoError(t, st.EnableEmbeddingChangeJournal(t.Context()))
		_, seedErr := st.DB().Exec(`
			UPDATE embedding_change_clock SET sequence = sequence + 1 WHERE singleton = 1;
			INSERT INTO embedding_changes (sequence, kind)
			SELECT sequence, 'message_update' FROM embedding_change_clock WHERE singleton = 1;`)
		require.NoError(t, seedErr)
	}
	assertDisabledAndEmpty := func() {
		t.Helper()
		var enabled bool
		var rows int
		require.NoError(t, st.DB().QueryRow(
			`SELECT enabled FROM embedding_change_clock WHERE singleton = 1`).Scan(&enabled))
		require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM embedding_changes`).Scan(&rows))
		assert.False(t, enabled)
		assert.Zero(t, rows)
	}
	assertEnabledAndRetained := func() {
		t.Helper()
		var enabled bool
		var rows int
		require.NoError(t, st.DB().QueryRow(
			`SELECT enabled FROM embedding_change_clock WHERE singleton = 1`).Scan(&enabled))
		require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM embedding_changes`).Scan(&rows))
		assert.True(t, enabled)
		assert.Equal(t, 1, rows)
	}

	retired, err := b.CreateGeneration(t.Context(), "context", 768, "context:retire")
	require.NoError(t, err)
	require.NoError(t, b.AdvanceDocumentChangeWatermark(t.Context(), retired, 1))
	seedJournal()
	require.NoError(t, b.CleanupDocumentJournalIfUnused(t.Context()))
	assertEnabledAndRetained()
	require.NoError(t, b.RetireGeneration(t.Context(), retired, false))
	assertDisabledAndEmpty()

	contextual, err := b.CreateGeneration(t.Context(), "context", 768, "context:replace")
	require.NoError(t, err)
	require.NoError(t, b.AdvanceDocumentChangeWatermark(t.Context(), contextual, 2))
	require.NoError(t, b.ActivateGeneration(t.Context(), contextual, true))
	seedJournal()
	ordinary, err := b.CreateGeneration(t.Context(), "ordinary", 768, "ordinary:replace")
	require.NoError(t, err)
	require.NoError(t, b.ActivateGeneration(t.Context(), ordinary, true))
	assertDisabledAndEmpty()
}

func TestDocumentJournalCleanupFailureDoesNotMaskLifecycleSQLite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	st, err := store.OpenForTest(mainPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	b, err := Open(t.Context(), Options{
		Path: filepath.Join(dir, "vectors.db"), MainPath: mainPath,
		MainDB: st.DB(), Dimension: 768,
	})
	require.NoError(err)
	t.Cleanup(func() { _ = b.Close() })

	installCleanupFailure := func() {
		t.Helper()
		_, triggerErr := st.DB().Exec(`
			CREATE TRIGGER fail_contextual_journal_cleanup
			BEFORE UPDATE OF sequence ON embedding_change_clock
			WHEN NEW.sequence = OLD.sequence
			BEGIN
				SELECT RAISE(FAIL, 'synthetic cleanup failure');
			END`)
		require.NoError(triggerErr)
	}
	removeCleanupFailure := func() {
		t.Helper()
		_, triggerErr := st.DB().Exec(`DROP TRIGGER fail_contextual_journal_cleanup`)
		require.NoError(triggerErr)
	}
	seedJournal := func() {
		t.Helper()
		require.NoError(st.EnableEmbeddingChangeJournal(t.Context()))
		_, seedErr := st.DB().Exec(`
			UPDATE embedding_change_clock SET sequence = sequence + 1 WHERE singleton = 1;
			INSERT INTO embedding_changes (sequence, kind)
			SELECT sequence, 'message_update' FROM embedding_change_clock WHERE singleton = 1;`)
		require.NoError(seedErr)
	}

	retired, err := b.CreateGeneration(t.Context(), "context", 768, "context:cleanup-retire")
	require.NoError(err)
	require.NoError(b.AdvanceDocumentChangeWatermark(t.Context(), retired, 1))
	seedJournal()
	installCleanupFailure()
	require.NoError(b.RetireGeneration(t.Context(), retired, false),
		"a committed retirement must not be reported as failed")
	removeCleanupFailure()
	require.NoError(b.CleanupDocumentJournalIfUnused(t.Context()),
		"cleanup remains independently retryable")

	contextual, err := b.CreateGeneration(t.Context(), "context", 768, "context:cleanup-activate")
	require.NoError(err)
	require.NoError(b.AdvanceDocumentChangeWatermark(t.Context(), contextual, 2))
	require.NoError(b.ActivateGeneration(t.Context(), contextual, true))
	seedJournal()
	ordinary, err := b.CreateGeneration(t.Context(), "ordinary", 768, "ordinary:cleanup-activate")
	require.NoError(err)
	installCleanupFailure()
	require.NoError(b.ActivateGeneration(t.Context(), ordinary, true),
		"a committed activation must not be reported as failed")
	active, err := b.ActiveGeneration(t.Context())
	require.NoError(err)
	assert.Equal(ordinary, active.ID)
	removeCleanupFailure()
	require.NoError(b.CleanupDocumentJournalIfUnused(t.Context()),
		"activation cleanup remains independently retryable")
}

func TestDocumentMigrationFreshUpgradeAndOrdinaryBasisSQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db := openTestDB(t, path)
	t.Cleanup(func() { _ = db.Close() })

	// This is the real prior chunked schema: it has all prior embedding
	// columns, but no source_basis and a progress table without journal_cursor.
	_, err := db.ExecContext(ctx, `
		CREATE TABLE index_generations (
			id INTEGER PRIMARY KEY AUTOINCREMENT, model TEXT NOT NULL,
			dimension INTEGER NOT NULL, fingerprint TEXT NOT NULL,
			started_at INTEGER NOT NULL, seeded_at INTEGER, completed_at INTEGER,
			activated_at INTEGER, state TEXT NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE embeddings (
			embedding_id INTEGER PRIMARY KEY AUTOINCREMENT,
			generation_id INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL, chunk_index INTEGER NOT NULL DEFAULT 0,
			embedded_at INTEGER NOT NULL, source_char_len INTEGER NOT NULL,
			chunk_char_start INTEGER NOT NULL DEFAULT 0,
			chunk_char_end INTEGER NOT NULL DEFAULT 0,
			truncated INTEGER NOT NULL DEFAULT 0,
			UNIQUE (generation_id, message_id, chunk_index)
		);
		CREATE TABLE embedding_document_progress (
			generation_id INTEGER PRIMARY KEY REFERENCES index_generations(id) ON DELETE CASCADE,
			change_sequence INTEGER NOT NULL DEFAULT 0,
			reconcile_cursor TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO index_generations
			(id, model, dimension, fingerprint, started_at, state)
			VALUES (1, 'm', 768, 'm:768', 1, 'active');
		INSERT INTO embeddings
			(generation_id, message_id, embedded_at, source_char_len)
			VALUES (1, 99, 1, 4);
		INSERT INTO embedding_document_progress
			(generation_id, change_sequence, reconcile_cursor)
			VALUES (1, 7, 'done:7');
	`)
	require.NoError(t, err)
	require.NoError(t, Migrate(ctx, db, 768))
	require.NoError(t, Migrate(ctx, db, 768), "migration must be idempotent")

	for _, table := range []string{"embedding_documents", "embedding_document_scopes", "embedding_document_members", "embedding_document_progress"} {
		var name string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name))
	}
	var upgradedProgress vector.DocumentProgress
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT change_sequence, reconcile_cursor, journal_cursor
		  FROM embedding_document_progress WHERE generation_id = 1`).
		Scan(&upgradedProgress.ChangeSequence, &upgradedProgress.ReconcileCursor, &upgradedProgress.JournalCursor))
	assert.Equal(t, vector.DocumentProgress{ChangeSequence: 7, ReconcileCursor: "done:7"}, upgradedProgress)
	_, err = db.ExecContext(ctx, `
		INSERT INTO embedding_documents
			(generation_id, document_key, kind, scope_key, state, published_revision, source_sequence, updated_at)
		VALUES (1, 'legacy-doc', 'chat-window', 'legacy-scope', 'current', 'r17', 17, 1);
		DROP TABLE embedding_document_scopes;
	`)
	require.NoError(t, err)
	require.NoError(t, Migrate(ctx, db, 768), "migration must restore and backfill scope sequence state")
	var legacyScopeSequence int64
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT source_sequence FROM embedding_document_scopes
		 WHERE generation_id = 1 AND scope_key = 'legacy-scope'`).Scan(&legacyScopeSequence))
	assert.Equal(t, int64(17), legacyScopeSequence)
	var basis int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT source_basis FROM embeddings WHERE generation_id = 1 AND message_id = 99`).Scan(&basis))
	assert.Equal(t, int(vector.SourceBasisSubjectBody), basis)

	// The upgraded schema preserves both contextual document and ordinary
	// flat source bases through the public scoring path.
	upgraded := &Backend{db: db, dim: 768}
	contextual := documentChunk(98, 0.3)
	require.NoError(t, upgraded.PublishScope(ctx, 1, "chat:98:day", 2,
		[]vector.DocumentPublication{documentPublication("chat:98", "r2", 2, 98)},
		[]vector.Chunk{contextual}))
	assertScoredChunkBasisSQLite(t, upgraded, 1, contextual, vector.SourceBasisBody)
	upgradedOrdinary := documentChunk(100, 0.4)
	upgradedOrdinary.SourceBasis = vector.SourceBasisSubjectBody
	require.NoError(t, upgraded.Upsert(ctx, 1, []vector.Chunk{upgradedOrdinary}))
	assertScoredChunkBasisSQLite(t, upgraded, 1, upgradedOrdinary, vector.SourceBasisSubjectBody)

	// Ordinary flat Upsert rows keep the zero-value subject+body basis.
	main := openMainDBWithOneMessage(t)
	b, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "fresh.db"), Dimension: 768, MainDB: main})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	gen, err := b.CreateGeneration(ctx, "model", 768, "")
	require.NoError(t, err)
	ordinary := documentChunk(100, 0.2)
	ordinary.SourceBasis = vector.SourceBasisBody
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{ordinary}))
	assertScoredChunkBasisSQLite(t, b, gen, ordinary, vector.SourceBasisBody)
	require.NoError(t, b.db.QueryRowContext(ctx,
		`SELECT source_basis FROM embeddings WHERE generation_id = ? AND message_id = ?`, int64(gen), int64(100)).Scan(&basis))
	assert.Equal(t, int(vector.SourceBasisBody), basis)
}

func assertScoredChunkBasisSQLite(t *testing.T, b *Backend, gen vector.GenerationID, chunk vector.Chunk, want vector.SourceBasis) {
	t.Helper()
	hits, err := b.ScoreMessageChunks(context.Background(), gen, chunk.MessageID, chunk.Vector)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, want, hits[0].SourceBasis)
}
