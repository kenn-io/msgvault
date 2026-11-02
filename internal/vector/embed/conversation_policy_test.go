//go:build sqlite_vec

package embed

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func TestWorkerColdPolicyFenceBeforeRequestAndPublication(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	_, policyErr := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	require.NoError(t, policyErr)
	src, err := st.GetOrCreateSource("beeper", "worker-policy")
	require.NoError(t, err)
	room, err := st.EnsureConversation(src.ID, "room", "Room")
	require.NoError(t, err)
	_, err = st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper", Subject: sql.NullString{String: "Policy message", Valid: true}})
	require.NoError(t, err)
	client := &fakeEmbeddingClient{dim: 4}
	w := NewWorker(WorkerDeps{Store: st, MainDB: st.DB(), Client: client, BatchSize: 1})
	ids, err := st.ScanForEmbedding(t.Context(), int64(1), 0, 10)
	require.NoError(t, err)
	assert.Empty(t, ids)
	// A batch scanned before demotion still cannot hydrate cold text.
	var id int64
	require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages LIMIT 1`).Scan(&id))
	result, err := w.embedBatch(t.Context(), []int64{id})
	require.NoError(t, err)
	assert.Empty(t, result.chunks)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	ctx, err := st.CaptureConversationPolicyWork(t.Context())
	require.NoError(t, err)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Cold))
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	_, err = w.embedBatch(ctx, []int64{id})
	require.ErrorIs(t, err, store.ErrConversationPolicyStale)
}

func TestEmbedBatchDistinguishesPolicyFilteredMessagesFromMissing(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "filtered-policy")
	require.NoError(t, err)
	room, err := st.EnsureConversation(src.ID, "room", "Room")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: room, SourceMessageID: "message",
		MessageType: "beeper", Subject: sql.NullString{String: "Synthetic body", Valid: true},
	})
	require.NoError(t, err)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	work, err := st.CaptureConversationPolicyWork(t.Context())
	require.NoError(t, err)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Cold))

	w := NewWorker(WorkerDeps{Store: st, MainDB: st.DB(), Client: &fakeEmbeddingClient{dim: 4}})
	missingID := id + 1000
	result, err := w.embedBatch(work, []int64{id, missingID})
	require.NoError(t, err)
	assert.Empty(t, result.chunks)
	assert.Equal(t, []int64{missingID}, result.missing, "only a physically absent message is missing")
}

func TestWorkerSkipStampRejectsStalePolicyAndPreservesVectors(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "skip-policy")
	require.NoError(t, err)
	room, err := st.EnsureConversation(src.ID, "room", "Room")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: room, SourceMessageID: "empty-message",
		MessageType: "beeper", Subject: sql.NullString{String: "", Valid: true},
	})
	require.NoError(t, err)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	b, err := sqlitevec.Open(t.Context(), sqlitevec.Options{
		Path: filepath.Join(t.TempDir(), "vectors.db"), MainDB: st.DB(), PolicyStore: st, Dimension: 4,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	gen, err := b.CreateGeneration(t.Context(), "skip-policy", 4, "")
	require.NoError(t, err)
	require.NoError(t, b.Upsert(t.Context(), gen, []vector.Chunk{{
		MessageID: id, Vector: []float32{1, 0, 0, 0},
	}}))

	changedPolicy := false
	w := NewWorker(WorkerDeps{
		Store: st, MainDB: st.DB(), Backend: b, VectorsDB: b.DB(),
		Client: &fakeEmbeddingClient{dim: 4}, BatchSize: 1, MaxConsecutiveFailures: 1,
		Recorder: newTestOperationRecorder(),
		beforeSkipStamp: func(ctx context.Context, _ []int64) {
			if changedPolicy {
				return
			}
			changedPolicy = true
			require.NoError(t, st.SetConversationPolicyOverride(ctx, src.ID, "room", conversationpolicy.Cold))
			require.NoError(t, st.SetConversationPolicyOverride(ctx, src.ID, "room", conversationpolicy.Full))
		},
	})
	_, err = w.RunOnce(t.Context(), gen, testEmbeddingPassScope())
	require.ErrorIs(t, err, store.ErrConversationPolicyStale)

	var embedGen sql.NullInt64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT embed_gen FROM messages WHERE id=?`), id).Scan(&embedGen))
	assert.False(t, embedGen.Valid, "stale skip authority leaves the message pending")
	var vectorCount int
	require.NoError(t, b.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM embeddings WHERE generation_id=? AND message_id=?`, int64(gen), id).Scan(&vectorCount))
	assert.Equal(t, 1, vectorCount, "stale skip authority cannot delete the existing vector")
}

func TestWorkerRechecksMessageEligibilityBetweenSubBatches(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "subbatch-policy")
	require.NoError(t, err)
	fullRoom, err := st.EnsureConversation(src.ID, "full-room", "Full room")
	require.NoError(t, err)
	coldRoom, err := st.EnsureConversation(src.ID, "cold-room", "Cold room")
	require.NoError(t, err)
	firstID, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: fullRoom, SourceMessageID: "first", MessageType: "beeper", Subject: sql.NullString{String: strings.Repeat("first synthetic input ", 4), Valid: true}})
	require.NoError(t, err)
	secondID, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: fullRoom, SourceMessageID: "second", MessageType: "beeper", Subject: sql.NullString{String: "second synthetic input", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "full-room", conversationpolicy.Full))
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "cold-room", conversationpolicy.Cold))
	beforeRevision, err := st.ConversationPolicyRevision(t.Context())
	require.NoError(t, err)

	b, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainDB: st.DB(), PolicyStore: st, Dimension: 4})
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Close()) }()
	gen, err := b.CreateGeneration(t.Context(), "subbatch", 4, "")
	require.NoError(t, err)
	var client *fakeEmbeddingClient
	client = &fakeEmbeddingClient{dim: 4, preReturn: func() {
		if client.calls != 1 {
			return
		}
		require.Len(t, client.LastInputs, 2)
		require.Contains(t, client.LastInputs[0], "first synthetic input")
		require.Contains(t, client.LastInputs[1], "first synthetic input")
		conversationID := coldRoom
		_, moveErr := st.UpsertMessage(&store.Message{
			SourceID: src.ID, ConversationID: conversationID, SourceMessageID: "second",
			MessageType: "beeper", Subject: sql.NullString{String: "second synthetic input", Valid: true},
		})
		require.NoError(t, moveErr)
	}}
	w := NewWorker(WorkerDeps{Store: st, MainDB: st.DB(), Backend: b, VectorsDB: b.DB(), Client: client, MaxInputChars: 40, BatchSize: 2, Recorder: newTestOperationRecorder()})
	_, err = w.RunOnce(t.Context(), gen, testEmbeddingPassScope())
	require.ErrorIs(t, err, store.ErrConversationPolicyStale)
	assert.Equal(t, 1, client.calls, "moved message is rejected before its provider request")
	afterRevision, err := st.ConversationPolicyRevision(t.Context())
	require.NoError(t, err)
	assert.Greater(t, afterRevision, beforeRevision, "leaving a full room fences in-flight progress with a new policy revision")
	stats, err := b.Stats(t.Context(), gen)
	require.NoError(t, err)
	assert.Zero(t, stats.EmbeddingCount)
	var firstRoom, secondRoom int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE id=?`), firstID).Scan(&firstRoom))
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE id=?`), secondID).Scan(&secondRoom))
	assert.True(t, firstRoom == coldRoom || secondRoom == coldRoom)
}

func TestContextWorkerColdPolicyWholeScopeAndPromotion(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	_, policyErr := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	require.NoError(t, policyErr)
	src, err := st.GetOrCreateSource("beeper", "context-policy")
	require.NoError(t, err)
	room, err := st.EnsureConversation(src.ID, "room", "Room")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper", Subject: sql.NullString{String: "Context message", Valid: true}})
	require.NoError(t, err)
	snapshot, err := BeginSourceSnapshot(t.Context(), st)
	require.NoError(t, err)
	_, found, err := snapshot.Message(t.Context(), id)
	require.NoError(t, err)
	assert.False(t, found)
	scopes, _, _, scanned, err := snapshot.sourceScopesAfter(t.Context(), 0, 100, vector.BuildScope{})
	require.NoError(t, err)
	assert.Empty(t, scopes)
	assert.Zero(t, scanned)
	require.NoError(t, snapshot.Close())
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	snapshot, err = BeginSourceSnapshot(t.Context(), st)
	require.NoError(t, err)
	defer func() { require.NoError(t, snapshot.Close()) }()
	_, found, err = snapshot.Message(t.Context(), id)
	require.NoError(t, err)
	assert.True(t, found)
}

func TestWorkerColdPolicyDelayedResponseAfterPromotion(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "delayed-policy")
	require.NoError(t, err)
	room, err := st.EnsureConversation(src.ID, "room", "Room")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper", Subject: sql.NullString{String: "Delayed response", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	b, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainDB: st.DB(), PolicyStore: st, Dimension: 4})
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Close()) }()
	gen, err := b.CreateGeneration(t.Context(), "delayed", 4, "")
	require.NoError(t, err)
	client := &fakeEmbeddingClient{dim: 4, preReturn: func() {
		require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Cold))
		require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	}}
	w := NewWorker(WorkerDeps{Store: st, MainDB: st.DB(), Backend: b, VectorsDB: b.DB(), Client: client, BatchSize: 1, Recorder: newTestOperationRecorder()})
	_, err = w.RunOnce(t.Context(), gen, testEmbeddingPassScope())
	require.ErrorIs(t, err, store.ErrConversationPolicyStale)
	assert.Equal(t, 1, client.calls)
	stats, err := b.Stats(t.Context(), gen)
	require.NoError(t, err)
	assert.Zero(t, stats.EmbeddingCount)
	var stamp any
	require.NoError(t, st.DB().QueryRow(`SELECT embed_gen FROM messages WHERE id=?`, id).Scan(&stamp))
	assert.Nil(t, stamp)
}

func TestWorkerColdPolicyRejectsHTTPRetry(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "retry-policy")
	require.NoError(t, err)
	fullRoom, err := st.EnsureConversation(src.ID, "full-room", "Full room")
	require.NoError(t, err)
	coldRoom, err := st.EnsureConversation(src.ID, "cold-room", "Cold room")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: fullRoom, SourceMessageID: "retry-message", MessageType: "beeper", Subject: sql.NullString{String: "retry policy input", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "full-room", conversationpolicy.Full))
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "cold-room", conversationpolicy.Cold))
	beforeRevision, err := st.ConversationPolicyRevision(t.Context())
	require.NoError(t, err)
	b, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainDB: st.DB(), PolicyStore: st, Dimension: 4})
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Close()) }()
	gen, err := b.CreateGeneration(t.Context(), "retry-policy", 4, "")
	require.NoError(t, err)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: coldRoom, SourceMessageID: "retry-message", MessageType: "beeper", Subject: sql.NullString{String: "retry policy input", Valid: true}})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Retry-After", "0")
		http.Error(w, "retry", http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := NewClient(Config{Endpoint: server.URL, Model: "policy", Dimension: 4, MaxRetries: 3})
	worker := NewWorker(WorkerDeps{Store: st, MainDB: st.DB(), Backend: b, VectorsDB: b.DB(), Client: client, BatchSize: 1, Recorder: newTestOperationRecorder()})
	_, err = worker.RunOnce(t.Context(), gen, testEmbeddingPassScope())
	require.ErrorIs(t, err, store.ErrConversationPolicyStale)
	assert.Equal(t, int32(1), calls.Load())
	afterRevision, err := st.ConversationPolicyRevision(t.Context())
	require.NoError(t, err)
	assert.Greater(t, afterRevision, beforeRevision, "leaving a full room fences in-flight progress with a new policy revision")
	stats, err := b.Stats(t.Context(), gen)
	require.NoError(t, err)
	assert.Zero(t, stats.EmbeddingCount)
	var stamp any
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT embed_gen FROM messages WHERE id=?`), id).Scan(&stamp))
	assert.Nil(t, stamp)
}

func TestContextWorkerColdPolicyDelayedResponseAfterPromotion(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "delayed-context-policy")
	require.NoError(t, err)
	room, err := st.EnsureConversation(src.ID, "room", "Room")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "message", MessageType: "email", Subject: sql.NullString{String: "Delayed context response", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: "Synthetic contextual input", Valid: true}, sql.NullString{}))
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	var mainPath string
	require.NoError(t, st.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&mainPath))
	b, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainPath: mainPath, MainDB: st.DB(), PolicyStore: st, Dimension: 4})
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Close()) }()
	gen, err := b.CreateGeneration(t.Context(), "delayed-context", 4, "")
	require.NoError(t, err)
	client := &contextSemanticFake{dim: 4, before: func() {
		require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Cold))
		require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	}}
	w := NewContextWorker(ContextWorkerDeps{Store: st, Backend: b, Publisher: b, Client: client, Assembler: CompositeAssembler{Policy: AssemblyPolicy{MaxChunkRunes: 200}}, Recorder: newTestOperationRecorder()})
	_, err = w.RunOnce(t.Context(), gen, testEmbeddingPassScope())
	require.ErrorIs(t, err, store.ErrConversationPolicyStale)
	assert.Equal(t, 1, client.Calls())
	stats, err := b.Stats(t.Context(), gen)
	require.NoError(t, err)
	assert.Zero(t, stats.EmbeddingCount)
	var stamp any
	require.NoError(t, st.DB().QueryRow(`SELECT embed_gen FROM messages WHERE id=?`, id).Scan(&stamp))
	assert.Nil(t, stamp)
}

func TestContextWorkerColdPolicyRejectsHTTPRetry(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "context-request-policy")
	require.NoError(t, err)
	fullRoom, err := st.EnsureConversation(src.ID, "full-room", "Full room")
	require.NoError(t, err)
	coldRoom, err := st.EnsureConversation(src.ID, "cold-room", "Cold room")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: fullRoom, SourceMessageID: "message",
		MessageType: "beeper", Subject: sql.NullString{String: "Synthetic request input", Valid: true},
	})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: "Synthetic contextual body", Valid: true}, sql.NullString{}))
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "full-room", conversationpolicy.Full))
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "cold-room", conversationpolicy.Cold))
	beforeRevision, err := st.ConversationPolicyRevision(t.Context())
	require.NoError(t, err)

	var mainPath string
	require.NoError(t, st.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&mainPath))
	b, err := sqlitevec.Open(t.Context(), sqlitevec.Options{
		Path: filepath.Join(t.TempDir(), "vectors.db"), MainPath: mainPath,
		MainDB: st.DB(), PolicyStore: st, Dimension: 4,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Close()) }()
	gen, err := b.CreateGeneration(t.Context(), "context-request-policy", 4, "")
	require.NoError(t, err)

	var providerRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if providerRequests.Add(1) == 1 {
			_, err := st.DB().ExecContext(r.Context(), st.Rebind(`UPDATE messages SET conversation_id=? WHERE id=?`), coldRoom, id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Retry-After", "0")
		http.Error(w, "retry contextual request", http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := NewVoyageClient(VoyageConfig{
		Endpoint: server.URL, Model: "synthetic-context", Dimension: 4, MaxRetries: 3,
	})
	w := NewContextWorker(ContextWorkerDeps{
		Store: st, Backend: b, Publisher: b, Client: client,
		Assembler: CompositeAssembler{
			Policy: AssemblyPolicy{MaxChunkRunes: 200},
			Chat:   ChatWindowAssembler{Policy: AssemblyPolicy{MaxChunkRunes: 200}},
		},
		Recorder: newTestOperationRecorder(),
	})
	_, err = w.RunOnce(t.Context(), gen, testEmbeddingPassScope())
	require.ErrorIs(t, err, store.ErrConversationPolicyStale)
	assert.Equal(t, int32(1), providerRequests.Load(), "the retry must not send a message after it moves to a Cold room")
	var conversationID int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE id=?`), id).Scan(&conversationID))
	assert.Equal(t, coldRoom, conversationID)
	afterRevision, err := st.ConversationPolicyRevision(t.Context())
	require.NoError(t, err)
	assert.Equal(t, beforeRevision, afterRevision, "moving a message does not change the global policy revision")
	stats, err := b.Stats(t.Context(), gen)
	require.NoError(t, err)
	assert.Zero(t, stats.EmbeddingCount)
	var stamp any
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT embed_gen FROM messages WHERE id=?`), id).Scan(&stamp))
	assert.Nil(t, stamp)
}

type pausedPolicyProgressPublisher struct {
	vector.DocumentPublisher

	beforeDone func()
}

func (p *pausedPolicyProgressPublisher) SetDocumentReconcileCursor(ctx context.Context, gen vector.GenerationID, cursor string) error {
	if strings.HasPrefix(cursor, "done:") && p.beforeDone != nil {
		pause := p.beforeDone
		p.beforeDone = nil
		pause()
	}
	return p.DocumentPublisher.SetDocumentReconcileCursor(ctx, gen, cursor)
}

func TestContextWorkerColdPolicyStaleProgressAfterPromotion(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	_, policyErr := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	require.NoError(t, policyErr)
	src, err := st.GetOrCreateSource("beeper", "progress-policy")
	require.NoError(t, err)
	room, err := st.EnsureConversation(src.ID, "room", "Room")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "message", MessageType: "email", Subject: sql.NullString{String: "Progress input", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: "Synthetic progress input", Valid: true}, sql.NullString{}))
	var mainPath string
	require.NoError(t, st.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&mainPath))
	b, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainPath: mainPath, MainDB: st.DB(), PolicyStore: st, Dimension: 4})
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Close()) }()
	gen, err := b.CreateGeneration(t.Context(), "progress", 4, "")
	require.NoError(t, err)
	publisher := &pausedPolicyProgressPublisher{DocumentPublisher: b, beforeDone: func() {
		require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
		_, err := b.PruneIneligibleEmbeddings(t.Context())
		require.NoError(t, err)
	}}
	client := &contextSemanticFake{dim: 4}
	w := NewContextWorker(ContextWorkerDeps{Store: st, Backend: b, Publisher: publisher, Client: client, Assembler: CompositeAssembler{Policy: AssemblyPolicy{MaxChunkRunes: 200}}, Recorder: newTestOperationRecorder()})
	_, err = w.RunOnce(t.Context(), gen, testEmbeddingPassScope())
	require.ErrorIs(t, err, store.ErrConversationPolicyStale)
	assert.Zero(t, client.Calls())
	_, err = w.RunOnce(t.Context(), gen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.Positive(t, client.Calls(), "ordinary discovery must find promoted low-ID work")
	var stamp int64
	require.NoError(t, st.DB().QueryRow(`SELECT embed_gen FROM messages WHERE id=?`, id).Scan(&stamp))
	assert.Equal(t, int64(gen), stamp)
}

type pausedPolicyStampStore struct {
	*store.Store

	afterStamp func()
}

func (s *pausedPolicyStampStore) SetEmbedGenIfUnchanged(ctx context.Context, items []store.EmbedGenStamp, gen int64) ([]int64, int, error) {
	missed, covered, err := s.Store.SetEmbedGenIfUnchanged(ctx, items, gen)
	if err == nil && s.afterStamp != nil {
		pause := s.afterStamp
		s.afterStamp = nil
		pause()
	}
	return missed, covered, err
}

func TestWorkerColdPolicyStaleWatermarkAfterPromotion(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "watermark-policy")
	require.NoError(t, err)
	room, err := st.EnsureConversation(src.ID, "room", "Room")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "message", MessageType: "email", Subject: sql.NullString{String: "Watermark input", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	var mainPath string
	require.NoError(t, st.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&mainPath))
	b, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainPath: mainPath, MainDB: st.DB(), PolicyStore: st, Dimension: 4})
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Close()) }()
	gen, err := b.CreateGeneration(t.Context(), "watermark", 4, "")
	require.NoError(t, err)
	wrapped := &pausedPolicyStampStore{Store: st, afterStamp: func() {
		require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Cold))
		_, err := b.PruneIneligibleEmbeddings(t.Context())
		require.NoError(t, err)
		require.NoError(t, st.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
		_, err = b.PruneIneligibleEmbeddings(t.Context())
		require.NoError(t, err)
	}}
	client := &fakeEmbeddingClient{dim: 4}
	w := NewWorker(WorkerDeps{Store: wrapped, MainDB: st.DB(), Backend: b, VectorsDB: b.DB(), Client: client, BatchSize: 1, Recorder: newTestOperationRecorder()})
	_, err = w.RunOnce(t.Context(), gen, testEmbeddingPassScope())
	require.NoError(t, err)
	watermark, err := w.wm.GetWatermark(t.Context(), gen)
	require.NoError(t, err)
	assert.Zero(t, watermark, "stale advancement must not undo cleanup reset")
	_, err = w.RunOnce(t.Context(), gen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.Equal(t, 2, client.calls, "ordinary run rediscovers low-ID message")
	var stamp int64
	require.NoError(t, st.DB().QueryRow(`SELECT embed_gen FROM messages WHERE id=?`, id).Scan(&stamp))
	assert.Equal(t, int64(gen), stamp)
}
