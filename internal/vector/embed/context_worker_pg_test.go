//go:build pgvector

package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

type pgContextSemanticFake struct {
	dim        int
	before     func()
	docs       int
	rejectText string
}

type pgContextPartialSemanticFake struct {
	calls [][]DocumentInput
}

func (c *pgContextSemanticFake) EmbedQuery(context.Context, string) ([]float32, error) {
	return make([]float32, c.dim), nil
}

func (c *pgContextPartialSemanticFake) EmbedQuery(context.Context, string) ([]float32, error) {
	return make([]float32, 4), nil
}

func (c *pgContextPartialSemanticFake) EmbedDocuments(_ context.Context, documents []DocumentInput) ([][][]float32, error) {
	c.calls = append(c.calls, append([]DocumentInput(nil), documents...))
	result := func(document DocumentInput) [][]float32 {
		vectors := make([][]float32, len(document.Chunks))
		for i := range vectors {
			vectors[i] = []float32{1, 2, 3, 4}
		}
		return vectors
	}
	if len(c.calls) == 1 {
		return [][][]float32{result(documents[0])}, errors.New("synthetic later packed request failure")
	}
	out := make([][][]float32, len(documents))
	for i := range documents {
		out[i] = result(documents[i])
	}
	return out, nil
}

func (c *pgContextSemanticFake) EmbedDocuments(_ context.Context, documents []DocumentInput) ([][][]float32, error) {
	if c.before != nil {
		c.before()
	}
	if c.rejectText != "" {
		for _, document := range documents {
			for _, chunk := range document.Chunks {
				if strings.Contains(chunk, c.rejectText) {
					return nil, fmt.Errorf("%w: synthetic PostgreSQL leaf", ErrDocumentTooLarge)
				}
			}
		}
	}
	c.docs += len(documents)
	out := make([][][]float32, len(documents))
	for i, document := range documents {
		out[i] = make([][]float32, len(document.Chunks))
		for j := range document.Chunks {
			out[i][j] = []float32{1, 2, 3, 4}
		}
	}
	return out, nil
}

type pgContextWorkerFixture struct {
	store        *store.Store
	backend      *pgvector.Backend
	gen          vector.GenerationID
	client       *pgContextSemanticFake
	worker       *ContextWorker
	sourceID     int64
	conversation int64
	person       int64
}

func newPGContextWorkerFixture(t *testing.T, mutate func(*ContextWorkerDeps)) *pgContextWorkerFixture {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("MSGVAULT_TEST_DB is required")
	}
	st := testutil.NewTestStore(t)
	var schemaName string
	require.NoError(t, st.DB().QueryRow(`SELECT current_schema()`).Scan(&schemaName))
	st.DB().SetMaxOpenConns(1)
	_, err := st.DB().Exec(`SELECT set_config('search_path', $1, false)`, schemaName+",public")
	require.NoError(t, err)
	backend, err := pgvector.Open(context.Background(), pgvector.Options{DB: st.DB(), Dimension: 4, SkipExtension: true})
	require.NoError(t, err)
	gen, err := backend.CreateGeneration(context.Background(), "synthetic", 4, "synthetic:4")
	require.NoError(t, err)
	client := &pgContextSemanticFake{dim: 4}
	deps := ContextWorkerDeps{
		Backend: backend, Publisher: backend, Store: st, Client: client,
		Assembler:          CompositeAssembler{Policy: AssemblyPolicy{MaxChunkRunes: 100}, Chat: ChatWindowAssembler{Policy: AssemblyPolicy{MaxChunkRunes: 100, ChatGap: 30 * time.Minute}}},
		ChangeBatchSize:    1,
		ReconcileBatchSize: 1,
		Recorder:           newTestOperationRecorder(),
	}
	if mutate != nil {
		mutate(&deps)
	}
	source, err := st.GetOrCreateSource("test", fmt.Sprintf("pg-context-worker-%d", time.Now().UnixNano()))
	require.NoError(t, err)
	conversation, err := st.EnsureConversationWithType(source.ID, "chat", "beeper", "PG chat")
	require.NoError(t, err)
	person, err := st.EnsureParticipant(fmt.Sprintf("pg-%d@example.test", time.Now().UnixNano()), "PG Person", "example.test")
	require.NoError(t, err)
	require.NoError(t, st.EnsureConversationParticipant(conversation, person, "member"))
	return &pgContextWorkerFixture{store: st, backend: backend, gen: gen, client: client,
		worker: NewContextWorker(deps), sourceID: source.ID, conversation: conversation, person: person}
}

func (f *pgContextWorkerFixture) seed(t *testing.T, sourceMessageID string) int64 {
	t.Helper()
	id, err := f.store.UpsertMessage(&store.Message{
		ConversationID: f.conversation, SourceID: f.sourceID, SourceMessageID: sourceMessageID,
		MessageType: "beeper", SentAt: sql.NullTime{Time: time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), Valid: true},
		SenderID: sql.NullInt64{Int64: f.person, Valid: true},
	})
	require.NoError(t, err)
	require.NoError(t, f.store.UpsertMessageBody(id, sql.NullString{String: "hello from PostgreSQL", Valid: true}, sql.NullString{}))
	return id
}

func TestContextWorker_PostgreSQLParity(t *testing.T) {
	f := newPGContextWorkerFixture(t, nil)
	messageID := f.seed(t, "pg-chat-1")
	result, err := f.worker.RunOnce(context.Background(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.True(t, result.Contextual.Converged)
	docs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen, fmt.Sprintf("chat:%d:2026-08-08", f.conversation))
	require.NoError(t, err)
	require.Len(t, docs, 1)
	assert.Equal(t, []int64{messageID}, docs[0].Members)
}

func TestContextWorker_PostgreSQLBackstopTimestampOnlyChatChangeDoesNotCallProvider(t *testing.T) {
	assert := assert.New(t)
	f := newPGContextWorkerFixture(t, nil)
	messageID := f.seed(t, "pg-chat-timestamp-only")
	result, err := f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	require.True(t, result.Contextual.Converged)
	documents, err := f.backend.ListDocumentsForScope(t.Context(), f.gen,
		fmt.Sprintf("chat:%d:2026-08-08", f.conversation))
	require.NoError(t, err)
	require.Len(t, documents, 1)
	beforeRevision := documents[0].PublishedRevision
	beforeDocuments := f.client.docs

	_, err = f.store.DB().Exec(
		`UPDATE messages SET last_modified = '2099-01-01 00:00:00+00' WHERE id = $1`, messageID)
	require.NoError(t, err)
	result, err = f.worker.RunBackstop(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.True(result.Contextual.Converged)
	assert.Equal(beforeDocuments, f.client.docs,
		"LastModified remains a CAS fence but must not change the semantic document revision")
	documents, err = f.backend.ListDocumentsForScope(t.Context(), f.gen,
		fmt.Sprintf("chat:%d:2026-08-08", f.conversation))
	require.NoError(t, err)
	require.Len(t, documents, 1)
	assert.Equal(beforeRevision, documents[0].PublishedRevision)
}

func TestContextWorker_PostgreSQLLaterPackedFailureRetainsSuccessfulDocumentWithinScope(t *testing.T) {
	assert := assert.New(t)
	client := &pgContextPartialSemanticFake{}
	f := newPGContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.Client = client
		deps.ChangeBatchSize = 2
		deps.ReconcileBatchSize = 2
	})
	firstID := f.seed(t, "pg-chat-prefix-1")
	secondID := f.seed(t, "pg-chat-prefix-2")
	_, err := f.store.DB().Exec(`UPDATE messages SET sent_at = $1 WHERE id = $2`,
		time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC), secondID)
	require.NoError(t, err)

	_, err = f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.Error(t, err)
	documents, err := f.backend.ListDocumentsForScope(t.Context(), f.gen,
		fmt.Sprintf("chat:%d:2026-08-08", f.conversation))
	require.NoError(t, err)
	assert.Len(documents, 1)
	var covered int
	require.NoError(t, f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE id IN ($1, $2) AND embed_gen = $3`,
		firstID, secondID, int64(f.gen)).Scan(&covered))
	assert.Zero(covered, "an incomplete atomic scope must remain uncovered")

	result, err := f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.True(result.Contextual.Converged)
	require.Len(t, client.calls, 2)
	assert.Len(client.calls[0], 2)
	assert.Len(client.calls[1], 1, "PostgreSQL must preserve the successful first document on retry")
	documents, err = f.backend.ListDocumentsForScope(t.Context(), f.gen,
		fmt.Sprintf("chat:%d:2026-08-08", f.conversation))
	require.NoError(t, err)
	assert.Len(documents, 2)
}

func TestContextWorker_PostgreSQLLaterPackedReplacementFailurePreservesUnresolvedExistingDocument(t *testing.T) {
	assert := assert.New(t)
	f := newPGContextWorkerFixture(t, nil)
	firstID := f.seed(t, "pg-chat-replacement-prefix-1")
	secondID := f.seed(t, "pg-chat-replacement-prefix-2")
	_, err := f.store.DB().Exec(`UPDATE messages SET sent_at = $1 WHERE id = $2`,
		time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC), secondID)
	require.NoError(t, err)
	result, err := f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	require.True(t, result.Contextual.Converged)
	scopeKey := fmt.Sprintf("chat:%d:2026-08-08", f.conversation)
	before, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, scopeKey)
	require.NoError(t, err)
	require.Len(t, before, 2)
	beforeRevisions := make(map[string]string, len(before))
	for _, document := range before {
		beforeRevisions[document.Key] = document.PublishedRevision
	}

	client := &pgContextPartialSemanticFake{}
	f.worker = NewContextWorker(ContextWorkerDeps{
		Backend: f.backend, Publisher: f.backend, Store: f.store,
		Assembler: CompositeAssembler{
			Policy: AssemblyPolicy{MaxChunkRunes: 200, ChatGap: 30 * time.Minute},
			Chat:   ChatWindowAssembler{Policy: AssemblyPolicy{MaxChunkRunes: 200, ChatGap: 30 * time.Minute}},
		},
		Client: client, ChangeBatchSize: 2, ReconcileBatchSize: 2,
		Recorder: newTestOperationRecorder(),
	})
	require.NoError(t, f.store.UpsertMessageBody(firstID,
		sql.NullString{String: "first PostgreSQL replacement", Valid: true}, sql.NullString{}))
	require.NoError(t, f.store.UpsertMessageBody(secondID,
		sql.NullString{String: "second PostgreSQL replacement", Valid: true}, sql.NullString{}))

	_, err = f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.Error(t, err)
	afterPartial, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, scopeKey)
	require.NoError(t, err)
	require.Len(t, afterPartial, 2,
		"PostgreSQL must not tombstone the unresolved document's existing vectors")
	changed := 0
	unchanged := 0
	for _, document := range afterPartial {
		if document.PublishedRevision == beforeRevisions[document.Key] {
			unchanged++
		} else {
			changed++
		}
	}
	assert.Equal(1, changed)
	assert.Equal(1, unchanged)
}

func TestContextWorker_PostgreSQLRejectedReplacementClearsCoverage(t *testing.T) {
	f := newPGContextWorkerFixture(t, nil)
	messageID := f.seed(t, "pg-chat-rejected-replacement")
	result, err := f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	require.True(t, result.Contextual.Converged)

	f.client.rejectText = "hello from PostgreSQL"
	_, err = f.store.DB().Exec(`UPDATE conversations SET title = 'Changed title' WHERE id = $1`, f.conversation)
	require.NoError(t, err)
	result, err = f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.False(t, result.Contextual.Converged)
	missing, err := f.store.MissingCount(t.Context(), int64(f.gen))
	require.NoError(t, err)
	assert.Equal(t, int64(1), missing)
	var embedGen sql.NullInt64
	require.NoError(t, f.store.DB().QueryRow(`SELECT embed_gen FROM messages WHERE id = $1`, messageID).Scan(&embedGen))
	assert.False(t, embedGen.Valid)
	docs, err := f.backend.ListDocumentsForScope(t.Context(), f.gen,
		chatDayContextScope(f.conversation, time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)).key)
	require.NoError(t, err)
	assert.Empty(t, docs)
}

func TestContextWorker_PostgreSQLRejectedDiscoveryDoesNotStarveLaterMessages(t *testing.T) {
	f := newPGContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 4
		deps.ReconcileBatchSize = 4
		deps.MaxRunUTF8Bytes = 1
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "ordinary", "Ordinary")
	require.NoError(t, err)
	seed := func(sourceID, body string, minute int) int64 {
		id, seedErr := f.store.UpsertMessage(&store.Message{
			ConversationID: conversationID, SourceID: f.sourceID, SourceMessageID: sourceID,
			MessageType: "email", Subject: sql.NullString{String: "subject", Valid: true},
			SentAt:   sql.NullTime{Time: time.Date(2026, 8, 8, 9, minute, 0, 0, time.UTC), Valid: true},
			SenderID: sql.NullInt64{Int64: f.person, Valid: true},
		})
		require.NoError(t, seedErr)
		require.NoError(t, f.store.UpsertMessageBody(id,
			sql.NullString{String: body, Valid: true}, sql.NullString{}))
		return id
	}
	seed("pg-reject-first", "reject-first", 0)
	seed("pg-reject-second", "reject-second", 1)
	goodID := seed("pg-later-valid", "later-valid-document", 2)
	f.client.rejectText = "reject-"

	var result RunResult
	for range 3 {
		result, err = f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
		require.NoError(t, err)
	}
	good, err := f.backend.GetDocument(t.Context(), f.gen, fmt.Sprintf("message:%d", goodID))
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentCurrent, good.State)
	missing, err := f.store.MissingCount(t.Context(), int64(f.gen))
	require.NoError(t, err)
	assert.Equal(t, int64(2), missing)
	for attempt := 0; attempt < 10 && !result.Contextual.Converged; attempt++ {
		result, err = f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
		require.NoError(t, err)
	}
	assert.True(t, result.Contextual.Converged,
		"durable discovery and reconciliation cursors must let the CLI loop terminate")
}

func TestContextWorker_PostgreSQLReplaysAfterIndexCommit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	crashed := false
	f := newPGContextWorkerFixture(t, func(d *ContextWorkerDeps) {
		d.Hooks.AfterPublish = func() error {
			if crashed {
				return nil
			}
			crashed = true
			return errors.New("synthetic PostgreSQL crash")
		}
	})
	id := f.seed(t, "pg-replay")
	pinnedSequence, err := f.store.LatestEmbeddingChangeSequence(t.Context())
	require.NoError(err)
	_, err = f.worker.RunOnce(context.Background(), f.gen, testEmbeddingPassScope())
	require.Error(err)
	progress, err := f.backend.GetDocumentProgress(context.Background(), f.gen)
	require.NoError(err)
	assert.Equal(pinnedSequence, progress.ChangeSequence)

	result, err := f.worker.RunOnce(context.Background(), f.gen, testEmbeddingPassScope())
	require.NoError(err)
	assert.True(result.Contextual.Converged)
	docs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen, fmt.Sprintf("chat:%d:2026-08-08", f.conversation))
	require.NoError(err)
	require.Len(docs, 1)
	assert.Equal([]int64{id}, docs[0].Members)
}

func TestContextWorker_PostgreSQLGroupCASMissHoldsWatermark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPGContextWorkerFixture(t, nil)
	f.seed(t, "pg-cas")
	pinnedSequence, err := f.store.LatestEmbeddingChangeSequence(t.Context())
	require.NoError(err)
	f.client.before = func() {
		f.client.before = nil
		_, err := f.store.DB().Exec(`UPDATE conversations SET title = 'raced in PostgreSQL' WHERE id = $1`, f.conversation)
		require.NoError(err)
	}

	_, err = f.worker.RunOnce(context.Background(), f.gen, testEmbeddingPassScope())
	require.Error(err)
	assert.Contains(err.Error(), "coverage CAS")
	progress, err := f.backend.GetDocumentProgress(context.Background(), f.gen)
	require.NoError(err)
	assert.Equal(pinnedSequence, progress.ChangeSequence)
	latestSequence, err := f.store.LatestEmbeddingChangeSequence(t.Context())
	require.NoError(err)
	assert.Greater(latestSequence, progress.ChangeSequence)
	var missing int
	require.NoError(f.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE embed_gen IS NULL`).Scan(&missing))
	assert.Equal(1, missing)
}
