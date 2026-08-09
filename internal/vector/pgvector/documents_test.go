//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
)

func pgDocumentChunk(messageID int64, value float32) vector.Chunk {
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

func pgDocumentPublication(key, revision string, sequence int64, members ...int64) vector.DocumentPublication {
	return vector.DocumentPublication{
		Key:            key,
		Kind:           "chat-window",
		Revision:       revision,
		SourceSequence: sequence,
		Members:        members,
	}
}

func TestDocumentPublicationIgnoresStaleScopeSequencePostgreSQL(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	publisher := vector.DocumentPublisher(b)

	const scope = "chat:sequence:day"
	newer := pgDocumentPublication("chat:sequence:window", "revision-20", 20, 91)
	require.NoError(t, publisher.PublishScope(ctx, gen, scope, 20,
		[]vector.DocumentPublication{newer}, []vector.Chunk{pgDocumentChunk(91, 0.9)}))
	stale := pgDocumentPublication(newer.Key, "revision-10", 10, 91)
	require.NoError(t, publisher.PublishScope(ctx, gen, scope, 10,
		[]vector.DocumentPublication{stale}, []vector.Chunk{pgDocumentChunk(91, 0.1)}))

	got, err := publisher.GetDocument(ctx, gen, newer.Key)
	require.NoError(t, err)
	assert.Equal(t, "revision-20", got.PublishedRevision)
	assert.Equal(t, int64(20), got.SourceSequence)
	assert.Equal(t, vector.DocumentCurrent, got.State)
	_, err = b.DB().Exec(`UPDATE embedding_documents SET published_revision = 'tampered'
		WHERE generation_id = $1 AND document_key = $2`, int64(gen), newer.Key)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishScope(ctx, gen, scope, 20,
		[]vector.DocumentPublication{newer}, []vector.Chunk{pgDocumentChunk(91, 0.9)}))
	got, err = publisher.GetDocument(ctx, gen, newer.Key)
	require.NoError(t, err)
	assert.Equal(t, "revision-20", got.PublishedRevision, "an equal snapshot must repair ledger drift")

	require.NoError(t, publisher.PublishScope(ctx, gen, scope, 30, nil, nil))
	require.NoError(t, publisher.PublishScope(ctx, gen, scope, 25,
		[]vector.DocumentPublication{pgDocumentPublication(newer.Key, "revision-25", 25, 91)},
		[]vector.Chunk{pgDocumentChunk(91, 0.5)}))
	got, err = publisher.GetDocument(ctx, gen, newer.Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, got.State)
	assert.Equal(t, int64(30), got.SourceSequence)

	const emptyScope = "chat:sequence:empty"
	require.NoError(t, publisher.PublishScope(ctx, gen, emptyScope, 40, nil, nil))
	require.NoError(t, publisher.PublishScope(ctx, gen, emptyScope, 39,
		[]vector.DocumentPublication{pgDocumentPublication("chat:sequence:late", "revision-39", 39, 92)},
		[]vector.Chunk{pgDocumentChunk(92, 0.4)}))
	_, err = publisher.GetDocument(ctx, gen, "chat:sequence:late")
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestDocumentPublicationPreservesMatchingVectorsPostgreSQL(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	const scope = "chat:preserve:day"
	first := pgDocumentPublication("chat:preserve:first", "revision-a", 1, 91)
	second := pgDocumentPublication("chat:preserve:second", "revision-a", 1, 92)
	require.NoError(t, b.PublishScope(ctx, gen, scope, 1,
		[]vector.DocumentPublication{first, second},
		[]vector.Chunk{pgDocumentChunk(91, 0.1), pgDocumentChunk(92, 0.2)}))

	first.SourceSequence = 2
	first.PreserveVectors = true
	second.SourceSequence = 2
	second.Revision = "revision-b"
	require.NoError(t, b.PublishScope(ctx, gen, scope, 2,
		[]vector.DocumentPublication{first, second}, []vector.Chunk{pgDocumentChunk(92, 0.3)}))
	var firstVectors, secondVectors int
	require.NoError(t, b.DB().QueryRow(`SELECT COUNT(*) FROM embeddings
		WHERE generation_id = $1 AND message_id = $2`, int64(gen), 91).Scan(&firstVectors))
	require.NoError(t, b.DB().QueryRow(`SELECT COUNT(*) FROM embeddings
		WHERE generation_id = $1 AND message_id = $2`, int64(gen), 92).Scan(&secondVectors))
	assert.Equal(t, 1, firstVectors)
	assert.Equal(t, 1, secondVectors)

	first.SourceSequence = 3
	first.Revision = "wrong-revision"
	second.SourceSequence = 3
	err = b.PublishScope(ctx, gen, scope, 3,
		[]vector.DocumentPublication{first, second}, []vector.Chunk{pgDocumentChunk(92, 0.4)})
	require.ErrorIs(t, err, vector.ErrDocumentFenceChanged)
}

func TestDocumentSequenceFencePostgreSQL(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	const scope = "chat:fence:day"
	initial := pgDocumentPublication("chat:fence:window", "revision-a", 1, 91)
	require.NoError(t, b.PublishScope(ctx, gen, scope, 1,
		[]vector.DocumentPublication{initial}, []vector.Chunk{pgDocumentChunk(91, 0.1)}))

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
		WHERE generation_id = $1 AND message_id = 91`, int64(gen)).Scan(&vectorRows))
	assert.Equal(t, 1, vectorRows, "a fence-only publication must preserve vectors")

	stale := pgDocumentPublication(initial.Key, "revision-b", 2, 91)
	require.NoError(t, b.PublishScope(ctx, gen, scope, 2,
		[]vector.DocumentPublication{stale}, []vector.Chunk{pgDocumentChunk(91, 0.9)}))
	got, err = b.GetDocument(ctx, gen, initial.Key)
	require.NoError(t, err)
	assert.Equal(t, "revision-a", got.PublishedRevision, "the newer fence must reject delayed vectors")

	const racedScope = "chat:fence:raced"
	racedA := pgDocumentPublication("chat:fence:raced-window", "revision-a", 1, 92)
	require.NoError(t, b.PublishScope(ctx, gen, racedScope, 1,
		[]vector.DocumentPublication{racedA}, []vector.Chunk{pgDocumentChunk(92, 0.1)}))
	racedB := pgDocumentPublication(racedA.Key, "revision-b", 2, 92)
	require.NoError(t, b.PublishScope(ctx, gen, racedScope, 2,
		[]vector.DocumentPublication{racedB}, []vector.Chunk{pgDocumentChunk(92, 0.9)}))
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

func TestDocumentPublicationPostgreSQL(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	publisher := vector.DocumentPublisher(b)

	first := pgDocumentPublication("chat:1:a", "r1", 9, 7, 8)
	firstChunk := pgDocumentChunk(7, 0.1)
	require.NoError(t, publisher.PublishScope(ctx, gen, "chat:1:day", 9, []vector.DocumentPublication{first}, []vector.Chunk{
		firstChunk, pgDocumentChunk(8, 0.2),
	}))
	assertScoredChunkBasisPostgreSQL(t, b, gen, firstChunk, vector.SourceBasisBody)
	got, err := publisher.GetDocument(ctx, gen, first.Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentCurrent, got.State)
	assert.Equal(t, "r1", got.PublishedRevision)
	assert.Equal(t, int64(9), got.SourceSequence)
	assert.Equal(t, []int64{7, 8}, got.Members)
	require.Error(t, publisher.PublishScope(ctx, gen, "chat:1:day", 10, []vector.DocumentPublication{first}, []vector.Chunk{
		pgDocumentChunk(7, 0.1), pgDocumentChunk(8, 0.2),
	}), "per-document sequence must match the scope publication sequence")

	// Replay is idempotent, then a split and merge replace ownership.
	require.NoError(t, publisher.PublishScope(ctx, gen, "chat:1:day", 9, []vector.DocumentPublication{first}, []vector.Chunk{
		pgDocumentChunk(7, 0.1), pgDocumentChunk(8, 0.2),
	}))
	left := pgDocumentPublication("chat:1:left", "r2", 10, 7)
	right := pgDocumentPublication("chat:1:right", "r2", 10, 8)
	require.NoError(t, publisher.PublishScope(ctx, gen, "chat:1:day", 10, []vector.DocumentPublication{left, right}, []vector.Chunk{
		pgDocumentChunk(7, 0.3), pgDocumentChunk(8, 0.4),
	}))
	retired, err := publisher.GetDocument(ctx, gen, first.Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, retired.State)

	merged := pgDocumentPublication("chat:1:merged", "r3", 11, 7, 8)
	require.NoError(t, publisher.PublishScope(ctx, gen, "chat:1:day", 11, []vector.DocumentPublication{merged}, []vector.Chunk{
		pgDocumentChunk(7, 0.5), pgDocumentChunk(8, 0.6),
	}))
	for _, messageID := range []int64{7, 8} {
		var owners int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM embedding_document_members WHERE generation_id = $1 AND message_id = $2`,
			int64(gen), messageID).Scan(&owners))
		assert.Equal(t, 1, owners)
	}
}

func TestDocumentReassignmentRollbackAndLifecyclePostgreSQL(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	publisher := vector.DocumentPublisher(b)

	old := pgDocumentPublication("old-anchor", "r1", 1, 21)
	require.NoError(t, publisher.PublishScope(ctx, gen, "old-scope", 1, []vector.DocumentPublication{old}, []vector.Chunk{pgDocumentChunk(21, 0.1)}))
	newDoc := pgDocumentPublication("new-anchor", "r2", 2, 21)
	require.NoError(t, publisher.PublishScope(ctx, gen, "new-scope", 2, []vector.DocumentPublication{newDoc}, []vector.Chunk{pgDocumentChunk(21, 0.9)}))
	require.NoError(t, publisher.PublishScope(ctx, gen, "old-scope", 12, nil, nil))
	require.NoError(t, publisher.PublishScope(ctx, gen, "old-scope", 12, nil, nil), "empty replay is idempotent")
	tombstone, err := publisher.GetDocument(ctx, gen, old.Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, tombstone.State)
	assert.Equal(t, int64(12), tombstone.SourceSequence)
	var vectorRows int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1 AND message_id = $2`,
		int64(gen), int64(21)).Scan(&vectorRows))
	assert.Equal(t, 1, vectorRows)

	stable := pgDocumentPublication("stable", "r1", 3, 31)
	require.NoError(t, publisher.PublishScope(ctx, gen, "stable-scope", 3, []vector.DocumentPublication{stable}, []vector.Chunk{pgDocumentChunk(31, 0.1)}))
	replacement := pgDocumentPublication("replacement", "r2", 4, 31)
	err = publisher.PublishScope(ctx, gen, "stable-scope", 4, []vector.DocumentPublication{replacement}, []vector.Chunk{
		pgDocumentChunk(31, 0.2), pgDocumentChunk(31, 0.3),
	})
	require.Error(t, err)
	got, getErr := publisher.GetDocument(ctx, gen, stable.Key)
	require.NoError(t, getErr)
	assert.Equal(t, vector.DocumentCurrent, got.State)

	require.NoError(t, b.RetireGeneration(ctx, gen, false))
	err = publisher.PublishScope(ctx, gen, "stable-scope", 4, []vector.DocumentPublication{replacement}, []vector.Chunk{pgDocumentChunk(31, 0.2)})
	require.ErrorIs(t, err, vector.ErrGenerationRetired)
}

func TestDocumentScopeBatchPublicationPostgreSQL(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	publisher := vector.DocumentPublisher(b)

	scopes := []vector.DocumentScopePublication{
		{ScopeKey: "scope:a", SourceSequence: 1,
			Documents: []vector.DocumentPublication{pgDocumentPublication("a", "r1", 1, 71)},
			Chunks:    []vector.Chunk{pgDocumentChunk(71, 0.1)}},
		{ScopeKey: "scope:b", SourceSequence: 2,
			Documents: []vector.DocumentPublication{pgDocumentPublication("b", "r1", 2, 72)},
			Chunks:    []vector.Chunk{pgDocumentChunk(72, 0.2)}},
	}
	require.NoError(t, publisher.PublishScopes(ctx, gen, scopes))
	for _, key := range []string{"a", "b"} {
		doc, getErr := publisher.GetDocument(ctx, gen, key)
		require.NoError(t, getErr)
		assert.Equal(t, vector.DocumentCurrent, doc.State)
	}
	var messageCount int64
	require.NoError(t, b.DB().QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&messageCount))
	assert.Equal(t, int64(2), messageCount)
	for _, tc := range []struct {
		name     string
		oldFirst bool
	}{
		{name: "new owner before old tombstone"},
		{name: "old tombstone before new owner", oldFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			moveBackend, moveCtx, moveDB := newBackendForTest(t)
			moveGen, createErr := moveBackend.CreateGeneration(moveCtx, "model", 768, "model:768:context")
			require.NoError(t, createErr)
			movePublisher := vector.DocumentPublisher(moveBackend)
			require.NoError(t, movePublisher.PublishScope(moveCtx, moveGen, "scope:old", 1,
				[]vector.DocumentPublication{pgDocumentPublication("old", "r1", 1, 81)},
				[]vector.Chunk{pgDocumentChunk(81, 0.1)}))

			newOwner := vector.DocumentScopePublication{ScopeKey: "scope:new", SourceSequence: 2,
				Documents: []vector.DocumentPublication{pgDocumentPublication("moved", "r2", 2, 81)},
				Chunks:    []vector.Chunk{pgDocumentChunk(81, 0.9)}}
			oldTombstone := vector.DocumentScopePublication{ScopeKey: "scope:old", SourceSequence: 3}
			move := []vector.DocumentScopePublication{newOwner, oldTombstone}
			if tc.oldFirst {
				move[0], move[1] = move[1], move[0]
			}
			require.NoError(t, movePublisher.PublishScopes(moveCtx, moveGen, move))

			var owner string
			require.NoError(t, moveDB.QueryRowContext(moveCtx,
				`SELECT document_key FROM embedding_document_members WHERE generation_id = $1 AND message_id = $2`,
				int64(moveGen), int64(81)).Scan(&owner))
			assert.Equal(t, "moved", owner)
			var vectorRows int
			require.NoError(t, moveDB.QueryRowContext(moveCtx,
				`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1 AND message_id = $2`,
				int64(moveGen), int64(81)).Scan(&vectorRows))
			assert.Equal(t, 1, vectorRows)
		})
	}

	bad := []vector.DocumentScopePublication{
		{ScopeKey: "scope:c", SourceSequence: 3,
			Documents: []vector.DocumentPublication{pgDocumentPublication("c", "r1", 3, 73)},
			Chunks:    []vector.Chunk{pgDocumentChunk(73, 0.3)}},
		{ScopeKey: "scope:d", SourceSequence: 4,
			Documents: []vector.DocumentPublication{pgDocumentPublication("d", "r1", 4, 74)},
			Chunks:    []vector.Chunk{pgDocumentChunk(74, 0.4), pgDocumentChunk(74, 0.5)}},
	}
	require.Error(t, publisher.PublishScopes(ctx, gen, bad))
	_, err = publisher.GetDocument(ctx, gen, "c")
	require.Error(t, err)
	_, err = publisher.GetDocument(ctx, gen, "d")
	require.Error(t, err)
}

func TestDocumentPaginationWatermarksAndBasisPostgreSQL(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	publisher := vector.DocumentPublisher(b)
	for i, key := range []string{"a", "b", "c"} {
		messageID := int64(41 + i)
		doc := pgDocumentPublication(key, "r1", int64(i+1), messageID)
		require.NoError(t, publisher.PublishScope(ctx, gen, "scope:"+key, int64(i+1), []vector.DocumentPublication{doc}, []vector.Chunk{pgDocumentChunk(messageID, 0.1)}))
	}
	page, err := publisher.ListDocumentsAfter(ctx, gen, "a", 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "b", page[0].Key)

	require.NoError(t, publisher.AdvanceDocumentChangeWatermark(ctx, gen, 12))
	require.NoError(t, publisher.AdvanceDocumentChangeWatermark(ctx, gen, 8))
	require.NoError(t, publisher.SetDocumentReconcileCursor(ctx, gen, "scope:b"))
	require.NoError(t, publisher.SetDocumentJournalCursor(ctx, gen, "12|chat:7:2026-08-08"))
	progress, err := publisher.GetDocumentProgress(ctx, gen)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentProgress{ChangeSequence: 12, ReconcileCursor: "scope:b", JournalCursor: "12|chat:7:2026-08-08"}, progress)
	minimum, tracked, err := b.MinimumDocumentChangeWatermark(ctx)
	require.NoError(t, err)
	assert.True(t, tracked)
	assert.Equal(t, int64(12), minimum)
	require.NoError(t, publisher.SetDocumentJournalCursor(ctx, gen, ""))
	require.NoError(t, publisher.ResetDocumentReconcileCursor(ctx, gen))

	ordinary := pgDocumentChunk(100, 0.2)
	ordinary.SourceBasis = vector.SourceBasisBody
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{ordinary}))
	assertScoredChunkBasisPostgreSQL(t, b, gen, ordinary, vector.SourceBasisBody)
	var basis int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT source_basis FROM embeddings WHERE generation_id = $1 AND message_id = $2`,
		int64(gen), int64(100)).Scan(&basis))
	assert.Equal(t, int(vector.SourceBasisBody), basis)
}

func TestDocumentJournalDisablesAfterLastContextualGenerationPostgreSQL(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	_, err := db.Exec(`
		CREATE TABLE embedding_change_clock (
			singleton SMALLINT PRIMARY KEY CHECK (singleton = 1),
			sequence BIGINT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT FALSE
		);
		INSERT INTO embedding_change_clock (singleton, sequence) VALUES (1, 0);
		CREATE TABLE embedding_changes (
			sequence BIGINT PRIMARY KEY,
			kind TEXT NOT NULL
		);`)
	require.NoError(t, err)
	seedJournal := func() {
		t.Helper()
		_, seedErr := db.Exec(`
			UPDATE embedding_change_clock SET enabled = TRUE, sequence = sequence + 1 WHERE singleton = 1;
			INSERT INTO embedding_changes (sequence, kind)
			SELECT sequence, 'message_update' FROM embedding_change_clock WHERE singleton = 1;`)
		require.NoError(t, seedErr)
	}
	assertDisabledAndEmpty := func() {
		t.Helper()
		var enabled bool
		var rows int
		require.NoError(t, db.QueryRow(
			`SELECT enabled FROM embedding_change_clock WHERE singleton = 1`).Scan(&enabled))
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM embedding_changes`).Scan(&rows))
		assert.False(t, enabled)
		assert.Zero(t, rows)
	}
	assertEnabledAndRetained := func() {
		t.Helper()
		var enabled bool
		var rows int
		require.NoError(t, db.QueryRow(
			`SELECT enabled FROM embedding_change_clock WHERE singleton = 1`).Scan(&enabled))
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM embedding_changes`).Scan(&rows))
		assert.True(t, enabled)
		assert.Equal(t, 1, rows)
	}

	retired, err := b.CreateGeneration(ctx, "context", 768, "context:retire")
	require.NoError(t, err)
	require.NoError(t, b.AdvanceDocumentChangeWatermark(ctx, retired, 1))
	seedJournal()
	require.NoError(t, b.CleanupDocumentJournalIfUnused(ctx))
	assertEnabledAndRetained()
	require.NoError(t, b.RetireGeneration(ctx, retired, false))
	assertDisabledAndEmpty()

	contextual, err := b.CreateGeneration(ctx, "context", 768, "context:replace")
	require.NoError(t, err)
	require.NoError(t, b.AdvanceDocumentChangeWatermark(ctx, contextual, 2))
	require.NoError(t, b.ActivateGeneration(ctx, contextual, true))
	seedJournal()
	ordinary, err := b.CreateGeneration(ctx, "ordinary", 768, "ordinary:replace")
	require.NoError(t, err)
	require.NoError(t, b.ActivateGeneration(ctx, ordinary, true))
	assertDisabledAndEmpty()
}

func TestDocumentJournalCleanupFailureDoesNotMaskLifecyclePostgreSQL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b, ctx, db := newBackendForTest(t)
	_, err := db.Exec(`
		CREATE TABLE embedding_change_clock (
			singleton SMALLINT PRIMARY KEY CHECK (singleton = 1),
			sequence BIGINT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT FALSE
		);
		INSERT INTO embedding_change_clock (singleton, sequence) VALUES (1, 0);
		CREATE TABLE embedding_changes (
			sequence BIGINT PRIMARY KEY,
			kind TEXT NOT NULL
		);
		CREATE FUNCTION fail_contextual_journal_cleanup() RETURNS trigger AS $$
		BEGIN
			IF NEW.sequence = OLD.sequence THEN
				RAISE EXCEPTION 'synthetic cleanup failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;`)
	require.NoError(err)
	installCleanupFailure := func() {
		t.Helper()
		_, triggerErr := db.Exec(`
			CREATE TRIGGER fail_contextual_journal_cleanup
			BEFORE UPDATE OF sequence ON embedding_change_clock
			FOR EACH ROW EXECUTE FUNCTION fail_contextual_journal_cleanup()`)
		require.NoError(triggerErr)
	}
	removeCleanupFailure := func() {
		t.Helper()
		_, triggerErr := db.Exec(`DROP TRIGGER fail_contextual_journal_cleanup ON embedding_change_clock`)
		require.NoError(triggerErr)
	}
	seedJournal := func() {
		t.Helper()
		_, seedErr := db.Exec(`
			UPDATE embedding_change_clock SET enabled = TRUE, sequence = sequence + 1 WHERE singleton = 1;
			INSERT INTO embedding_changes (sequence, kind)
			SELECT sequence, 'message_update' FROM embedding_change_clock WHERE singleton = 1;`)
		require.NoError(seedErr)
	}

	retired, err := b.CreateGeneration(ctx, "context", 768, "context:cleanup-retire")
	require.NoError(err)
	require.NoError(b.AdvanceDocumentChangeWatermark(ctx, retired, 1))
	seedJournal()
	installCleanupFailure()
	require.NoError(b.RetireGeneration(ctx, retired, false),
		"a committed retirement must not be reported as failed")
	removeCleanupFailure()
	require.NoError(b.CleanupDocumentJournalIfUnused(ctx),
		"cleanup remains independently retryable")

	contextual, err := b.CreateGeneration(ctx, "context", 768, "context:cleanup-activate")
	require.NoError(err)
	require.NoError(b.AdvanceDocumentChangeWatermark(ctx, contextual, 2))
	require.NoError(b.ActivateGeneration(ctx, contextual, true))
	seedJournal()
	ordinary, err := b.CreateGeneration(ctx, "ordinary", 768, "ordinary:cleanup-activate")
	require.NoError(err)
	installCleanupFailure()
	require.NoError(b.ActivateGeneration(ctx, ordinary, true),
		"a committed activation must not be reported as failed")
	active, err := b.ActiveGeneration(ctx)
	require.NoError(err)
	assert.Equal(ordinary, active.ID)
	removeCleanupFailure()
	require.NoError(b.CleanupDocumentJournalIfUnused(ctx),
		"activation cleanup remains independently retryable")
}

func TestDocumentMigrationUpgradePostgreSQL(t *testing.T) {
	ctx := context.Background()
	db := openPGTestDB(t)
	_, err := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		CREATE TABLE index_generations (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			model TEXT NOT NULL, dimension INTEGER NOT NULL, fingerprint TEXT NOT NULL,
			started_at BIGINT NOT NULL, seeded_at BIGINT, completed_at BIGINT,
			activated_at BIGINT, state TEXT NOT NULL, message_count BIGINT NOT NULL DEFAULT 0
		);
		CREATE TABLE embeddings (
			generation_id BIGINT NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
			message_id BIGINT NOT NULL, chunk_index INTEGER NOT NULL DEFAULT 0,
			embedded_at BIGINT NOT NULL, source_char_len INTEGER NOT NULL,
			chunk_char_start INTEGER NOT NULL DEFAULT 0,
			chunk_char_end INTEGER NOT NULL DEFAULT 0,
			truncated BOOLEAN NOT NULL DEFAULT FALSE,
			dimension INTEGER NOT NULL, embedding vector NOT NULL,
			PRIMARY KEY (generation_id, message_id, chunk_index)
		);
		CREATE TABLE embedding_document_progress (
			generation_id BIGINT PRIMARY KEY REFERENCES index_generations(id) ON DELETE CASCADE,
			change_sequence BIGINT NOT NULL DEFAULT 0,
			reconcile_cursor TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO index_generations
			(model, dimension, fingerprint, started_at, state)
			VALUES ('m', 2, 'm:2', 1, 'active');
		INSERT INTO embeddings
			(generation_id, message_id, embedded_at, source_char_len, dimension, embedding)
			VALUES (1, 99, 1, 4, 2, '[0.1,0.2]');
		INSERT INTO embedding_document_progress
			(generation_id, change_sequence, reconcile_cursor)
			VALUES (1, 7, 'done:7');
	`)
	require.NoError(t, err)
	require.NoError(t, Migrate(ctx, db, 0, true))
	require.NoError(t, Migrate(ctx, db, 0, true), "migration must be idempotent")
	for _, table := range []string{"embedding_documents", "embedding_document_scopes", "embedding_document_members", "embedding_document_progress"} {
		var exists bool
		require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists))
		assert.True(t, exists, table)
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
	require.NoError(t, Migrate(ctx, db, 0, true), "migration must restore and backfill scope sequence state")
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
	b := &Backend{db: db}
	contextual := pgDocumentChunk(98, 0.3)
	contextual.Vector = contextual.Vector[:2]
	require.NoError(t, b.PublishScope(ctx, 1, "chat:98:day", 2,
		[]vector.DocumentPublication{pgDocumentPublication("chat:98", "r2", 2, 98)},
		[]vector.Chunk{contextual}))
	assertScoredChunkBasisPostgreSQL(t, b, 1, contextual, vector.SourceBasisBody)
	ordinary := pgDocumentChunk(100, 0.4)
	ordinary.Vector = ordinary.Vector[:2]
	ordinary.SourceBasis = vector.SourceBasisSubjectBody
	require.NoError(t, b.Upsert(ctx, 1, []vector.Chunk{ordinary}))
	assertScoredChunkBasisPostgreSQL(t, b, 1, ordinary, vector.SourceBasisSubjectBody)
}

func assertScoredChunkBasisPostgreSQL(t *testing.T, b *Backend, gen vector.GenerationID, chunk vector.Chunk, want vector.SourceBasis) {
	t.Helper()
	hits, err := b.ScoreMessageChunks(context.Background(), gen, chunk.MessageID, chunk.Vector)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, want, hits[0].SourceBasis)
}
