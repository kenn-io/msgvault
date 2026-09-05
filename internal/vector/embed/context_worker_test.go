//go:build sqlite_vec

package embed

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

type contextSemanticFake struct {
	mu         sync.Mutex
	dim        int
	docs       int
	calls      int
	err        error
	before     func()
	sizeOnce   bool
	rejectText string
}

type contextPartialSemanticFake struct {
	mu    sync.Mutex
	calls [][]DocumentInput
}

type contextBatchPublisher struct {
	vector.DocumentPublisher

	batches   int
	scopes    int
	maxScopes int
}

type contextFenceRacePublisher struct {
	vector.DocumentPublisher

	beforeFence func() error
}

type countingContextAssembler struct {
	next      Assembler
	calls     int
	selectors map[AffectedScope]int
}

func (a *countingContextAssembler) AssembleScopes(
	ctx context.Context, snapshot SourceSnapshot, scopes []AffectedScope,
) ([]Document, error) {
	a.calls++
	if a.selectors == nil {
		a.selectors = make(map[AffectedScope]int)
	}
	for _, scope := range scopes {
		a.selectors[scope]++
	}
	return a.next.AssembleScopes(ctx, snapshot, scopes)
}

func (p *contextBatchPublisher) PublishScopes(ctx context.Context, gen vector.GenerationID, scopes []vector.DocumentScopePublication) error {
	p.batches++
	p.scopes += len(scopes)
	p.maxScopes = max(p.maxScopes, len(scopes))
	return p.DocumentPublisher.PublishScopes(ctx, gen, scopes)
}

func (p *contextFenceRacePublisher) PublishScopes(
	ctx context.Context, gen vector.GenerationID, scopes []vector.DocumentScopePublication,
) error {
	for _, scope := range scopes {
		if !scope.FenceOnly || p.beforeFence == nil {
			continue
		}
		beforeFence := p.beforeFence
		p.beforeFence = nil
		if err := beforeFence(); err != nil {
			return err
		}
		break
	}
	return p.DocumentPublisher.PublishScopes(ctx, gen, scopes)
}

func (c *contextSemanticFake) EmbedQuery(context.Context, string) ([]float32, error) {
	return make([]float32, c.dim), nil
}

func (c *contextPartialSemanticFake) EmbedQuery(context.Context, string) ([]float32, error) {
	return make([]float32, 4), nil
}

func (c *contextPartialSemanticFake) EmbedDocuments(_ context.Context, documents []DocumentInput) ([][][]float32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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

func (c *contextSemanticFake) EmbedDocuments(_ context.Context, documents []DocumentInput) ([][][]float32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.before != nil {
		c.before()
	}
	if c.err != nil {
		return nil, c.err
	}
	if c.rejectText != "" {
		for _, document := range documents {
			for _, chunk := range document.Chunks {
				if strings.Contains(chunk, c.rejectText) {
					return nil, fmt.Errorf("%w: synthetic leaf", ErrDocumentTooLarge)
				}
			}
		}
	}
	if c.sizeOnce && len(documents) > 1 {
		c.sizeOnce = false
		return nil, &voyageSizeError{message: "synthetic request too large"}
	}
	c.docs += len(documents)
	out := make([][][]float32, len(documents))
	for i, document := range documents {
		out[i] = make([][]float32, len(document.Chunks))
		for j := range document.Chunks {
			out[i][j] = []float32{float32(i + 1), float32(j + 1), 3, 4}
		}
	}
	return out, nil
}

func (c *contextSemanticFake) Documents() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.docs
}

func (c *contextSemanticFake) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type contextWorkerFixture struct {
	t        *testing.T
	store    *store.Store
	backend  *sqlitevec.Backend
	gen      vector.GenerationID
	client   *contextSemanticFake
	deps     ContextWorkerDeps
	worker   *ContextWorker
	sourceID int64
	chatID   int64
	personID int64
	sequence int
}

func newContextWorkerFixture(t *testing.T, mutate func(*ContextWorkerDeps)) *contextWorkerFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	st, err := store.OpenForTest(mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	require.NoError(t, sqlitevec.RegisterExtension())
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path: filepath.Join(dir, "vectors.db"), MainPath: mainPath,
		MainDB: st.DB(), Dimension: 4,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	gen, err := backend.CreateGeneration(ctx, "synthetic", 4, "synthetic:4")
	require.NoError(t, err)
	source, err := st.GetOrCreateSource("test", fmt.Sprintf("context-%d", time.Now().UnixNano()))
	require.NoError(t, err)
	chatID, err := st.EnsureConversationWithType(source.ID, "chat", "beeper", "Team")
	require.NoError(t, err)
	personID, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	require.NoError(t, err)
	require.NoError(t, st.EnsureConversationParticipant(chatID, personID, "member"))
	client := &contextSemanticFake{dim: 4}
	deps := ContextWorkerDeps{
		Backend: backend, Publisher: backend, Store: st,
		Assembler: CompositeAssembler{
			Policy: AssemblyPolicy{MaxChunkRunes: 200, ChatGap: 30 * time.Minute},
			Chat:   ChatWindowAssembler{Policy: AssemblyPolicy{MaxChunkRunes: 200, ChatGap: 30 * time.Minute}},
		},
		Client: client, ChangeBatchSize: 2, ReconcileBatchSize: 2,
		Recorder: newTestOperationRecorder(),
	}
	if mutate != nil {
		mutate(&deps)
	}
	return &contextWorkerFixture{t: t, store: st, backend: backend, gen: gen,
		client: client, deps: deps, worker: NewContextWorker(deps), sourceID: source.ID,
		chatID: chatID, personID: personID}
}

func (f *contextWorkerFixture) seed(kind string, conversationID int64, at time.Time, body string) int64 {
	return f.seedForSource(f.sourceID, kind, conversationID, at, body)
}

func (f *contextWorkerFixture) seedForSource(sourceID int64, kind string, conversationID int64, at time.Time, body string) int64 {
	f.t.Helper()
	f.sequence++
	id, err := f.store.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: sourceID,
		SourceMessageID: fmt.Sprintf("%s-%d", kind, f.sequence), MessageType: kind,
		SentAt:   sql.NullTime{Time: at, Valid: !at.IsZero()},
		SenderID: sql.NullInt64{Int64: f.personID, Valid: true},
		Subject:  sql.NullString{String: "subject", Valid: kind != "beeper"},
	})
	require.NoError(f.t, err)
	require.NoError(f.t, f.store.UpsertMessageBody(id,
		sql.NullString{String: body, Valid: true}, sql.NullString{}))
	return id
}

func (f *contextWorkerFixture) run() (RunResult, error) {
	return f.worker.RunOnce(context.Background(), f.gen, testEmbeddingPassScope())
}

func (f *contextWorkerFixture) restartWorker() {
	f.worker = NewContextWorker(f.deps)
}

func (f *contextWorkerFixture) progress() vector.DocumentProgress {
	f.t.Helper()
	p, err := f.backend.GetDocumentProgress(context.Background(), f.gen)
	require.NoError(f.t, err)
	return p
}

func latestContextSequence(t *testing.T, st *store.Store) int64 {
	t.Helper()
	sequence, err := st.LatestEmbeddingChangeSequence(t.Context())
	require.NoError(t, err)
	return sequence
}

func (f *contextWorkerFixture) missing() int {
	f.t.Helper()
	var count int
	require.NoError(f.t, f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE deleted_at IS NULL AND deleted_from_source_at IS NULL AND (embed_gen IS NULL OR embed_gen <> ?)`,
		int64(f.gen)).Scan(&count))
	return count
}

func TestContextWorker_NewChatTailRepublishesOnlyOpenWindow(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	f.seed("beeper", f.chatID, day, "closed")
	f.seed("beeper", f.chatID, day.Add(time.Hour), "open one")
	f.seed("beeper", f.chatID, day.Add(time.Hour+time.Minute), "open two")
	first, err := f.run()
	require.NoError(t, err)
	require.True(t, first.Contextual.Converged)
	before := f.client.Documents()
	f.seed("beeper", f.chatID, day.Add(time.Hour+2*time.Minute), "open tail")
	second, err := f.run()
	require.NoError(t, err)
	assert.Equal(t, before+1, f.client.Documents(), "the closed sibling window must reuse its published revision")
	assert.True(t, second.Contextual.Converged)
	assert.Zero(t, f.missing())
}

func TestContextWorkerOperationPassRecordsOneTerminalMessageRun(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	f.deps.Recorder = f.store
	f.restartWorker()
	f.seed("beeper", f.chatID, time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC), "record me")
	scope := operations.PassScope{
		Key: "manual:context:terminal-ledger", Trigger: operations.TriggerManual,
		StartedAt: time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC),
	}

	result, err := f.worker.RunOnce(t.Context(), f.gen, scope)
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	runs := messageEmbeddingOperationRuns(t, f.store)
	require.Len(t, runs, 1)
	assert.Equal(t, operations.StateSucceeded, runs[0].State)
	attempted := operationCounter(runs[0], operations.CounterAttempted)
	assert.Positive(t, attempted)
	assert.Equal(t, attempted, operationCounter(runs[0], operations.CounterSucceeded))
}

func TestContextWorkerCheckpointDetachesFromCancelledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.Hooks.AfterCoverage = func() error {
			cancel()
			return nil
		}
	})
	t.Cleanup(cancel)
	f.seed("beeper", f.chatID, time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC), "cancel after coverage")

	_, _ = f.worker.RunOnce(ctx, f.gen, testEmbeddingPassScope())
	recorder, ok := f.deps.Recorder.(*testOperationRecorder)
	require.True(t, ok)
	contexts := recorder.contexts()
	require.NotEmpty(t, contexts, "durable coverage must attempt a checkpoint")
	for _, observed := range contexts {
		require.NoError(t, observed.err, "checkpoint context must survive caller cancellation")
		assert.True(t, observed.hasDeadline, "checkpoint context must remain bounded")
	}
}

func TestContextWorker_BackstopTimestampOnlyChatChangeDoesNotCallProvider(t *testing.T) {
	assert := assert.New(t)
	f := newContextWorkerFixture(t, nil)
	messageID := f.seed("beeper", f.chatID, time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), "unchanged body")
	result, err := f.run()
	require.NoError(t, err)
	require.True(t, result.Contextual.Converged)
	beforeCalls := f.client.Calls()
	documents, err := f.backend.ListDocumentsForScope(t.Context(), f.gen,
		fmt.Sprintf("chat:%d:2026-08-08", f.chatID))
	require.NoError(t, err)
	require.Len(t, documents, 1)
	beforeRevision := documents[0].PublishedRevision

	_, err = f.store.DB().Exec(
		`UPDATE messages SET last_modified = '2099-01-01 00:00:00' WHERE id = ?`, messageID)
	require.NoError(t, err)
	result, err = f.worker.RunBackstop(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.True(result.Contextual.Converged)
	assert.Equal(beforeCalls, f.client.Calls(),
		"LastModified remains a CAS fence but must not change the semantic document revision")
	documents, err = f.backend.ListDocumentsForScope(t.Context(), f.gen,
		fmt.Sprintf("chat:%d:2026-08-08", f.chatID))
	require.NoError(t, err)
	require.Len(t, documents, 1)
	assert.Equal(beforeRevision, documents[0].PublishedRevision)
}

func TestContextWorker_FirstTickPublishesEachScopeOnce(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	f.seed("beeper", f.chatID, day, "first committed message")
	f.seed("beeper", f.chatID, day.Add(time.Minute), "second committed message")

	result, err := f.run()
	require.NoError(t, err)
	require.True(t, result.Contextual.Converged)
	assert.Equal(t, 1, f.client.calls, "journal drain and initial reconciliation must share the published revision")
	assert.Equal(t, 1, f.client.Documents())
	assert.Zero(t, f.missing())
}

func TestContextWorker_BuildScopeNeverSubmitsExcludedBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.BuildScope = vector.NewBuildScope([]string{"beeper"}, nil)
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "excluded-mail", "Excluded")
	require.NoError(err)
	excludedID := f.seed("email", conversationID, time.Now().UTC(), "excluded-synthetic-body")
	f.seed("beeper", f.chatID, time.Now().UTC(), "included chat")
	f.client.rejectText = "excluded-synthetic-body"

	result, err := f.run()
	require.NoError(err)
	assert.True(result.Contextual.Converged)
	assert.Equal(1, f.client.Documents(), "only the in-scope chat document reaches the client")
	_, err = f.backend.GetDocument(context.Background(), f.gen, fmt.Sprintf("message:%d", excludedID))
	assert.Error(err)
}

// TestContextWorker_SourceScopeNeverSubmitsExcludedAccountBody pins the
// account dimension of the contextual privacy boundary: with a source-only
// build scope, an excluded account's message text must never reach the
// embedding client (the fake errors if it does) and must publish no
// document, while the in-scope account still converges. This also proves a
// source-only scope is not rejected wholesale — each scope dimension is
// enforced independently, and ContainsMessageType on an empty type list
// would otherwise veto everything.
func TestContextWorker_SourceScopeNeverSubmitsExcludedAccountBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var included int64
	f := newContextWorkerFixture(t, nil)
	other, err := f.store.GetOrCreateSource("test", fmt.Sprintf("excluded-%d", time.Now().UnixNano()))
	require.NoError(err)
	otherChat, err := f.store.EnsureConversationWithType(other.ID, "chat-excluded", "beeper", "Excluded Team")
	require.NoError(err)
	f.deps.BuildScope = vector.NewBuildScope(nil, []int64{f.sourceID})
	f.restartWorker()

	included = f.seed("beeper", f.chatID, time.Now().UTC(), "included chat body")
	excludedChat := f.seedForSource(other.ID, "beeper", otherChat, time.Now().UTC(), "excluded-account-chat-body")
	excludedMail := f.seedForSource(other.ID, "email", otherChat, time.Now().UTC(), "excluded-account-mail-body")
	f.client.rejectText = "excluded-account"

	result, err := f.run()
	require.NoError(err, "excluded-account text reaching the client fails the run")
	assert.True(result.Contextual.Converged, "a source-only scope must converge, not reject everything")
	assert.Equal(1, f.client.Documents(), "only the in-scope chat document reaches the client")
	_, err = f.backend.GetDocument(context.Background(), f.gen,
		fmt.Sprintf("message:%d", excludedMail))
	require.Error(err, "excluded ordinary message must publish no document")
	_ = excludedChat
	_ = included

	// Phase two exercises the JOURNAL path: post-convergence mutations reach
	// scopes through scopesForChanges, whose enumeration is deliberately
	// unfiltered by source (moved-out scopes must tombstone), so only the
	// selectorInBuildScope gate stands between an excluded account's text
	// and the embedding client.
	journalMail := f.seedForSource(other.ID, "email", otherChat, time.Now().UTC(), "excluded-account-journal-mail")
	journalChat := f.seedForSource(other.ID, "beeper", otherChat, time.Now().UTC(), "excluded-account-journal-chat")
	result, err = f.run()
	require.NoError(err, "journal-delivered excluded-account text must not reach the client")
	assert.True(result.Contextual.Converged)
	assert.Equal(1, f.client.Documents(), "journal drain must add no excluded documents")
	_, err = f.backend.GetDocument(context.Background(), f.gen,
		fmt.Sprintf("message:%d", journalMail))
	require.Error(err, "journal-delivered excluded message must publish no document")
	_ = journalChat
}

func TestContextWorker_BuildScopeTombstonesOrdinaryMoveOutWithoutSubmitting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.BuildScope = vector.NewBuildScope([]string{"sms"}, nil)
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "scoped-text", "Scoped")
	require.NoError(err)
	messageID := f.seed("sms", conversationID, time.Now().UTC(), "included text")
	_, err = f.run()
	require.NoError(err)
	before := f.client.Documents()

	_, err = f.store.DB().Exec(`UPDATE messages SET message_type = 'email' WHERE id = ?`, messageID)
	require.NoError(err)
	changes, err := f.store.ScanEmbeddingChanges(t.Context(), f.progress().ChangeSequence, 10)
	require.NoError(err)
	require.Len(changes, 1, "moving a live ordinary message across the configured scope must be journaled")
	assert.Equal("sms", changes[0].OldMessageType.String)
	assert.Equal("email", changes[0].NewMessageType.String)
	require.NoError(f.store.UpsertMessageBody(messageID,
		sql.NullString{String: "excluded-moved-body", Valid: true}, sql.NullString{}))
	f.client.rejectText = "excluded-moved-body"

	result, err := f.run()
	require.NoError(err)
	assert.True(result.Contextual.Converged)
	assert.Equal(before, f.client.Documents(), "moving out of scope must not submit replacement content")
	doc, err := f.backend.GetDocument(context.Background(), f.gen, fmt.Sprintf("message:%d", messageID))
	require.NoError(err)
	assert.Equal(vector.DocumentTombstoned, doc.State)
}

func TestContextWorker_FreshGenerationPinsJournalBeforeReconciliation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var counted *countingContextAssembler
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 1
		counted = &countingContextAssembler{next: deps.Assembler}
		deps.Assembler = counted
	})
	messageID := f.seed("beeper", f.chatID, time.Now().UTC(), "initial")
	for i := range 5 {
		require.NoError(f.store.UpsertMessageBody(messageID,
			sql.NullString{String: fmt.Sprintf("current revision %d", i), Valid: true}, sql.NullString{}))
	}
	wantSequence := latestContextSequence(t, f.store)

	result, err := f.run()
	require.NoError(err)
	assert.True(result.Contextual.Converged)
	assert.Equal(wantSequence, f.progress().ChangeSequence)
	assert.LessOrEqual(counted.calls, 3,
		"fresh generations must reconcile current state instead of assembling every historical journal page")
}

func TestContextWorker_BatchesIndependentScopesInOneSemanticCall(t *testing.T) {
	var publisher *contextBatchPublisher
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 64
		deps.ReconcileBatchSize = 64
		publisher = &contextBatchPublisher{DocumentPublisher: deps.Publisher}
		deps.Publisher = publisher
	})
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	for i := range 17 {
		f.seed("email", f.chatID, day.Add(time.Duration(i)*time.Minute), fmt.Sprintf("ordinary message %d", i))
	}

	result, err := f.run()
	require.NoError(t, err)
	require.True(t, result.Contextual.Converged)
	assert.Equal(t, 17, f.client.Documents())
	assert.Equal(t, 1, f.client.Calls(), "independent scopes in one prepared page must share a semantic request")
	assert.Equal(t, 1, publisher.batches, "one prepared page must use one vector publication transaction")
	assert.Equal(t, 17, publisher.scopes)
	assert.Zero(t, f.missing())
}

func TestContextWorker_BatchesIndependentJournalEvents(t *testing.T) {
	var publisher *contextBatchPublisher
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 64
		deps.ReconcileBatchSize = 64
		publisher = &contextBatchPublisher{DocumentPublisher: deps.Publisher}
		deps.Publisher = publisher
	})
	f.seed("email", f.chatID, time.Now().UTC(), "initial")
	_, err := f.run()
	require.NoError(t, err)
	publisher.batches, publisher.scopes, publisher.maxScopes = 0, 0, 0
	beforeCalls := f.client.Calls()
	beforeDocuments := f.client.Documents()

	day := time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC)
	for i := range 17 {
		f.seed("email", f.chatID, day.Add(time.Duration(i)*time.Minute),
			fmt.Sprintf("journal message %d", i))
	}
	result, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, 1, f.client.Calls()-beforeCalls,
		"one journal page must batch independent ordinary documents in one semantic request")
	assert.Equal(t, 17, f.client.Documents()-beforeDocuments)
	assert.Equal(t, 1, publisher.batches)
	assert.Equal(t, 17, publisher.scopes)
	assert.Zero(t, f.missing())
}

func TestContextWorker_RestartPreservesUnchangedSiblingVectors(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	changedID := f.seed("beeper", f.chatID, day, "first window")
	f.seed("beeper", f.chatID, day.Add(2*time.Hour), "unchanged window")
	_, err := f.run()
	require.NoError(t, err)
	beforeDocuments := f.client.Documents()
	f.restartWorker()
	require.NoError(t, f.store.UpsertMessageBody(changedID,
		sql.NullString{String: "first window edited", Valid: true}, sql.NullString{}))

	result, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, 1, f.client.Documents()-beforeDocuments,
		"a restart must not re-embed an unchanged sibling document")
}

func TestContextWorker_BodylessOrdinaryLifecycleConverges(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	_, err := f.run()
	require.NoError(t, err)
	id, err := f.store.UpsertMessage(&store.Message{
		ConversationID: f.chatID, SourceID: f.sourceID, SourceMessageID: "subject-only",
		MessageType: "email", Subject: sql.NullString{String: "First subject", Valid: true},
		SentAt: sql.NullTime{Time: time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(t, err)

	result, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	record, err := f.backend.GetDocument(t.Context(), f.gen, fmt.Sprintf("message:%d", id))
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentCurrent, record.State)

	_, err = f.store.UpsertMessage(&store.Message{
		ConversationID: f.chatID, SourceID: f.sourceID, SourceMessageID: "subject-only",
		MessageType: "email", Subject: sql.NullString{String: "Second subject", Valid: true},
		SentAt: sql.NullTime{Time: time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(t, err)
	beforeDocuments := f.client.Documents()
	_, err = f.run()
	require.NoError(t, err)
	assert.Equal(t, 1, f.client.Documents()-beforeDocuments)

	_, err = f.store.UpsertMessage(&store.Message{
		ConversationID: f.chatID, SourceID: f.sourceID, SourceMessageID: "subject-only",
		MessageType: "email", Subject: sql.NullString{},
		SentAt: sql.NullTime{Time: time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(t, err)
	_, err = f.run()
	require.NoError(t, err)
	record, err = f.backend.GetDocument(t.Context(), f.gen, fmt.Sprintf("message:%d", id))
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, record.State,
		"clearing a bodyless subject must retire its stale ordinary document")

	_, err = f.store.DB().Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	require.NoError(t, err)
	_, err = f.run()
	require.NoError(t, err)
	record, err = f.backend.GetDocument(t.Context(), f.gen, fmt.Sprintf("message:%d", id))
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, record.State)
}

func TestContextWorker_NewBlankBodylessOrdinaryConvergesAfterReconciliation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newContextWorkerFixture(t, nil)
	result, err := f.run()
	require.NoError(err)
	require.NotNil(result.Contextual)
	require.True(result.Contextual.Converged)
	beforeSequence := latestContextSequence(t, f.store)
	beforeDocuments := f.client.Documents()

	_, err = f.store.UpsertMessage(&store.Message{
		ConversationID:  f.chatID,
		SourceID:        f.sourceID,
		SourceMessageID: "blank-bodyless-after-reconcile",
		MessageType:     "email",
		SentAt: sql.NullTime{
			Time:  time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC),
			Valid: true,
		},
	})
	require.NoError(err)
	afterSequence := latestContextSequence(t, f.store)
	assert.Greater(afterSequence, beforeSequence,
		"a new blank ordinary message must enter the durable journal")

	result, err = f.run()
	require.NoError(err)
	require.NotNil(result.Contextual)
	assert.True(result.Contextual.Converged)
	assert.Zero(f.missing(), "the blank message must receive terminal coverage")
	assert.Equal(beforeDocuments, f.client.Documents(),
		"blank content must not call the semantic provider")
	assert.Equal(afterSequence, f.progress().ChangeSequence)
}

func TestContextWorker_NewBlankBodylessContextualConvergesAfterReconciliation(t *testing.T) {
	for _, messageType := range []string{"beeper", "meeting_transcript"} {
		t.Run(messageType, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newContextWorkerFixture(t, nil)
			result, err := f.run()
			require.NoError(err)
			require.NotNil(result.Contextual)
			require.True(result.Contextual.Converged)
			beforeSequence := latestContextSequence(t, f.store)
			beforeDocuments := f.client.Documents()

			_, err = f.store.UpsertMessage(&store.Message{
				ConversationID:  f.chatID,
				SourceID:        f.sourceID,
				SourceMessageID: "blank-bodyless-" + messageType,
				MessageType:     messageType,
				SentAt: sql.NullTime{
					Time:  time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC),
					Valid: true,
				},
			})
			require.NoError(err)
			afterSequence := latestContextSequence(t, f.store)
			assert.Greater(afterSequence, beforeSequence,
				"a new blank contextual message must enter the durable journal")

			result, err = f.run()
			require.NoError(err)
			require.NotNil(result.Contextual)
			assert.True(result.Contextual.Converged)
			assert.Zero(f.missing(), "the blank contextual message must receive terminal coverage")
			assert.Equal(beforeDocuments, f.client.Documents(),
				"blank contextual content must not call the semantic provider")
			assert.Equal(afterSequence, f.progress().ChangeSequence)
		})
	}
}

func TestContextWorker_RepairedBodylessMessageConvergesAfterReconciliation(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	_, err := f.run()
	require.NoError(t, err)
	id, err := f.store.UpsertMessage(&store.Message{
		ConversationID: f.chatID, SourceID: f.sourceID, SourceMessageID: "repaired-subject-only",
		MessageType: "email", Subject: sql.NullString{String: "Before repair", Valid: true},
	})
	require.NoError(t, err)
	_, err = f.run()
	require.NoError(t, err)
	key := fmt.Sprintf("message:%d", id)
	before, err := f.backend.GetDocument(t.Context(), f.gen, key)
	require.NoError(t, err)
	beforeDocuments := f.client.Documents()

	_, err = f.store.DB().Exec(
		`UPDATE messages SET subject = 'After repair' WHERE id = ?`, id)
	require.NoError(t, err)
	require.NoError(t, f.store.ResetEmbedGen(t.Context(), []int64{id}))
	assert.Equal(t, 1, f.missing())

	result, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	after, err := f.backend.GetDocument(t.Context(), f.gen, key)
	require.NoError(t, err)
	assert.NotEqual(t, before.PublishedRevision, after.PublishedRevision)
	assert.Equal(t, 1, f.client.Documents()-beforeDocuments)
	assert.Zero(t, f.missing())
}

func TestContextWorker_CoalescesConsecutiveMetadataFanout(t *testing.T) {
	var counted *countingContextAssembler
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 64
		deps.ReconcileBatchSize = 64
		counted = &countingContextAssembler{next: deps.Assembler}
		deps.Assembler = counted
	})
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	f.seed("beeper", f.chatID, day, "first day")
	f.seed("beeper", f.chatID, day.AddDate(0, 0, 1), "second day")
	_, err := f.run()
	require.NoError(t, err)
	counted.calls = 0
	counted.selectors = make(map[AffectedScope]int)
	beforeCalls := f.client.Calls()
	beforeDocuments := f.client.Documents()
	_, err = f.store.DB().Exec(`UPDATE conversations SET title = 'First rename' WHERE id = ?`, f.chatID)
	require.NoError(t, err)
	_, err = f.store.DB().Exec(`UPDATE conversations SET title = 'Final rename' WHERE id = ?`, f.chatID)
	require.NoError(t, err)

	result, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, 1, f.client.Calls()-beforeCalls)
	assert.Equal(t, 2, f.client.Documents()-beforeDocuments)
	assert.Equal(t, 1, counted.calls, "consecutive changes for one conversation must share one fanout")
	for offset := range 2 {
		selector := chatDayContextScope(f.chatID, day.AddDate(0, 0, offset)).selector
		assert.Equal(t, 1, counted.selectors[selector])
	}
}

func TestContextWorker_MetadataFanoutBudgetsPreservedScopes(t *testing.T) {
	var counted *countingContextAssembler
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 64
		deps.ReconcileBatchSize = 64
		counted = &countingContextAssembler{next: deps.Assembler}
		deps.Assembler = counted
	})
	start := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	for day := range 3 {
		f.seed("beeper", f.chatID, start.AddDate(0, 0, day), fmt.Sprintf("day %d", day))
	}
	_, err := f.run()
	require.NoError(t, err)

	snapshot, err := BeginSourceSnapshot(t.Context(), f.store)
	require.NoError(t, err)
	documents, err := f.worker.deps.Assembler.AssembleScopes(t.Context(), snapshot,
		[]AffectedScope{chatDayContextScope(f.chatID, start).selector})
	require.NoError(t, err)
	require.NoError(t, snapshot.Close())
	require.Len(t, documents, 1)
	input := DocumentInput{Chunks: make([]string, len(documents[0].Chunks))}
	for i, chunk := range documents[0].Chunks {
		input.Chunks[i] = chunk.Text
	}
	f.worker.deps.MaxRunUTF8Bytes = documentInputUTF8Bytes(input, 0)
	counted.calls = 0
	counted.selectors = make(map[AffectedScope]int)
	beforeDocuments := f.client.Documents()
	_, err = f.store.DB().Exec(`UPDATE conversations SET title = 'Temporary title' WHERE id = ?`, f.chatID)
	require.NoError(t, err)
	_, err = f.store.DB().Exec(`UPDATE conversations SET title = 'Team' WHERE id = ?`, f.chatID)
	require.NoError(t, err)

	runs := 0
	for ; runs < 20; runs++ {
		result, runErr := f.run()
		require.NoError(t, runErr)
		require.NotNil(t, result.Contextual)
		if result.Contextual.Converged {
			break
		}
	}
	assert.Less(t, runs, 20, "bounded preserved-scope work must eventually converge")
	assert.Greater(t, runs, 1, "the small work budget must defer full-history assembly")
	assert.Equal(t, beforeDocuments, f.client.Documents(), "unchanged durable vectors must not call the provider")
	for day := range 3 {
		selector := chatDayContextScope(f.chatID, start.AddDate(0, 0, day)).selector
		assert.Positive(t, counted.selectors[selector])
	}
}

func TestDocumentInputUTF8BytesIncludesDocumentPrefixPerChunk(t *testing.T) {
	input := DocumentInput{Chunks: []string{"first", "second"}}
	withoutPrefix := documentInputUTF8Bytes(input, 0)

	assert.Equal(t, withoutPrefix+2*len("search_document: "),
		documentInputUTF8Bytes(input, len("search_document: ")))
}

func TestContextWorker_MetadataFanoutUsesBoundedResumablePages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var publisher *contextBatchPublisher
	crashAfterPublish := false
	crashed := false
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 2
		deps.ReconcileBatchSize = 64
		publisher = &contextBatchPublisher{DocumentPublisher: deps.Publisher}
		deps.Publisher = publisher
		deps.Hooks.AfterPublish = func() error {
			if crashAfterPublish && !crashed {
				crashed = true
				return errors.New("synthetic metadata fanout crash")
			}
			return nil
		}
	})
	start := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	for day := range 5 {
		f.seed("beeper", f.chatID, start.AddDate(0, 0, day), fmt.Sprintf("day %d", day))
	}
	_, err := f.run()
	require.NoError(err)
	publisher.batches, publisher.scopes, publisher.maxScopes = 0, 0, 0

	_, err = f.store.DB().Exec(`UPDATE conversations SET title = 'Paged title' WHERE id = ?`, f.chatID)
	require.NoError(err)
	wantSequence := latestContextSequence(t, f.store)
	crashAfterPublish = true
	_, err = f.run()
	require.ErrorContains(err, "synthetic metadata fanout crash")
	assert.Equal(1, publisher.batches)
	assert.Equal(2, publisher.maxScopes, "one metadata page must not exceed ChangeBatchSize")
	assert.Less(f.progress().ChangeSequence, wantSequence, "the journal cursor must wait for every metadata page")
	afterCrashDocuments := f.client.Documents()

	crashAfterPublish = false
	publisher.batches, publisher.scopes, publisher.maxScopes = 0, 0, 0
	result, err := f.run()
	require.NoError(err)
	require.NotNil(result.Contextual)
	assert.True(result.Contextual.Converged)
	assert.Equal(wantSequence, f.progress().ChangeSequence)
	assert.Equal(3, f.client.Documents()-afterCrashDocuments,
		"retry must reuse the already published first page and embed only the remaining scopes")
	assert.Equal(2, publisher.batches)
	assert.Equal(3, publisher.scopes)
	assert.LessOrEqual(publisher.maxScopes, 2)
}

func TestContextWorker_MetadataFanoutNewestBlockIsOneAtomicBudgetUnit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 1
	})
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, chatScopeMaxMessages+1)
	for i := range chatScopeMaxMessages + 1 {
		ids = append(ids, f.seed("beeper", f.chatID, day.Add(time.Duration(i)*time.Minute),
			fmt.Sprintf("bounded metadata message %d", i)))
	}
	_, err := f.run()
	require.NoError(err)
	firstScope := chatMessageContextScope(f.chatID, day, ids[0])
	lastScope := chatMessageContextScope(f.chatID, day, ids[len(ids)-1])
	assert.NotEqual(firstScope.key, lastScope.key)
	for _, scope := range []contextScope{firstScope, lastScope} {
		docs, listErr := f.backend.ListDocumentsForScope(t.Context(), f.gen, scope.key)
		require.NoError(listErr)
		require.Len(docs, 1)
		assert.LessOrEqual(len(docs[0].Members), chatScopeMaxMessages)
	}

	_, err = f.store.DB().Exec(`UPDATE conversations SET title = 'Bounded title' WHERE id = ?`, f.chatID)
	require.NoError(err)
	wantSequence := latestContextSequence(t, f.store)
	f.worker.deps.MaxRunUTF8Bytes = 1
	beforeDocuments := f.client.Documents()
	result, err := f.run()
	require.NoError(err)
	require.NotNil(result.Contextual)
	assert.True(result.Contextual.Converged)
	assert.Equal(wantSequence, f.progress().ChangeSequence)
	assert.Empty(f.progress().JournalCursor)
	assert.Equal(1, f.client.Documents()-beforeDocuments,
		"the newest stable block remains one atomic unit even when it exceeds the run budget")
}

func TestContextWorker_ChatBlockBoundaryRefreshesPreviousAndCurrentMetadataModes(t *testing.T) {
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 64
		deps.ReconcileBatchSize = 64
	})
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, chatScopeMaxMessages)
	for i := range chatScopeMaxMessages {
		ids = append(ids, f.seed("beeper", f.chatID, day.Add(time.Duration(i)*time.Minute),
			fmt.Sprintf("boundary message %d", i)))
	}
	_, err := f.run()
	require.NoError(t, err)
	oldScope := chatMessageContextScope(f.chatID, day, ids[0])
	oldBefore, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, oldScope.key)
	require.NoError(t, err)
	require.Len(t, oldBefore, 1)

	boundaryID := f.seed("beeper", f.chatID, day.Add(time.Duration(chatScopeMaxMessages)*time.Minute),
		"new metadata boundary")
	newScope := chatMessageContextScope(f.chatID, day, boundaryID)
	require.NotEqual(t, oldScope.key, newScope.key)
	beforeInsert := f.client.Documents()
	result, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, 2, f.client.Documents()-beforeInsert,
		"the new block and the former newest block must both change metadata modes")
	oldHistorical, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, oldScope.key)
	require.NoError(t, err)
	require.Len(t, oldHistorical, 1)
	assert.NotEqual(t, oldBefore[0].PublishedRevision, oldHistorical[0].PublishedRevision)
	newDocuments, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, newScope.key)
	require.NoError(t, err)
	require.Len(t, newDocuments, 1)

	_, err = f.store.DB().Exec(`DELETE FROM messages WHERE id = ?`, boundaryID)
	require.NoError(t, err)
	beforeDelete := f.client.Documents()
	result, err = f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, 1, f.client.Documents()-beforeDelete,
		"the previous block must regain live metadata when it becomes newest")
	oldCurrent, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, oldScope.key)
	require.NoError(t, err)
	require.Len(t, oldCurrent, 1)
	assert.Equal(t, oldBefore[0].PublishedRevision, oldCurrent[0].PublishedRevision)
	newAfter, err := f.backend.GetDocument(t.Context(), f.gen, newDocuments[0].Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, newAfter.State)
}

func TestContextWorker_ChatBlockBoundaryRefreshesMetadataModesOnSoftDeleteAndRestore(t *testing.T) {
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 64
		deps.ReconcileBatchSize = 64
	})
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, chatScopeMaxMessages+1)
	for i := range chatScopeMaxMessages + 1 {
		ids = append(ids, f.seed("beeper", f.chatID, day.Add(time.Duration(i)*time.Minute),
			fmt.Sprintf("soft boundary message %d", i)))
	}
	_, err := f.run()
	require.NoError(t, err)
	oldScope := chatMessageContextScope(f.chatID, day, ids[0])
	newScope := chatMessageContextScope(f.chatID, day, ids[len(ids)-1])
	require.NotEqual(t, oldScope.key, newScope.key)
	oldHistorical, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, oldScope.key)
	require.NoError(t, err)
	require.Len(t, oldHistorical, 1)
	newBefore, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, newScope.key)
	require.NoError(t, err)
	require.Len(t, newBefore, 1)

	_, err = f.store.DB().Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, ids[len(ids)-1])
	require.NoError(t, err)
	beforeDelete := f.client.Documents()
	result, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, 1, f.client.Documents()-beforeDelete,
		"the previous block must regain live metadata after a boundary message is soft-deleted")
	oldCurrent, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, oldScope.key)
	require.NoError(t, err)
	require.Len(t, oldCurrent, 1)
	assert.NotEqual(t, oldHistorical[0].PublishedRevision, oldCurrent[0].PublishedRevision)
	newAfterDelete, err := f.backend.GetDocument(t.Context(), f.gen, newBefore[0].Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, newAfterDelete.State)

	_, err = f.store.DB().Exec(`UPDATE messages SET deleted_at = NULL WHERE id = ?`, ids[len(ids)-1])
	require.NoError(t, err)
	beforeRestore := f.client.Documents()
	result, err = f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, 2, f.client.Documents()-beforeRestore,
		"the restored boundary block and former newest block must both change metadata modes")
	oldRestored, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, oldScope.key)
	require.NoError(t, err)
	require.Len(t, oldRestored, 1)
	assert.Equal(t, oldHistorical[0].PublishedRevision, oldRestored[0].PublishedRevision)
	newAfterRestore, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, newScope.key)
	require.NoError(t, err)
	require.Len(t, newAfterRestore, 1)
	assert.Equal(t, vector.DocumentCurrent, newAfterRestore[0].State)
}

func TestContextWorker_MetadataFanoutEmbedsOnlyNewestStableChatBlock(t *testing.T) {
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 64
		deps.ReconcileBatchSize = 64
	})
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, chatScopeMaxMessages+1)
	for i := range chatScopeMaxMessages + 1 {
		ids = append(ids, f.seed("beeper", f.chatID, day.Add(time.Duration(i)*time.Minute),
			fmt.Sprintf("bounded metadata archive message %d", i)))
	}
	_, err := f.run()
	require.NoError(t, err)
	oldScope := chatMessageContextScope(f.chatID, day, ids[0])
	recentScope := chatMessageContextScope(f.chatID, day, ids[len(ids)-1])
	require.NotEqual(t, oldScope.key, recentScope.key)
	oldBefore, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, oldScope.key)
	require.NoError(t, err)
	require.Len(t, oldBefore, 1)
	recentBefore, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, recentScope.key)
	require.NoError(t, err)
	require.Len(t, recentBefore, 1)
	beforeDocuments := f.client.Documents()

	_, err = f.store.DB().Exec(`UPDATE conversations SET title = 'Bounded metadata title' WHERE id = ?`, f.chatID)
	require.NoError(t, err)
	result, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, 1, f.client.Documents()-beforeDocuments,
		"one metadata mutation must not resubmit historical chat blocks")
	oldAfter, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, oldScope.key)
	require.NoError(t, err)
	require.Len(t, oldAfter, 1)
	assert.Equal(t, oldBefore[0].PublishedRevision, oldAfter[0].PublishedRevision)
	recentAfter, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, recentScope.key)
	require.NoError(t, err)
	require.Len(t, recentAfter, 1)
	assert.NotEqual(t, recentBefore[0].PublishedRevision, recentAfter[0].PublishedRevision)
	beforeBackstop := f.client.Documents()
	backstop, err := f.worker.RunBackstop(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	require.NotNil(t, backstop.Contextual)
	assert.True(t, backstop.Contextual.Converged)
	assert.Equal(t, beforeBackstop, f.client.Documents(),
		"the full backstop must keep historical metadata-independent revisions stable")
	oldAfterBackstop, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, oldScope.key)
	require.NoError(t, err)
	require.Len(t, oldAfterBackstop, 1)
	assert.Equal(t, oldBefore[0].PublishedRevision, oldAfterBackstop[0].PublishedRevision)

	participantID, err := f.store.EnsureParticipant("recent-member@example.test", "Recent Member", "example.test")
	require.NoError(t, err)
	require.NoError(t, f.store.EnsureConversationParticipant(f.chatID, participantID, "member"))
	beforeMembership := f.client.Documents()
	result, err = f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, 1, f.client.Documents()-beforeMembership)
	beforeMembershipBackstop := f.client.Documents()
	backstop, err = f.worker.RunBackstop(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	require.NotNil(t, backstop.Contextual)
	assert.True(t, backstop.Contextual.Converged)
	assert.Equal(t, beforeMembershipBackstop, f.client.Documents(),
		"membership changes must not make historical blocks differ during a backstop")
}

func TestContextWorker_MetadataFanoutHonorsPerRunByteBudgetAndResumes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var counted *countingContextAssembler
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 1
		counted = &countingContextAssembler{next: deps.Assembler}
		deps.Assembler = counted
	})
	start := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	for day := range 5 {
		f.seed("beeper", f.chatID, start.AddDate(0, 0, day), fmt.Sprintf("day %d", day))
	}
	_, err := f.run()
	require.NoError(err)
	counted.calls = 0
	counted.selectors = make(map[AffectedScope]int)

	_, err = f.store.DB().Exec(`UPDATE conversations SET title = 'Budgeted title' WHERE id = ?`, f.chatID)
	require.NoError(err)
	snapshot, err := BeginSourceSnapshot(t.Context(), f.store)
	require.NoError(err)
	documents, err := f.worker.deps.Assembler.AssembleScopes(t.Context(), snapshot,
		[]AffectedScope{chatDayContextScope(f.chatID, start).selector})
	require.NoError(err)
	require.NoError(snapshot.Close())
	require.Len(documents, 1)
	input := DocumentInput{Chunks: make([]string, len(documents[0].Chunks))}
	for i, chunk := range documents[0].Chunks {
		input.Chunks[i] = chunk.Text
	}
	f.worker.deps.MaxRunUTF8Bytes = documentInputUTF8Bytes(input, 0)
	counted.calls = 0
	counted.selectors = make(map[AffectedScope]int)
	for run := 1; run <= 5; run++ {
		before := f.client.Documents()
		result, runErr := f.run()
		require.NoError(runErr)
		assert.Equal(1, f.client.Documents()-before, "each run may submit only one atomic changed scope")
		if run < 5 {
			require.NotNil(result.Contextual)
			assert.False(result.Contextual.Converged)
			assert.NotEmpty(f.progress().JournalCursor)
			continue
		}
		require.NotNil(result.Contextual)
		assert.True(result.Contextual.Converged)
		assert.Empty(f.progress().JournalCursor)
	}
	for day := range 5 {
		selector := chatDayContextScope(f.chatID, start.AddDate(0, 0, day)).selector
		assert.Equal(1, counted.selectors[selector], "a resumed metadata event must not reassemble completed day scopes")
	}
}

func TestContextWorker_ReconcileAssemblesChatDayOnceAcrossSourcePages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var counted *countingContextAssembler
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ReconcileBatchSize = 2
		counted = &countingContextAssembler{next: deps.Assembler}
		deps.Assembler = counted
	})
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	for minute := range 5 {
		f.seed("beeper", f.chatID, day.Add(time.Duration(minute)*time.Minute), fmt.Sprintf("message %d", minute))
	}
	_, err := f.run()
	require.NoError(err)
	counted.calls = 0
	counted.selectors = make(map[AffectedScope]int)

	result, err := f.worker.RunBackstop(context.Background(), f.gen, testEmbeddingPassScope())
	require.NoError(err)
	require.NotNil(result.Contextual)
	assert.True(result.Contextual.Converged)
	want := chatDayContextScope(f.chatID, day).selector
	assert.Equal(2, counted.selectors[want],
		"the chat-day selector should appear once in source reconciliation and once in ledger reconciliation")
}

func TestContextWorker_DocumentTooLargeKeepsValidSiblingInSameScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	badID := f.seed("beeper", f.chatID, day, "reject-this-session")
	goodID := f.seed("beeper", f.chatID, day.Add(time.Hour), "keep-this-session")
	f.client.rejectText = "reject-this-session"

	result, err := f.run()
	require.NoError(err)
	require.NotNil(result.Contextual)
	assert.False(result.Contextual.Converged,
		"a pass with rejected coverage must report incomplete before activation rechecks the source")
	assert.GreaterOrEqual(result.Failed, 1)
	assert.GreaterOrEqual(result.Succeeded, 1)
	assert.Equal(1, f.client.Documents(), "the valid sibling vector should be reused while the oversized sibling is quarantined")
	assert.Equal(1, f.missing())
	docs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen,
		fmt.Sprintf("chat:%d:2026-08-08", f.chatID))
	require.NoError(err)
	require.Len(docs, 1)
	assert.Equal([]int64{goodID}, docs[0].Members)
	assert.NotContains(docs[0].Members, badID)
}

func TestContextWorker_IdleRunSkipsOrdinaryDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	discoveryPages := 0
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.Hooks.AfterOrdinaryDiscoveryPage = func(int) error {
			discoveryPages++
			return nil
		}
	})
	f.seed("email", f.chatID, time.Now().UTC(), "ordinary")
	_, err := f.run()
	require.NoError(err)
	discoveryPages = 0
	beforeCalls := f.client.Calls()

	result, err := f.run()
	require.NoError(err)
	require.NotNil(result.Contextual)
	assert.True(result.Contextual.Converged)
	assert.Zero(discoveryPages, "an idle normal run must not restart ordinary discovery from id zero")
	assert.Equal(beforeCalls, f.client.Calls())
}

func TestContextWorker_OrdinaryBodyEditReembedsOnNextNormalRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newContextWorkerFixture(t, nil)
	id := f.seed("email", f.chatID, time.Now().UTC(), "before")
	_, err := f.run()
	require.NoError(err)
	beforeDocuments := f.client.Documents()
	before, err := f.backend.GetDocument(context.Background(), f.gen, fmt.Sprintf("message:%d", id))
	require.NoError(err)

	require.NoError(f.store.UpsertMessageBody(id,
		sql.NullString{String: "after", Valid: true}, sql.NullString{}))
	result, err := f.run()
	require.NoError(err)
	require.NotNil(result.Contextual)
	assert.True(result.Contextual.Converged)
	assert.Equal(beforeDocuments+1, f.client.Documents())
	after, err := f.backend.GetDocument(context.Background(), f.gen, fmt.Sprintf("message:%d", id))
	require.NoError(err)
	assert.NotEqual(before.PublishedRevision, after.PublishedRevision)
}

func TestContextWorker_UnchangedRevisionFencesDelayedPublication(t *testing.T) {
	var racePublisher *contextFenceRacePublisher
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		racePublisher = &contextFenceRacePublisher{DocumentPublisher: deps.Publisher}
		deps.Publisher = racePublisher
	})
	id := f.seed("email", f.chatID, time.Now().UTC(), "revision a")
	_, err := f.run()
	require.NoError(t, err)
	initial, err := f.backend.GetDocument(t.Context(), f.gen, fmt.Sprintf("message:%d", id))
	require.NoError(t, err)
	initialCalls := f.client.Calls()
	initialWatermark := f.progress().ChangeSequence

	require.NoError(t, f.store.UpsertMessageBody(id,
		sql.NullString{String: "revision b", Valid: true}, sql.NullString{}))
	snapshot, err := BeginSourceSnapshot(t.Context(), f.store)
	require.NoError(t, err)
	staleDocuments, err := f.worker.deps.Assembler.AssembleScopes(t.Context(), snapshot,
		[]AffectedScope{{Kind: "email", MessageID: id}})
	require.NoError(t, err)
	staleSequence := snapshot.SourceSequence()
	require.NoError(t, snapshot.Close())
	require.Len(t, staleDocuments, 1)
	staleVectors := make([][][]float32, len(staleDocuments))
	for i, document := range staleDocuments {
		staleVectors[i] = make([][]float32, len(document.Chunks))
		for j := range document.Chunks {
			staleVectors[i][j] = []float32{9, 9, 9, 9}
		}
	}
	stalePublications, staleChunks, err := buildDocumentPublication(
		staleDocuments, staleVectors, make([]bool, len(staleDocuments)))
	require.NoError(t, err)

	require.NoError(t, f.store.UpsertMessageBody(id,
		sql.NullString{String: "revision a", Valid: true}, sql.NullString{}))
	latestSequence := latestContextSequence(t, f.store)
	require.Greater(t, latestSequence, staleSequence)
	racePublisher.beforeFence = func() error {
		return racePublisher.PublishScope(t.Context(), f.gen,
			fmt.Sprintf("message:%d", id), staleSequence, stalePublications, staleChunks)
	}

	_, err = f.run()
	require.ErrorIs(t, err, vector.ErrDocumentFenceChanged)
	assert.Equal(t, initialCalls, f.client.Calls(), "an unchanged desired revision must not call the provider")
	assert.Equal(t, initialWatermark, f.progress().ChangeSequence, "a rejected fence must hold the journal watermark")
	stale, err := f.backend.GetDocument(t.Context(), f.gen, initial.Key)
	require.NoError(t, err)
	assert.NotEqual(t, initial.PublishedRevision, stale.PublishedRevision)
	assert.Equal(t, staleSequence, stale.SourceSequence)

	result, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, initialCalls+1, f.client.Calls(), "retry must repair the stale publication")
	repaired, err := f.backend.GetDocument(t.Context(), f.gen, initial.Key)
	require.NoError(t, err)
	assert.Equal(t, initial.PublishedRevision, repaired.PublishedRevision)
	assert.Equal(t, latestSequence, repaired.SourceSequence)
	assert.Equal(t, latestSequence, f.progress().ChangeSequence)
	assert.Zero(t, f.missing())
}

func TestContextWorker_NonemptyToBlankJournalChangeStampsTerminalCoverage(t *testing.T) {
	tests := []struct {
		name string
		kind string
	}{
		{name: "ordinary", kind: "email"},
		{name: "contextual chat", kind: "beeper"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newContextWorkerFixture(t, nil)
			at := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
			id := f.seed(tt.kind, f.chatID, at, "before")

			first, err := f.run()
			require.NoError(err)
			require.NotNil(first.Contextual)
			require.True(first.Contextual.Converged)
			require.Contains(f.progress().ReconcileCursor, "done:")
			beforeDocuments := f.client.Documents()

			if tt.kind == "email" {
				_, err = f.store.DB().Exec(`UPDATE messages SET subject = NULL WHERE id = ?`, id)
				require.NoError(err)
			}
			require.NoError(f.store.UpsertMessageBody(id,
				sql.NullString{String: "", Valid: true}, sql.NullString{}))

			result, err := f.run()
			require.NoError(err)
			require.NotNil(result.Contextual)
			assert.True(result.Contextual.Converged)
			assert.Zero(f.missing())
			assert.Equal(beforeDocuments, f.client.Documents(), "blank content must not call the semantic provider")
			assert.Equal(latestContextSequence(t, f.store), f.progress().ChangeSequence)

			key := fmt.Sprintf("message:%d", id)
			if tt.kind == "beeper" {
				key = fmt.Sprintf("chat:%d:%s:%d", f.chatID, chatDocumentPolicyVersion, id)
			}
			record, err := f.backend.GetDocument(context.Background(), f.gen, key)
			require.NoError(err)
			assert.Equal(vector.DocumentTombstoned, record.State)
		})
	}
}

func TestContextWorker_DeduplicatesJournalAndRepairsEditMoveAndMetadata(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	id := f.seed("beeper", f.chatID, day, "before")
	_, err := f.run()
	require.NoError(t, err)
	before := f.client.Documents()
	require.NoError(t, f.store.UpsertMessageBody(id, sql.NullString{String: "after", Valid: true}, sql.NullString{}))
	_, err = f.store.DB().Exec(`UPDATE conversations SET title = 'Renamed' WHERE id = ?`, f.chatID)
	require.NoError(t, err)
	_, err = f.store.DB().Exec(`UPDATE participants SET display_name = 'Alicia' WHERE id = ?`, f.personID)
	require.NoError(t, err)
	result, err := f.run()
	require.NoError(t, err)
	assert.True(t, result.Contextual.Converged)
	assert.Greater(t, f.client.Documents(), before)
	assert.Equal(t, f.progress().ChangeSequence, result.Contextual.SourceSequence)
}

func TestContextWorker_DuplicateJournalEventsEmbedScopeOnce(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	id := f.seed("beeper", f.chatID, time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), "before")
	_, err := f.run()
	require.NoError(t, err)
	before := f.client.Documents()
	require.NoError(t, f.store.UpsertMessageBody(id, sql.NullString{String: "first edit", Valid: true}, sql.NullString{}))
	require.NoError(t, f.store.UpsertMessageBody(id, sql.NullString{String: "second edit", Valid: true}, sql.NullString{}))

	result, err := f.run()
	require.NoError(t, err)
	assert.Equal(t, before+1, f.client.Documents())
	assert.Equal(t, result.Contextual.SourceSequence, f.progress().ChangeSequence)
}

func TestContextWorker_ConversationTitleChangeRepublishesChatScope(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	f.seed("beeper", f.chatID, time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), "hello")
	_, err := f.run()
	require.NoError(t, err)
	before := f.client.Documents()
	_, err = f.store.DB().Exec(`UPDATE conversations SET title = 'Renamed alone' WHERE id = ?`, f.chatID)
	require.NoError(t, err)

	result, err := f.run()
	require.NoError(t, err)
	assert.Equal(t, before+1, f.client.Documents())
	assert.True(t, result.Contextual.Converged)
}

func TestContextWorker_ParticipantNameChangeRepublishesChatScope(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	f.seed("beeper", f.chatID, time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), "hello")
	_, err := f.run()
	require.NoError(t, err)
	before := f.client.Documents()
	_, err = f.store.DB().Exec(`UPDATE participants SET display_name = 'Alicia alone' WHERE id = ?`, f.personID)
	require.NoError(t, err)

	result, err := f.run()
	require.NoError(t, err)
	assert.Equal(t, before+1, f.client.Documents())
	assert.True(t, result.Contextual.Converged)
}

func TestContextWorker_ConversationParticipantChangeRepublishesChatScope(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	f.seed("beeper", f.chatID, time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), "hello")
	_, err := f.run()
	require.NoError(t, err)
	before := f.client.Documents()
	bob, err := f.store.EnsureParticipant("bob@example.test", "Bob", "example.test")
	require.NoError(t, err)
	require.NoError(t, f.store.EnsureConversationParticipant(f.chatID, bob, "member"))

	result, err := f.run()
	require.NoError(t, err)
	assert.Equal(t, before+1, f.client.Documents())
	assert.True(t, result.Contextual.Converged)
}

func TestContextWorker_TimestampAndConversationMoveRepairsOldAndNewScopes(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	oldTime := time.Date(2026, 8, 8, 23, 50, 0, 0, time.UTC)
	id := f.seed("beeper", f.chatID, oldTime, "moving")
	_, err := f.run()
	require.NoError(t, err)
	oldScope := fmt.Sprintf("chat:%d:2026-08-08", f.chatID)
	oldDocs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen, oldScope)
	require.NoError(t, err)
	require.Len(t, oldDocs, 1)

	newConversation, err := f.store.EnsureConversationWithType(f.sourceID, "chat-moved", "beeper", "Moved")
	require.NoError(t, err)
	require.NoError(t, f.store.EnsureConversationParticipant(newConversation, f.personID, "member"))
	newTime := oldTime.Add(20 * time.Minute)
	_, err = f.store.DB().Exec(`UPDATE messages SET conversation_id = ?, sent_at = ? WHERE id = ?`, newConversation, newTime, id)
	require.NoError(t, err)
	result, err := f.run()
	require.NoError(t, err)
	assert.True(t, result.Contextual.Converged)
	oldDocs, err = f.backend.ListDocumentsForScope(context.Background(), f.gen, oldScope)
	require.NoError(t, err)
	assert.Empty(t, oldDocs)
	newDocs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen,
		fmt.Sprintf("chat:%d:2026-08-09", newConversation))
	require.NoError(t, err)
	require.Len(t, newDocs, 1)
	assert.Equal(t, []int64{id}, newDocs[0].Members)
}

func TestContextWorker_MultiDayEditDoesNotRepairUnrelatedChatDay(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	firstDay := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	firstID := f.seed("beeper", f.chatID, firstDay, "first day")
	secondID := f.seed("beeper", f.chatID, firstDay.Add(24*time.Hour), "second day")
	_, err := f.run()
	require.NoError(t, err)
	firstScope := fmt.Sprintf("chat:%d:2026-08-08", f.chatID)
	tamperSequence := f.progress().ChangeSequence
	require.NoError(t, f.backend.PublishScope(context.Background(), f.gen, firstScope, tamperSequence,
		[]vector.DocumentPublication{{Key: firstScope + ":tampered", Kind: "chat-window", Revision: "tampered", SourceSequence: tamperSequence, Members: []int64{firstID}}},
		[]vector.Chunk{{MessageID: firstID, Vector: []float32{1, 2, 3, 4}}}))

	require.NoError(t, f.store.UpsertMessageBody(secondID,
		sql.NullString{String: "second day edited", Valid: true}, sql.NullString{}))
	_, err = f.run()
	require.NoError(t, err)
	firstDocs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen, firstScope)
	require.NoError(t, err)
	require.Len(t, firstDocs, 1)
	assert.Equal(t, "tampered", firstDocs[0].PublishedRevision,
		"a journal edit must not expand from one Beeper row to every day in its conversation")
}

func TestContextWorker_ChatBlockEditDoesNotRepairSiblingBlock(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, chatScopeMaxMessages+1)
	for i := range chatScopeMaxMessages + 1 {
		ids = append(ids, f.seed("beeper", f.chatID, day.Add(time.Duration(i)*time.Minute),
			fmt.Sprintf("bounded edit message %d", i)))
	}
	_, err := f.run()
	require.NoError(t, err)
	firstScope := chatMessageContextScope(f.chatID, day, ids[0]).key
	lastScope := chatMessageContextScope(f.chatID, day, ids[len(ids)-1]).key
	require.NotEqual(t, firstScope, lastScope)
	tamperSequence := f.progress().ChangeSequence
	require.NoError(t, f.backend.PublishScope(t.Context(), f.gen, firstScope, tamperSequence,
		[]vector.DocumentPublication{{
			Key: firstScope + ":tampered", Kind: "chat-window", Revision: "tampered",
			SourceSequence: tamperSequence, Members: []int64{ids[0]},
		}}, []vector.Chunk{{MessageID: ids[0], Vector: []float32{1, 2, 3, 4}}}))

	require.NoError(t, f.store.UpsertMessageBody(ids[len(ids)-1],
		sql.NullString{String: "last block edited", Valid: true}, sql.NullString{}))
	_, err = f.run()
	require.NoError(t, err)
	firstDocs, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, firstScope)
	require.NoError(t, err)
	require.Len(t, firstDocs, 1)
	assert.Equal(t, "tampered", firstDocs[0].PublishedRevision,
		"a bounded edit must not expand into a sibling ID block")
}

func TestContextWorker_UndatedDeleteTombstonesOldChatScope(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	id := f.seed("beeper", f.chatID, time.Time{}, "undated")
	_, err := f.run()
	require.NoError(t, err)
	scope := fmt.Sprintf("chat:%d:undated", f.chatID)
	docs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen, scope)
	require.NoError(t, err)
	require.Len(t, docs, 1)

	_, err = f.store.DB().Exec(`DELETE FROM messages WHERE id = ?`, id)
	require.NoError(t, err)
	_, err = f.run()
	require.NoError(t, err)
	doc, err := f.backend.GetDocument(context.Background(), f.gen, docs[0].Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, doc.State)
}

func TestContextWorker_UndatedMoveTombstonesOldAndPublishesNewChatScope(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	id := f.seed("beeper", f.chatID, time.Time{}, "undated move")
	_, err := f.run()
	require.NoError(t, err)
	oldScope := fmt.Sprintf("chat:%d:undated", f.chatID)
	oldDocs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen, oldScope)
	require.NoError(t, err)
	require.Len(t, oldDocs, 1)
	newConversation, err := f.store.EnsureConversationWithType(f.sourceID, "undated-moved", "beeper", "Moved")
	require.NoError(t, err)
	require.NoError(t, f.store.EnsureConversationParticipant(newConversation, f.personID, "member"))
	_, err = f.store.DB().Exec(`UPDATE messages SET conversation_id = ? WHERE id = ?`, newConversation, id)
	require.NoError(t, err)

	_, err = f.run()
	require.NoError(t, err)
	oldDoc, err := f.backend.GetDocument(context.Background(), f.gen, oldDocs[0].Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, oldDoc.State)
	newDocs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen,
		fmt.Sprintf("chat:%d:undated", newConversation))
	require.NoError(t, err)
	require.Len(t, newDocs, 1)
	assert.Equal(t, []int64{id}, newDocs[0].Members)
}

func TestContextWorker_UndatedEditDoesNotRepairDatedSiblingScope(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	datedID := f.seed("beeper", f.chatID, day, "dated sibling")
	undatedID := f.seed("beeper", f.chatID, time.Time{}, "undated target")
	_, err := f.run()
	require.NoError(t, err)
	datedScope := fmt.Sprintf("chat:%d:2026-08-08", f.chatID)
	tamperSequence := f.progress().ChangeSequence
	require.NoError(t, f.backend.PublishScope(context.Background(), f.gen, datedScope, tamperSequence,
		[]vector.DocumentPublication{{Key: datedScope + ":tampered", Kind: "chat-window", Revision: "tampered", SourceSequence: tamperSequence, Members: []int64{datedID}}},
		[]vector.Chunk{{MessageID: datedID, Vector: []float32{1, 2, 3, 4}}}))

	require.NoError(t, f.store.UpsertMessageBody(undatedID,
		sql.NullString{String: "undated edited", Valid: true}, sql.NullString{}))
	_, err = f.run()
	require.NoError(t, err)
	datedDocs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen, datedScope)
	require.NoError(t, err)
	require.Len(t, datedDocs, 1)
	assert.Equal(t, "tampered", datedDocs[0].PublishedRevision,
		"an undated selector must not expand to dated Beeper rows")
}

func TestContextWorker_MeetingBodyEditDoesNotRepairBeeperSharedDay(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	beeperID := f.seed("beeper", f.chatID, day, "chat sibling")
	meetingID := f.seed("meeting_transcript", f.chatID, day, assemblyMeetingBody())
	_, err := f.run()
	require.NoError(t, err)
	chatScope := fmt.Sprintf("chat:%d:2026-08-08", f.chatID)
	tamperSequence := f.progress().ChangeSequence
	require.NoError(t, f.backend.PublishScope(context.Background(), f.gen, chatScope, tamperSequence,
		[]vector.DocumentPublication{{Key: chatScope + ":tampered", Kind: "chat-window", Revision: "tampered", SourceSequence: tamperSequence, Members: []int64{beeperID}}},
		[]vector.Chunk{{MessageID: beeperID, Vector: []float32{1, 2, 3, 4}}}))

	require.NoError(t, f.store.UpsertMessageBody(meetingID,
		sql.NullString{String: assemblyMeetingBody() + "\nEdited", Valid: true}, sql.NullString{}))
	_, err = f.run()
	require.NoError(t, err)
	chatDocs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen, chatScope)
	require.NoError(t, err)
	require.Len(t, chatDocs, 1)
	assert.Equal(t, "tampered", chatDocs[0].PublishedRevision,
		"a meeting event must not invent a Beeper scope from shared coordinates")
}

func TestContextWorker_BeeperToOrdinaryRepairsOldScopeWithoutReembeddingSibling(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	targetID := f.seed("beeper", f.chatID, day, "transition target")
	siblingID := f.seed("beeper", f.chatID, day.Add(3*time.Hour), "chat sibling")
	_, err := f.run()
	require.NoError(t, err)
	chatScope := fmt.Sprintf("chat:%d:2026-08-08", f.chatID)
	beforeDocs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen, chatScope)
	require.NoError(t, err)
	require.Len(t, beforeDocs, 2)
	var targetKey string
	for _, doc := range beforeDocs {
		if len(doc.Members) == 1 && doc.Members[0] == targetID {
			targetKey = doc.Key
		}
	}
	require.NotEmpty(t, targetKey)
	beforeEmbedded := f.client.Documents()

	_, err = f.store.DB().Exec(`UPDATE messages SET message_type = 'email', subject = 'ordinary now' WHERE id = ?`, targetID)
	require.NoError(t, err)
	result, err := f.run()
	require.NoError(t, err)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, beforeEmbedded+1, f.client.Documents(),
		"the remaining Beeper sibling keeps its cached unchanged revision")
	oldTarget, err := f.backend.GetDocument(context.Background(), f.gen, targetKey)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, oldTarget.State)
	ordinary, err := f.backend.GetDocument(context.Background(), f.gen, fmt.Sprintf("message:%d", targetID))
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentCurrent, ordinary.State)
	chatDocs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen, chatScope)
	require.NoError(t, err)
	require.Len(t, chatDocs, 1)
	assert.Equal(t, []int64{siblingID}, chatDocs[0].Members)
}

func TestContextWorker_OrdinaryToBeeperRepairsOldAndBoundedNewScopes(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	id := f.seed("email", f.chatID, day, "ordinary before transition")
	_, err := f.run()
	require.NoError(t, err)
	ordinaryKey := fmt.Sprintf("message:%d", id)
	ordinaryBefore, err := f.backend.GetDocument(t.Context(), f.gen, ordinaryKey)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentCurrent, ordinaryBefore.State)

	_, err = f.store.DB().Exec(`UPDATE messages SET message_type = 'beeper', subject = NULL WHERE id = ?`, id)
	require.NoError(t, err)
	result, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	ordinaryAfter, err := f.backend.GetDocument(t.Context(), f.gen, ordinaryKey)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, ordinaryAfter.State)
	chatScope := chatMessageContextScope(f.chatID, day, id)
	chatDocuments, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, chatScope.key)
	require.NoError(t, err)
	require.Len(t, chatDocuments, 1)
	assert.Equal(t, []int64{id}, chatDocuments[0].Members)
}

func TestContextWorker_TypeTransitionRejectionKeepsCanonicalScopeUncovered(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	id := f.seed("email", f.chatID, day, "ordinary before rejected transition")
	_, err := f.run()
	require.NoError(t, err)
	ordinaryKey := fmt.Sprintf("message:%d", id)

	meetingBody := strings.Replace(assemblyMeetingBody(), "Decision summary.", "reject-type-transition", 1)
	_, err = f.store.DB().Exec(
		`UPDATE messages SET message_type = 'meeting_transcript', subject = NULL WHERE id = ?`, id)
	require.NoError(t, err)
	require.NoError(t, f.store.UpsertMessageBody(id,
		sql.NullString{String: meetingBody, Valid: true}, sql.NullString{}))
	f.client.rejectText = "reject-type-transition"

	result, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.False(t, result.Contextual.Converged)
	assert.Equal(t, 1, f.missing(),
		"an obsolete ordinary scope must not stamp the rejected canonical meeting scope")
	ordinary, err := f.backend.GetDocument(t.Context(), f.gen, ordinaryKey)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, ordinary.State)
	_, err = f.backend.GetDocument(t.Context(), f.gen, fmt.Sprintf("meeting:%d", id))
	require.Error(t, err)
	var embedGen sql.NullInt64
	require.NoError(t, f.store.DB().QueryRow(`SELECT embed_gen FROM messages WHERE id = ?`, id).Scan(&embedGen))
	assert.False(t, embedGen.Valid)

	f.client.rejectText = ""
	goodID := f.seed("email", f.chatID, day.Add(time.Minute), "later valid journal work")
	beforeDocuments := f.client.Documents()
	_, err = f.run()
	require.NoError(t, err)
	assert.Equal(t, beforeDocuments+1, f.client.Documents(),
		"the rejected transition must not prevent later journal work")
	good, err := f.backend.GetDocument(t.Context(), f.gen, fmt.Sprintf("message:%d", goodID))
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentCurrent, good.State)
	assert.Equal(t, latestContextSequence(t, f.store), f.progress().ChangeSequence)
	require.NoError(t, f.store.DB().QueryRow(`SELECT embed_gen FROM messages WHERE id = ?`, id).Scan(&embedGen))
	assert.False(t, embedGen.Valid)
}

func TestContextWorker_BeeperToMeetingMoveRepairsExactOldAndNewKinds(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	oldTime := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	id := f.seed("beeper", f.chatID, oldTime, "type move")
	_, err := f.run()
	require.NoError(t, err)
	oldScope := fmt.Sprintf("chat:%d:2026-08-08", f.chatID)
	oldDocs, err := f.backend.ListDocumentsForScope(context.Background(), f.gen, oldScope)
	require.NoError(t, err)
	require.Len(t, oldDocs, 1)
	meetingConversation, err := f.store.EnsureConversation(f.sourceID, "type-move-meeting", "Meeting")
	require.NoError(t, err)
	_, err = f.store.DB().Exec(`UPDATE messages SET message_type = 'meeting_transcript', conversation_id = ?, sent_at = ? WHERE id = ?`,
		meetingConversation, oldTime.Add(24*time.Hour), id)
	require.NoError(t, err)
	require.NoError(t, f.store.UpsertMessageBody(id,
		sql.NullString{String: assemblyMeetingBody(), Valid: true}, sql.NullString{}))

	result, err := f.run()
	require.NoError(t, err)
	assert.True(t, result.Contextual.Converged)
	oldDoc, err := f.backend.GetDocument(context.Background(), f.gen, oldDocs[0].Key)
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, oldDoc.State)
	meetingDoc, err := f.backend.GetDocument(context.Background(), f.gen, fmt.Sprintf("meeting:%d", id))
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentCurrent, meetingDoc.State)
}

func TestContextWorker_MeetingDeletePublishesTombstoneImmediately(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	conversationID, err := f.store.EnsureConversation(f.sourceID, "meeting", "Meeting")
	require.NoError(t, err)
	id := f.seed("meeting_transcript", conversationID,
		time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC), assemblyMeetingBody())
	_, err = f.run()
	require.NoError(t, err)
	_, err = f.store.DB().Exec(`DELETE FROM messages WHERE id = ?`, id)
	require.NoError(t, err)
	result, err := f.run()
	require.NoError(t, err)
	record, err := f.backend.GetDocument(context.Background(), f.gen, fmt.Sprintf("meeting:%d", id))
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, record.State)
	assert.True(t, result.Contextual.Converged)
}

func TestContextWorker_MixedOrdinaryContextualAndSizeRecovery(t *testing.T) {
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {})
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	otherConversation, err := f.store.EnsureConversation(f.sourceID, "ordinary", "Ordinary")
	require.NoError(t, err)
	f.seed("email", otherConversation, day, "mail")
	f.seed("sms", otherConversation, day, "text")
	f.seed("beeper", f.chatID, day, "chat")
	f.seed("meeting_transcript", otherConversation, day, assemblyMeetingBody())
	result, err := f.run()
	require.NoError(t, err)
	assert.True(t, result.Contextual.Converged)
	assert.Zero(t, f.missing())
}

func TestContextWorker_RecognizedSize400SplitsOnlyBetweenDocuments(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	f.client.sizeOnce = true
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	f.seed("beeper", f.chatID, day, "first window")
	f.seed("beeper", f.chatID, day.Add(time.Hour), "second window")
	result, err := f.run()
	require.NoError(t, err)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, 3, f.client.calls, "one rejected group followed by two complete document calls")
	assert.Equal(t, 2, f.client.Documents())
}

func TestContextWorker_VoyageSizeFallbackDoesNotRepeatSuccessfulSibling(t *testing.T) {
	var handlerMu sync.Mutex
	var handlerErr error
	var successfulSingletons int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request voyageRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			handlerMu.Lock()
			handlerErr = err
			handlerMu.Unlock()
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		if len(request.Inputs) > 1 || strings.Contains(strings.Join(request.Inputs[0], "\n"), "second window") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			if _, err := w.Write([]byte(`{"detail":"The total number of tokens in the batch exceeds the limit"}`)); err != nil {
				handlerMu.Lock()
				handlerErr = err
				handlerMu.Unlock()
			}
			return
		}
		handlerMu.Lock()
		successfulSingletons++
		handlerMu.Unlock()
		response := voyageResponse{}
		response.Data = make([]struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			} `json:"data"`
			Index int `json:"index"`
		}, 1)
		response.Data[0].Data = make([]struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}, len(request.Inputs[0]))
		for i := range response.Data[0].Data {
			response.Data[0].Data[i].Index = i
			response.Data[0].Data[i].Embedding = []float32{1, 2, 3, 4}
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			handlerMu.Lock()
			handlerErr = err
			handlerMu.Unlock()
		}
	}))
	t.Cleanup(server.Close)

	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.Client = NewVoyageClient(VoyageConfig{
			Endpoint: server.URL, Model: "voyage-context-4", Dimension: 4, MaxRetries: 1,
		})
	})
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	f.seed("beeper", f.chatID, day, "first window")
	f.seed("beeper", f.chatID, day.Add(time.Hour), "second window")

	_, err := f.run()

	require.NoError(t, err)
	handlerMu.Lock()
	defer handlerMu.Unlock()
	require.NoError(t, handlerErr)
	assert.Equal(t, 1, successfulSingletons, "a successful sibling must not be embedded twice")
}

func TestContextWorker_LaterPackedFailurePublishesAndResumesSuccessfulScopePrefix(t *testing.T) {
	assert := assert.New(t)
	var handlerMu sync.Mutex
	var handlerErr error
	requestsByDocument := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request voyageRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			handlerMu.Lock()
			handlerErr = err
			handlerMu.Unlock()
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		joined := strings.Join(request.Inputs[0], "\n")
		document := "first"
		if strings.Contains(joined, "second retry") {
			document = "second"
		}
		handlerMu.Lock()
		requestsByDocument[document]++
		attempt := requestsByDocument[document]
		handlerMu.Unlock()
		if document == "second" && attempt == 1 {
			http.Error(w, "synthetic provider failure", http.StatusServiceUnavailable)
			return
		}
		data := make([]map[string]any, len(request.Inputs))
		for outer, chunks := range request.Inputs {
			inner := make([]map[string]any, len(chunks))
			for index := range chunks {
				inner[index] = map[string]any{"embedding": []float32{1, 2, 3, 4}, "index": index}
			}
			data[outer] = map[string]any{"data": inner, "index": outer}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			handlerMu.Lock()
			handlerErr = err
			handlerMu.Unlock()
		}
	}))
	t.Cleanup(server.Close)

	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.Client = NewVoyageClient(VoyageConfig{
			Endpoint: server.URL, Model: "voyage-context-4", Dimension: 4, MaxRetries: 1,
			Limits: RequestLimits{MaxDocuments: 1, MaxChunks: 16_000, MaxUTF8Bytes: 100_000},
		})
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "ordinary", "Ordinary")
	require.NoError(t, err)
	firstID := f.seed("email", conversationID, time.Now().UTC(), "first prefix")
	secondID := f.seed("email", conversationID, time.Now().UTC().Add(time.Minute), "second retry")

	_, err = f.run()
	require.Error(t, err)
	_, err = f.backend.GetDocument(t.Context(), f.gen, fmt.Sprintf("message:%d", firstID))
	require.NoError(t, err, "the successful packed request must be durably published before returning the later error")
	_, err = f.backend.GetDocument(t.Context(), f.gen, fmt.Sprintf("message:%d", secondID))
	require.Error(t, err)
	assert.Equal(1, f.missing())

	result, err := f.run()
	require.NoError(t, err)
	assert.True(result.Contextual.Converged)
	assert.Zero(f.missing())
	handlerMu.Lock()
	defer handlerMu.Unlock()
	require.NoError(t, handlerErr)
	assert.Equal(1, requestsByDocument["first"], "the successful provider request must not be billed twice")
	assert.Equal(2, requestsByDocument["second"], "only the failed document is retried")
}

func TestContextWorker_LaterPackedFailureRetainsSuccessfulDocumentWithinScope(t *testing.T) {
	assert := assert.New(t)
	client := &contextPartialSemanticFake{}
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.Client = client
	})
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	f.seed("beeper", f.chatID, day, "first window")
	f.seed("beeper", f.chatID, day.Add(time.Hour), "second window")

	_, err := f.run()
	require.Error(t, err)
	documents, err := f.backend.ListDocumentsForScope(t.Context(), f.gen,
		fmt.Sprintf("chat:%d:2026-08-08", f.chatID))
	require.NoError(t, err)
	assert.Len(documents, 1, "the successful document is retained without publishing an incomplete coverage stamp")
	assert.Equal(2, f.missing(), "an incomplete atomic scope must remain uncovered")

	result, err := f.run()
	require.NoError(t, err)
	assert.True(result.Contextual.Converged)
	assert.Zero(f.missing())
	client.mu.Lock()
	defer client.mu.Unlock()
	require.Len(t, client.calls, 2)
	assert.Len(client.calls[0], 2)
	assert.Len(client.calls[1], 1, "the published first document must be preserved on retry")
	assert.Contains(strings.Join(client.calls[1][0].Chunks, "\n"), "second window")
	assert.NotContains(strings.Join(client.calls[1][0].Chunks, "\n"), "first window")
}

func TestContextWorker_LaterPackedReplacementFailurePreservesUnresolvedExistingDocument(t *testing.T) {
	assert := assert.New(t)
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	firstID := f.seed("beeper", f.chatID, day, "first original window")
	secondID := f.seed("beeper", f.chatID, day.Add(time.Hour), "second original window")

	result, err := f.run()
	require.NoError(t, err)
	require.True(t, result.Contextual.Converged)
	scopeKey := fmt.Sprintf("chat:%d:2026-08-08", f.chatID)
	before, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, scopeKey)
	require.NoError(t, err)
	require.Len(t, before, 2)
	beforeRevisions := make(map[string]string, len(before))
	for _, document := range before {
		beforeRevisions[document.Key] = document.PublishedRevision
	}

	client := &contextPartialSemanticFake{}
	f.deps.Client = client
	f.restartWorker()
	require.NoError(t, f.store.UpsertMessageBody(firstID,
		sql.NullString{String: "first replacement window", Valid: true}, sql.NullString{}))
	require.NoError(t, f.store.UpsertMessageBody(secondID,
		sql.NullString{String: "second replacement window", Valid: true}, sql.NullString{}))

	_, err = f.run()
	require.Error(t, err)
	afterPartial, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, scopeKey)
	require.NoError(t, err)
	require.Len(t, afterPartial, 2,
		"a partial replacement must not tombstone the unresolved document's existing vectors")
	changed := 0
	unchanged := 0
	for _, document := range afterPartial {
		if document.PublishedRevision == beforeRevisions[document.Key] {
			unchanged++
		} else {
			changed++
		}
	}
	assert.Equal(1, changed, "the successful replacement must be retained")
	assert.Equal(1, unchanged, "the unresolved replacement must keep its prior current document")
	assert.Equal(2, f.missing(), "the incomplete scope must remain uncovered")

	result, err = f.run()
	require.NoError(t, err)
	assert.True(result.Contextual.Converged)
	assert.Zero(f.missing())
	client.mu.Lock()
	defer client.mu.Unlock()
	require.Len(t, client.calls, 2)
	assert.Len(client.calls[0], 2)
	assert.Len(client.calls[1], 1, "only the unresolved replacement must be retried")
}

func TestContextWorker_PermanentAuthFailurePublishesNothingAndHoldsWatermark(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	f.seed("beeper", f.chatID, time.Now().UTC(), "secret")
	pinnedSequence := latestContextSequence(t, f.store)
	f.client.err = fmt.Errorf("HTTP 401: %w", ErrPermanent4xx)
	_, err := f.run()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPermanent4xx)
	assert.Zero(t, f.client.Documents())
	assert.Equal(t, pinnedSequence, f.progress().ChangeSequence)
	assert.Equal(t, 1, f.missing())
}

func TestContextWorker_DocumentTooLargeLeavesOnlyItsScopeUncovered(t *testing.T) {
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 1
		deps.ReconcileBatchSize = 1
	})
	_, err := f.run()
	require.NoError(t, err)
	conversationID, err := f.store.EnsureConversation(f.sourceID, "ordinary", "Ordinary")
	require.NoError(t, err)
	badID := f.seed("email", conversationID, time.Now().UTC(), "reject-this-document")
	goodID := f.seed("email", conversationID, time.Now().UTC().Add(time.Minute), "keep-this-document")
	f.client.rejectText = "reject-this-document"

	result, err := f.run()
	require.NoError(t, err)
	assert.False(t, result.Contextual.Converged,
		"a pass with rejected coverage must report incomplete before activation rechecks the source")
	assert.GreaterOrEqual(t, result.Failed, 1)
	assert.GreaterOrEqual(t, result.Succeeded, 1)
	assert.Equal(t, 1, f.missing())
	_, err = f.backend.GetDocument(context.Background(), f.gen, fmt.Sprintf("message:%d", badID))
	require.Error(t, err)
	good, err := f.backend.GetDocument(context.Background(), f.gen, fmt.Sprintf("message:%d", goodID))
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentCurrent, good.State)
	assert.Equal(t, latestContextSequence(t, f.store), f.progress().ChangeSequence)
	assert.Contains(t, f.progress().ReconcileCursor, "done:")
	beforeRetry := f.client.Calls()

	_, err = f.run()
	require.NoError(t, err)
	assert.Equal(t, beforeRetry, f.client.Calls(), "normal runs must not let one rejected document block or hot-loop")
	assert.Equal(t, 1, f.missing())

	f.client.rejectText = ""
	result, err = f.worker.RunBackstop(context.Background(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
	assert.Zero(t, f.missing())
	bad, err := f.backend.GetDocument(context.Background(), f.gen, fmt.Sprintf("message:%d", badID))
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentCurrent, bad.State)
}

func TestContextWorker_TokenDenseDocumentTruncatesInsteadOfBlockingActivation(t *testing.T) {
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		policy := AssemblyPolicy{MaxChunkRunes: 16384, ChatGap: 30 * time.Minute}
		deps.Assembler = CompositeAssembler{Policy: policy, Chat: ChatWindowAssembler{Policy: policy}}
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "ordinary", "Ordinary")
	require.NoError(t, err)
	// The provider rejects on a marker in the document tail; a byte-truncated
	// retry no longer contains it, standing in for a token-dense document that
	// passes local byte packing but exceeds the provider token limit.
	body := strings.Repeat("filler words for a token dense document ", 200) + "reject-huge-document"
	id := f.seed("email", conversationID, time.Now().UTC(), body)
	f.client.rejectText = "reject-huge-document"

	result, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged,
		"a truncated document must converge instead of blocking activation")
	assert.Zero(t, result.Failed)
	assert.Equal(t, 1, result.Truncated)
	assert.Zero(t, f.missing())
	doc, err := f.backend.GetDocument(context.Background(), f.gen, fmt.Sprintf("message:%d", id))
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentCurrent, doc.State)

	// Published offsets must describe only the truncated text that was
	// actually embedded, not the original full chunk.
	var start, end int
	var truncated bool
	require.NoError(t, f.backend.DB().QueryRow(
		`SELECT chunk_char_start, chunk_char_end, truncated FROM embeddings WHERE message_id = ?`, id).
		Scan(&start, &end, &truncated))
	assert.Zero(t, start)
	assert.True(t, truncated, "stored chunk must be flagged truncated")
	assert.Positive(t, end)
	assert.Less(t, end, utf8.RuneCountInString(body),
		"stored span must end within the embedded truncated prefix")

	beforeIdle := f.client.Calls()
	_, err = f.run()
	require.NoError(t, err)
	assert.Equal(t, beforeIdle, f.client.Calls(),
		"a truncated document must not re-embed once its revision is published")
}

func TestContextWorker_DocumentTooLargeClearsStaleCoverageAfterMetadataChange(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	messageID := f.seed("beeper", f.chatID, day, "reject-after-title-change")
	result, err := f.run()
	require.NoError(t, err)
	require.True(t, result.Contextual.Converged)
	assert.Zero(t, f.missing())

	f.client.rejectText = "reject-after-title-change"
	_, err = f.store.DB().Exec(`UPDATE conversations SET title = 'Changed title' WHERE id = ?`, f.chatID)
	require.NoError(t, err)
	result, err = f.run()
	require.NoError(t, err)
	require.NotNil(t, result.Contextual)
	assert.False(t, result.Contextual.Converged,
		"a rejected replacement must not leave the generation falsely converged")
	assert.Equal(t, 1, f.missing(), "every member of the rejected document must be uncovered")
	docs, err := f.backend.ListDocumentsForScope(t.Context(), f.gen, chatDayContextScope(f.chatID, day).key)
	require.NoError(t, err)
	assert.Empty(t, docs,
		"complete-scope publication may remove the stale vector only when coverage is also reset")

	var embedGen sql.NullInt64
	require.NoError(t, f.store.DB().QueryRow(`SELECT embed_gen FROM messages WHERE id = ?`, messageID).Scan(&embedGen))
	assert.False(t, embedGen.Valid)
}

func TestContextWorker_DocumentTooLargeDoesNotStarveLaterDiscovery(t *testing.T) {
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ChangeBatchSize = 4
		deps.ReconcileBatchSize = 4
		deps.MaxRunUTF8Bytes = 1
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "ordinary", "Ordinary")
	require.NoError(t, err)
	day := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	f.seed("email", conversationID, day, "reject-first")
	f.seed("email", conversationID, day.Add(time.Minute), "reject-second")
	goodID := f.seed("email", conversationID, day.Add(2*time.Minute), "later-valid-document")
	f.client.rejectText = "reject-"

	var result RunResult
	for range 3 {
		result, err = f.run()
		require.NoError(t, err)
	}
	good, err := f.backend.GetDocument(t.Context(), f.gen, fmt.Sprintf("message:%d", goodID))
	require.NoError(t, err, "durable discovery progress must reach work after permanent rejections")
	assert.Equal(t, vector.DocumentCurrent, good.State)
	assert.Equal(t, 2, f.missing(), "only the two permanently rejected documents stay uncovered")
	for attempt := 0; attempt < 10 && !result.Contextual.Converged; attempt++ {
		result, err = f.run()
		require.NoError(t, err)
	}
	assert.True(t, result.Contextual.Converged,
		"durable discovery and reconciliation cursors must let the CLI loop terminate")
}

func TestContextWorker_GroupCASMissLeavesScopeUncoveredAndWatermarkHeld(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	id := f.seed("beeper", f.chatID, time.Now().UTC(), "before")
	pinnedSequence := latestContextSequence(t, f.store)
	f.client.before = func() {
		f.client.before = nil
		_, err := f.store.DB().Exec(`UPDATE message_bodies SET body_text = 'raced' WHERE message_id = ?`, id)
		require.NoError(t, err)
		_, err = f.store.DB().Exec(`UPDATE messages SET last_modified = '2099-01-01 00:00:00' WHERE id = ?`, id)
		require.NoError(t, err)
	}
	_, err := f.run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "coverage CAS")
	assert.Equal(t, pinnedSequence, f.progress().ChangeSequence)
	assert.Greater(t, latestContextSequence(t, f.store), f.progress().ChangeSequence)
	assert.Equal(t, 1, f.missing())
}

func TestContextWorker_MetadataCASMissLeavesWholeScopeUncovered(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	f.seed("beeper", f.chatID, time.Now().UTC(), "before")
	pinnedSequence := latestContextSequence(t, f.store)
	f.client.before = func() {
		f.client.before = nil
		_, err := f.store.DB().Exec(`UPDATE conversations SET title = 'raced title' WHERE id = ?`, f.chatID)
		require.NoError(t, err)
	}
	_, err := f.run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "coverage CAS")
	assert.Equal(t, pinnedSequence, f.progress().ChangeSequence)
	assert.Greater(t, latestContextSequence(t, f.store), f.progress().ChangeSequence)
	assert.Equal(t, 1, f.missing())
}

func TestContextWorker_BlankRowCASMissLeavesCoverageUnchanged(t *testing.T) {
	snapshots := 0
	var f *contextWorkerFixture
	f = newContextWorkerFixture(t, func(d *ContextWorkerDeps) {
		d.Hooks.AfterSnapshot = func(SourceSnapshot) error {
			snapshots++
			if snapshots != 1 {
				return nil
			}
			_, err := f.store.DB().Exec(`UPDATE messages SET subject = 'raced blank', last_modified = '2099-01-01 00:00:00' WHERE source_message_id = 'email-1'`)
			return err
		}
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "blank", "Blank")
	require.NoError(t, err)
	id := f.seed("email", conversationID, time.Now().UTC(), "")
	_, err = f.store.DB().Exec(`UPDATE messages SET subject = NULL WHERE id = ?`, id)
	require.NoError(t, err)

	_, err = f.run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "coverage CAS missed")
	assert.Equal(t, 1, f.missing())
}

func TestContextWorker_SkippedRowCASMissLeavesCoverageUnchanged(t *testing.T) {
	snapshots := 0
	var f *contextWorkerFixture
	f = newContextWorkerFixture(t, func(d *ContextWorkerDeps) {
		d.Assembler = CompositeAssembler{Policy: AssemblyPolicy{
			MaxChunkRunes: 200,
			SkipMessage:   func(AssemblyMessage) bool { return true },
		}}
		d.Hooks.AfterSnapshot = func(SourceSnapshot) error {
			snapshots++
			if snapshots != 1 {
				return nil
			}
			_, err := f.store.DB().Exec(`UPDATE messages SET subject = 'raced skip', last_modified = '2099-01-01 00:00:00' WHERE source_message_id = 'email-1'`)
			return err
		}
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "skip", "Skip")
	require.NoError(t, err)
	f.seed("email", conversationID, time.Now().UTC(), "skipped")

	_, err = f.run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "coverage CAS missed")
	assert.Equal(t, 1, f.missing())
}

func TestContextWorker_CrashBoundariesReplayIdempotently(t *testing.T) {
	boundaries := []struct {
		name string
		set  func(*ContextWorkerDeps, func() error)
	}{
		{"before-index", func(d *ContextWorkerDeps, crash func() error) { d.Hooks.BeforePublish = crash }},
		{"after-index", func(d *ContextWorkerDeps, crash func() error) { d.Hooks.AfterPublish = crash }},
		{"after-coverage", func(d *ContextWorkerDeps, crash func() error) { d.Hooks.AfterCoverage = crash }},
	}
	for _, boundary := range boundaries {
		t.Run(boundary.name, func(t *testing.T) {
			crashed := false
			f := newContextWorkerFixture(t, func(d *ContextWorkerDeps) {
				boundary.set(d, func() error {
					if crashed {
						return nil
					}
					crashed = true
					return errors.New("synthetic crash")
				})
			})
			f.seed("beeper", f.chatID, time.Now().UTC(), "one")
			_, err := f.run()
			require.Error(t, err)
			result, err := f.run()
			require.NoError(t, err)
			assert.True(t, result.Contextual.Converged)
			assert.Zero(t, f.missing())
		})
	}
}

func TestContextWorker_BackstopReconcilesBelowWatermarkAndOrphanWithBoundedCursor(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	conversationID, err := f.store.EnsureConversation(f.sourceID, "mail", "Mail")
	require.NoError(t, err)
	first := f.seed("email", conversationID, time.Now().UTC(), "first")
	f.seed("email", conversationID, time.Now().UTC(), "second")
	_, err = f.run()
	require.NoError(t, err)
	_, err = f.store.DB().Exec(`UPDATE messages SET embed_gen = NULL WHERE id = ?`, first)
	require.NoError(t, err)
	orphanSequence := f.progress().ChangeSequence
	require.NoError(t, f.backend.PublishScope(context.Background(), f.gen, "message:999999", orphanSequence,
		[]vector.DocumentPublication{{Key: "message:999999", Kind: "ordinary-message", Revision: "orphan", SourceSequence: orphanSequence, Members: []int64{999999}}},
		[]vector.Chunk{{MessageID: 999999, Vector: []float32{1, 2, 3, 4}}}))
	result, err := f.worker.RunBackstop(context.Background(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.True(t, result.Contextual.Converged)
	assert.Zero(t, f.missing())
	orphan, err := f.backend.GetDocument(context.Background(), f.gen, "message:999999")
	require.NoError(t, err)
	assert.Equal(t, vector.DocumentTombstoned, orphan.State)
	assert.Contains(t, f.progress().ReconcileCursor, "done:")
}

func TestContextWorker_ReconcileSourceUsesBoundedRowPagesDespiteCurrentCoverage(t *testing.T) {
	var pageRows []int
	f := newContextWorkerFixture(t, func(d *ContextWorkerDeps) {
		d.ReconcileBatchSize = 2
		d.Hooks.AfterSourcePage = func(rows int) error {
			pageRows = append(pageRows, rows)
			return nil
		}
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "bounded-source", "Bounded")
	require.NoError(t, err)
	for i := range 5 {
		f.seed("email", conversationID, time.Date(2026, 8, 8, 9, i, 0, 0, time.UTC), fmt.Sprintf("mail %d", i))
	}
	_, err = f.run()
	require.NoError(t, err)
	pageRows = nil

	result, err := f.worker.RunBackstop(context.Background(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	assert.True(t, result.Contextual.Converged)
	require.GreaterOrEqual(t, len(pageRows), 3)
	total := 0
	for _, rows := range pageRows {
		assert.LessOrEqual(t, rows, 2)
		total += rows
	}
	assert.Equal(t, 5, total, "source enumeration must not use embed_gen as its input filter")
}

func TestContextWorker_BackstopPreservesIncompleteReconciliationCursor(t *testing.T) {
	var pageRows []int
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ReconcileBatchSize = 10
		deps.Hooks.AfterSourcePage = func(rows int) error {
			pageRows = append(pageRows, rows)
			return nil
		}
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "resume-source", "Resume")
	require.NoError(t, err)
	ids := make([]int64, 0, 3)
	for i := range 3 {
		ids = append(ids, f.seed("email", conversationID,
			time.Date(2026, 8, 8, 9, i, 0, 0, time.UTC), fmt.Sprintf("mail %d", i)))
	}
	_, err = f.run()
	require.NoError(t, err)
	pageRows = nil
	require.NoError(t, f.backend.SetDocumentReconcileCursor(t.Context(), f.gen,
		"source:"+strconv.FormatInt(ids[0], 10)))

	result, err := f.worker.RunBackstop(t.Context(), f.gen, testEmbeddingPassScope())

	require.NoError(t, err)
	assert.True(t, result.Contextual.Converged)
	assert.Equal(t, []int{2}, pageRows,
		"backstop must resume after the durable cursor instead of restarting at row zero")
}

func TestContextWorker_PrunesJournalThroughMinimumLiveContextCursor(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	conversationID, err := f.store.EnsureConversation(f.sourceID, "retention-source", "Retention")
	require.NoError(t, err)
	messageID := f.seed("email", conversationID,
		time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), "initial body")
	_, err = f.run()
	require.NoError(t, err)
	require.NoError(t, f.backend.ActivateGeneration(t.Context(), f.gen, true))

	floor := latestContextSequence(t, f.store)
	building, err := f.backend.CreateGeneration(t.Context(), "synthetic-next", 4, "synthetic-next:4")
	require.NoError(t, err)
	require.NoError(t, f.backend.AdvanceDocumentChangeWatermark(t.Context(), building, floor))
	require.NoError(t, f.store.UpsertMessageBody(messageID,
		sql.NullString{String: "updated body", Valid: true}, sql.NullString{}))
	latest := latestContextSequence(t, f.store)

	_, err = f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	changes, err := f.store.ScanEmbeddingChanges(t.Context(), 0, 100)
	require.NoError(t, err)
	require.NotEmpty(t, changes, "the slower building generation still needs the retained suffix")
	assert.Greater(t, changes[0].Sequence, floor)

	require.NoError(t, f.backend.AdvanceDocumentChangeWatermark(t.Context(), building, latest))
	_, err = f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	changes, err = f.store.ScanEmbeddingChanges(t.Context(), 0, 100)
	require.NoError(t, err)
	assert.Empty(t, changes, "all live contextual generations consumed the journal prefix")
}

func TestContextWorker_ReconcileSourceResumesInsideBudgetLimitedPage(t *testing.T) {
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ReconcileBatchSize = 64
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "budget-source", "Budget source")
	require.NoError(t, err)
	for i := range 3 {
		f.seed("email", conversationID, time.Date(2026, 8, 8, 9, i, 0, 0, time.UTC), fmt.Sprintf("mail %d", i))
	}
	_, err = f.run()
	require.NoError(t, err)
	f.worker.deps.MaxRunUTF8Bytes = 1

	result, err := f.worker.RunBackstop(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	for run := 0; run < 8 && !result.Contextual.Converged; run++ {
		result, err = f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
		require.NoError(t, err)
	}
	assert.True(t, result.Contextual.Converged, "the source cursor must resume after the processed prefix")
}

func TestContextWorker_ReconcileOrphansResumeInsideBudgetLimitedPage(t *testing.T) {
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ReconcileBatchSize = 64
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "budget-orphan", "Budget orphan")
	require.NoError(t, err)
	f.seed("email", conversationID, time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), "live")
	_, err = f.run()
	require.NoError(t, err)
	sequence := f.progress().ChangeSequence
	for i := range 3 {
		messageID := int64(900000 + i)
		key := fmt.Sprintf("message:%d", messageID)
		require.NoError(t, f.backend.PublishScope(t.Context(), f.gen, key, sequence,
			[]vector.DocumentPublication{{Key: key, Kind: "ordinary-message", Revision: "orphan",
				SourceSequence: sequence, Members: []int64{messageID}}},
			[]vector.Chunk{{MessageID: messageID, Vector: []float32{1, 2, 3, 4}}}))
	}
	f.worker.deps.MaxRunUTF8Bytes = 1

	result, err := f.worker.RunBackstop(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	for run := 0; run < 10 && !result.Contextual.Converged; run++ {
		result, err = f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
		require.NoError(t, err)
	}
	assert.True(t, result.Contextual.Converged, "the orphan cursor must resume after the processed prefix")
	for i := range 3 {
		record, readErr := f.backend.GetDocument(t.Context(), f.gen, fmt.Sprintf("message:%d", 900000+i))
		require.NoError(t, readErr)
		assert.Equal(t, vector.DocumentTombstoned, record.State)
	}
}

func TestContextWorker_ReconcileOrphansDoesNotSkipPulledForwardScopeWhenOrdersDiffer(t *testing.T) {
	f := newContextWorkerFixture(t, func(deps *ContextWorkerDeps) {
		deps.ReconcileBatchSize = 2
	})
	conversationID, err := f.store.EnsureConversation(f.sourceID, "ordered-orphans", "Ordered orphans")
	require.NoError(t, err)
	f.seed("email", conversationID, time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC), "live")
	_, err = f.run()
	require.NoError(t, err)
	sequence := f.progress().ChangeSequence

	orphans := []struct {
		documentKey string
		scopeKey    string
		messageID   int64
	}{
		{documentKey: "a-document", scopeKey: "message:900003", messageID: 900003},
		{documentKey: "b-document", scopeKey: "message:900001", messageID: 900001},
		{documentKey: "c-document", scopeKey: "message:900000", messageID: 900000},
	}
	for _, orphan := range orphans {
		require.NoError(t, f.backend.PublishScope(t.Context(), f.gen, orphan.scopeKey, sequence,
			[]vector.DocumentPublication{{Key: orphan.documentKey, Kind: "ordinary-message", Revision: "orphan",
				SourceSequence: sequence, Members: []int64{orphan.messageID}}},
			[]vector.Chunk{{MessageID: orphan.messageID, Vector: []float32{1, 2, 3, 4}}}))
	}
	f.worker.deps.MaxRunUTF8Bytes = 1

	result, err := f.worker.RunBackstop(t.Context(), f.gen, testEmbeddingPassScope())
	require.NoError(t, err)
	for run := 0; run < 10 && !result.Contextual.Converged; run++ {
		result, err = f.worker.RunOnce(t.Context(), f.gen, testEmbeddingPassScope())
		require.NoError(t, err)
	}
	require.True(t, result.Contextual.Converged)
	for _, orphan := range orphans {
		record, readErr := f.backend.GetDocument(t.Context(), f.gen, orphan.documentKey)
		require.NoError(t, readErr)
		assert.Equal(t, vector.DocumentTombstoned, record.State, orphan.documentKey)
	}
}

func TestContextWorker_ReconciliationRepairsLedgerWhileEmbedGenIsCurrent(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*testing.T, *contextWorkerFixture, string)
	}{
		{
			name: "changed revision",
			mutate: func(t *testing.T, f *contextWorkerFixture, key string) {
				t.Helper()
				_, err := f.backend.DB().Exec(`UPDATE embedding_documents SET published_revision = 'tampered' WHERE generation_id = ? AND document_key = ?`, int64(f.gen), key)
				require.NoError(t, err)
			},
		},
		{
			name: "deleted record",
			mutate: func(t *testing.T, f *contextWorkerFixture, key string) {
				t.Helper()
				_, err := f.backend.DB().Exec(`DELETE FROM embedding_documents WHERE generation_id = ? AND document_key = ?`, int64(f.gen), key)
				require.NoError(t, err)
			},
		},
		{
			name: "tombstoned record",
			mutate: func(t *testing.T, f *contextWorkerFixture, key string) {
				t.Helper()
				_, err := f.backend.DB().Exec(`UPDATE embedding_documents SET state = 'tombstoned' WHERE generation_id = ? AND document_key = ?`, int64(f.gen), key)
				require.NoError(t, err)
			},
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			f := newContextWorkerFixture(t, nil)
			conversationID, err := f.store.EnsureConversation(f.sourceID, "repair-ledger", "Repair")
			require.NoError(t, err)
			id := f.seed("email", conversationID, time.Now().UTC(), "repair me")
			_, err = f.run()
			require.NoError(t, err)
			key := fmt.Sprintf("message:%d", id)
			mutation.mutate(t, f, key)
			assert.Zero(t, f.missing(), "embed_gen stays current before reconciliation")

			result, err := f.worker.RunBackstop(context.Background(), f.gen, testEmbeddingPassScope())
			require.NoError(t, err)
			assert.True(t, result.Contextual.Converged)
			record, err := f.backend.GetDocument(context.Background(), f.gen, key)
			require.NoError(t, err)
			assert.Equal(t, vector.DocumentCurrent, record.State)
			assert.NotEqual(t, "tampered", record.PublishedRevision)
		})
	}
}

func TestContextWorker_RetiredGenerationIsBenignStop(t *testing.T) {
	f := newContextWorkerFixture(t, nil)
	f.seed("email", f.chatID, time.Now().UTC(), "mail")
	require.NoError(t, f.backend.RetireGeneration(context.Background(), f.gen, false))
	result, err := f.run()
	require.NoError(t, err)
	assert.False(t, result.Contextual.Converged)
}

func TestContextWorker_ClosesSourceSnapshotBeforeSemanticCall(t *testing.T) {
	var observed SourceSnapshot
	f := newContextWorkerFixture(t, func(d *ContextWorkerDeps) {
		d.Hooks.AfterSnapshot = func(snapshot SourceSnapshot) error {
			observed = snapshot
			return nil
		}
	})
	f.client.before = func() {
		_, _, err := observed.Message(context.Background(), 1)
		assert.ErrorIs(t, err, ErrSourceSnapshotClosed)
	}
	f.seed("email", f.chatID, time.Now().UTC(), "mail")
	_, err := f.run()
	require.NoError(t, err)
}
