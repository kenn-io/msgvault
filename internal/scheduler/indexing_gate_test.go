//go:build sqlite_vec

package scheduler_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// slowEmbeddingClient takes 70 seconds per request.
type slowEmbeddingClient struct {
	mu        sync.Mutex
	completed map[string]int
}

func (c *slowEmbeddingClient) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	select {
	case <-time.After(70 * time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	c.mu.Lock()
	c.completed[strings.Join(inputs, "\x00")]++
	c.mu.Unlock()
	out := make([][]float32, len(inputs))
	for i, input := range inputs {
		out[i] = []float32{float32(len(input)%4 + 1), 0, 0, 0}
	}
	return out, nil
}

func newIndexingGateFixture(t *testing.T, email string, bodies []string) (*store.Store, *sqlitevec.Backend, vector.GenerationID) {
	t.Helper()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	st, err := store.OpenForTest(mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", email)
	require.NoError(t, err)
	conversation, err := st.EnsureConversation(source.ID, "thread", "Thread")
	require.NoError(t, err)
	for i, body := range bodies {
		id, err := st.UpsertMessage(&store.Message{
			ConversationID: conversation, SourceID: source.ID,
			SourceMessageID: fmt.Sprintf("indexed-%d", i), MessageType: "email",
		})
		require.NoError(t, err)
		require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	}
	require.NoError(t, sqlitevec.RegisterExtension())
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{
		Path: filepath.Join(dir, "vectors.db"), MainPath: mainPath, MainDB: st.DB(), Dimension: 4,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	gen, err := backend.CreateGeneration(t.Context(), "synthetic", 4, "synthetic:4")
	require.NoError(t, err)
	return st, backend, gen
}

func TestPostSyncIndexingYieldsToWaitingSyncsAndResumes(t *testing.T) {
	bodies := make([]string, 4)
	for i := range bodies {
		bodies[i] = strings.Repeat(fmt.Sprintf("body %d ", i), 15)
	}
	for _, tc := range []struct {
		name      string
		bodies    []string
		newWorker func(*store.Store, *sqlitevec.Backend, *slowEmbeddingClient) scheduler.EmbedRunner
		requests  int
		verify    func(*testing.T, *store.Store, *sqlitevec.Backend, vector.GenerationID)
	}{
		{name: "messages", bodies: bodies, requests: 6,
			newWorker: func(st *store.Store, backend *sqlitevec.Backend, client *slowEmbeddingClient) scheduler.EmbedRunner {
				// Each message spans three requests in a two-message batch.
				return embed.NewWorker(embed.WorkerDeps{Backend: backend, VectorsDB: backend.DB(), MainDB: st.DB(), Store: st,
					Client: client, BatchSize: 2, MaxInputChars: 50, Recorder: st})
			},
			verify: func(t *testing.T, st *store.Store, _ *sqlitevec.Backend, gen vector.GenerationID) {
				t.Helper()
				missing, err := st.ScanForEmbedding(t.Context(), int64(gen), 0, 10)
				require.NoError(t, err)
				assert.Empty(t, missing, "the continuation publishes every message")
			}},
		{name: "people", requests: 2,
			newWorker: func(st *store.Store, backend *sqlitevec.Backend, client *slowEmbeddingClient) scheduler.EmbedRunner {
				people := embed.NewPersonWorker(embed.PersonWorkerDeps{
					Store: scheduledPersonSource{
						{PersonID: 1, Revision: "one", Text: "Synthetic One", RendererPolicy: store.PersonSemanticRendererPolicy},
						{PersonID: 2, Revision: "two", Text: "Synthetic Two", RendererPolicy: store.PersonSemanticRendererPolicy},
					}, Backend: backend, Client: &slowPersonClient{client}, BatchSize: 1, Recorder: st,
					Gate: vector.SemanticPersonEmbeddingGateFunc(func(context.Context) error { return nil }),
				})
				messages := embed.NewWorker(embed.WorkerDeps{Backend: backend, VectorsDB: backend.DB(), MainDB: st.DB(), Store: st, Client: client, Recorder: st})
				return embed.NewGenerationWorker(messages, people)
			},
			verify: func(t *testing.T, _ *store.Store, backend *sqlitevec.Backend, gen vector.GenerationID) {
				t.Helper()
				revisions, err := backend.ListPersonRevisions(t.Context(), gen)
				require.NoError(t, err)
				assert.Equal(t, map[int64]string{1: "one", 2: "two"}, revisions, "the continuation finishes people after the message scan completed")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			st, backend, gen := newIndexingGateFixture(t, "mail-a@example.com", tc.bodies)
			client := &slowEmbeddingClient{completed: make(map[string]int)}
			newWorker := func() scheduler.EmbedRunner { return tc.newWorker(st, backend, client) }
			embedJob := &scheduler.EmbedJob{
				Worker: newWorker(), Backend: backend, Store: st,
				Fingerprint: "synthetic:4", BackstopInterval: -1,
			}

			synctest.Test(t, func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)
				var mu sync.Mutex
				var events []string
				record := func(event string) {
					mu.Lock()
					defer mu.Unlock()
					events = append(events, fmt.Sprintf("%s at %s", event, time.Duration(time.Now().Unix()-946684800)*time.Second))
				}
				var restarted sync.Once
				s := scheduler.New(func(_ context.Context, email string) error {
					if email == "mail-b@example.com" {
						// The gate has released the previous pass. Rebuild its worker
						// before the follow-up to require durable request reuse.
						restarted.Do(func() { embedJob.Worker = newWorker() })
					}
					record("sync " + email)
					return nil
				}).WithWorkTracker(api.NewSerialOperationGate())
				defer func() { <-s.Stop().Done() }()
				requirements.NoError(s.SetEmbedJob(embedJob, "", true))
				requirements.NoError(s.AddAccount("mail-a@example.com", "0 0 1 1 *"))
				requirements.NoError(s.AddAccount("mail-b@example.com", "0 0 1 1 *"))
				requirements.NoError(s.AddJob(scheduler.Job{Name: "carddav:work", Schedule: "0 0 1 1 *", Run: func(context.Context) error {
					record("carddav")
					return nil
				}}))

				requirements.NoError(s.TriggerSync("mail-a@example.com", false))
				time.Sleep(time.Second)
				requirements.NoError(s.TriggerSync("mail-b@example.com", false))
				_, err := s.StartJob("carddav:work", false)
				requirements.NoError(err)

				go func() {
					for range 14 {
						time.Sleep(time.Minute)
						_ = s.TriggerSync("mail-b@example.com", false)
					}
				}()
				time.Sleep(30 * time.Minute)
				synctest.Wait()
				requirements.GreaterOrEqual(len(events), 3)
				assertions.Equal("sync mail-a@example.com at 0s", events[0])
				assertions.ElementsMatch([]string{"sync mail-b@example.com at 1m10s", "carddav at 1m10s"}, events[1:3],
					"waiting syncs run after the pass yields, not after all indexing")
				assertions.Equal("idle", s.EmbeddingStatus().Job.State)
			})

			requirements.Len(client.completed, tc.requests, "all unique provider requests eventually finish after repeated sync yields")
			for request, completions := range client.completed {
				assert.Equal(t, 1, completions, "provider request %q must complete exactly once", request)
			}
			tc.verify(t, st, backend, gen)
		})
	}
}

type scheduledPersonSource []store.PersonSemanticDocument

func (s scheduledPersonSource) ListPersonSemanticDocumentsContext(context.Context) ([]store.PersonSemanticDocument, error) {
	return s, nil
}

func (s scheduledPersonSource) LoadPersonSemanticDocumentContext(_ context.Context, id int64) (*store.PersonSemanticDocument, error) {
	for _, doc := range s {
		if doc.PersonID == id {
			return &doc, nil
		}
	}
	return nil, store.ErrPersonNotFound
}

type slowPersonClient struct{ *slowEmbeddingClient }

func (c *slowPersonClient) EmbedQuery(context.Context, string) ([]float32, error) {
	panic("unexpected query")
}
func (c *slowPersonClient) EmbedDocuments(ctx context.Context, docs []vector.DocumentInput) ([][][]float32, error) {
	inputs := make([]string, len(docs))
	for i, doc := range docs {
		inputs[i] = doc.Chunks[0]
	}
	vectors, err := c.Embed(ctx, inputs)
	if err != nil {
		return nil, err
	}
	out := make([][][]float32, len(vectors))
	for i, v := range vectors {
		out[i] = [][]float32{v}
	}
	return out, nil
}
