package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/vector"
)

type localSetupTestController struct {
	calls        atomic.Int32
	builtin      atomic.Bool
	started      chan context.Context
	release      chan struct{}
	failurePhase string
}

func (c *localSetupTestController) Inspect(context.Context) (LocalEmbeddingsStatus, error) {
	return LocalEmbeddingsStatus{Phase: "idle", DownloadBytes: 290000000}, nil
}
func (c *localSetupTestController) Enable(ctx context.Context, builtin bool, report func(LocalEmbeddingsStatus)) error {
	c.calls.Add(1)
	c.builtin.Store(builtin)
	c.started <- ctx
	if c.failurePhase != "idle" {
		report(LocalEmbeddingsStatus{Phase: "downloading", DoneBytes: 5, TotalBytes: 10})
	}
	select {
	case <-c.release:
		if c.failurePhase != "downloading" {
			report(LocalEmbeddingsStatus{Phase: "indexing"})
		}
		if c.failurePhase == "idle" {
			report(LocalEmbeddingsStatus{Phase: "idle"})
		}
		return errors.New("build interrupted")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestLocalEmbeddingsJobLifecycle(t *testing.T) {
	for _, behavior := range []string{"request lifetime", "admission", "later successful build", "retry after failure", "shutdown join", "installation failure", "publication failure", "built-in retry choice"} {
		t.Run(behavior, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				history := &operationHistoryStub{status: make(map[operations.Kind]operations.LaneHistoryStatus)}
				srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, OperationHistoryReader: history, Logger: testLogger()})
				defer func() { _ = srv.Shutdown(context.Background()) }()
				controller := &localSetupTestController{started: make(chan context.Context, 2), release: make(chan struct{})}
				if behavior == "request lifetime" {
					controller.builtin.Store(true)
				}
				if behavior == "installation failure" || behavior == "built-in retry choice" {
					controller.failurePhase = "downloading"
				}
				if behavior == "publication failure" {
					// An idle failure must not mark the indexing phase as failed.
					controller.failurePhase = "idle"
				}
				srv.SetLocalEmbeddings(controller)
				post := func(ctx context.Context, body string) *httptest.ResponseRecorder {
					t.Helper()
					request := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings/local", strings.NewReader(body)).WithContext(ctx)
					request.Header.Set("Content-Type", "application/json")
					response := httptest.NewRecorder()
					srv.Router().ServeHTTP(response, request)
					return response
				}
				status := func() LocalEmbeddingsStatus {
					t.Helper()
					response := httptest.NewRecorder()
					srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/local", nil))
					var value LocalEmbeddingsStatus
					require.NoError(json.Unmarshal(response.Body.Bytes(), &value))
					return value
				}
				recordSuccessfulBuild := func() {
					run := operationRunForKindFixture(t, operations.KindMessageEmbedding, operations.StateSucceeded)
					run.StartedAt = time.Now()
					time.Sleep(time.Second)
					finished := time.Now()
					run.FinishedAt = &finished
					history.status[operations.KindMessageEmbedding] = operations.LaneHistoryStatus{Kind: operations.KindMessageEmbedding, Lane: operations.LaneMessages, HistoryAvailability: operations.HistoryAvailable, Latest: &run}
					srv.SetEmbeddingStatus(func(context.Context, int64) (vector.EmbeddingStatus, error) {
						return vector.EmbeddingStatus{
							Generation: vector.EmbeddingGenerationStatus{ID: 1}, ActiveGeneration: &vector.EmbeddingGenerationStatus{ID: 1}, Current: 1,
						}, nil
					}, 10)
				}

				requestCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				body := `{"use_builtin":false}`
				if behavior == "built-in retry choice" {
					body = `{"use_builtin":true}`
				}
				require.Equal(http.StatusAccepted, post(requestCtx, body).Code)
				jobCtx := <-controller.started
				synctest.Wait()
				switch behavior {
				case "request lifetime":
					cancel()
					require.NoError(jobCtx.Err())
					assert.False(controller.builtin.Load())
					assert.Equal(int64(5), status().DoneBytes)
				case "admission":
					require.Equal(http.StatusAccepted, post(t.Context(), `{"use_builtin":false}`).Code)
					assert.Equal(int32(1), controller.calls.Load())
					conflict := post(t.Context(), `{"use_builtin":true}`)
					require.Equal(http.StatusConflict, conflict.Code)
					assert.Contains(conflict.Body.String(), "different provider choice")
					assert.Equal(int32(1), controller.calls.Load())
				case "later successful build":
					close(controller.release)
					synctest.Wait()
					assert.Equal("build interrupted", status().Error)
					recordSuccessfulBuild()
					assert.Empty(status().Error, "a later successful build resolves the setup build error")
				case "retry after failure":
					close(controller.release)
					synctest.Wait()
					controller.release = make(chan struct{})
					require.Equal(http.StatusAccepted, post(t.Context(), `{"use_builtin":true}`).Code)
					<-controller.started
					assert.True(controller.builtin.Load())
					assert.Equal(int32(2), controller.calls.Load())
				case "installation failure", "publication failure", "built-in retry choice":
					close(controller.release)
					synctest.Wait()
					recordSuccessfulBuild()
					assert.Equal("build interrupted", status().Error)
					response := httptest.NewRecorder()
					srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/local", nil))
					var payload map[string]any
					require.NoError(json.Unmarshal(response.Body.Bytes(), &payload))
					assert.Equal(behavior == "built-in retry choice", payload["retry_use_builtin"] == true)

				case "shutdown join":
					require.NoError(srv.Shutdown(t.Context()))
					require.ErrorIs(jobCtx.Err(), context.Canceled)
					assert.Equal(int32(1), controller.calls.Load())
				}
			})
		})
	}
}

func TestLocalEmbeddingsRejectsMissingProviderChoice(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"use_builtin":null}`} {
		t.Run(body, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger()})
			t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
			controller := &localSetupTestController{started: make(chan context.Context, 1), release: make(chan struct{})}
			server.SetLocalEmbeddings(controller)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings/local", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			assert.Equal(http.StatusBadRequest, response.Code, response.Body.String())
			require.NoError(server.Shutdown(t.Context()))
			assert.Zero(controller.calls.Load(), "malformed requests must not start setup")
		})
	}
}
