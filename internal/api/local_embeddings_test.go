package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

type localSetupTestController struct {
	calls   atomic.Int32
	started chan context.Context
	release chan struct{}
}

func (c *localSetupTestController) Inspect(context.Context) (LocalEmbeddingsStatus, error) {
	return LocalEmbeddingsStatus{Host: "system.example.test", Phase: "idle", DownloadBytes: 290000000}, nil
}
func (c *localSetupTestController) Enable(ctx context.Context, report func(LocalEmbeddingsStatus)) error {
	c.calls.Add(1)
	c.started <- ctx
	report(LocalEmbeddingsStatus{Phase: "downloading", DoneBytes: 5, TotalBytes: 10})
	select {
	case <-c.release:
		report(LocalEmbeddingsStatus{Phase: "indexing"})
		return errors.New("build interrupted")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestLocalEmbeddingsJobLifecycle(t *testing.T) {
	for _, behavior := range []string{"request lifetime", "admission", "retry after failure", "shutdown join"} {
		t.Run(behavior, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				checks, must := assert.New(t), require.New(t)
				srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger()})
				defer func() { _ = srv.Shutdown(t.Context()) }()
				controller := &localSetupTestController{started: make(chan context.Context, 2), release: make(chan struct{})}
				srv.SetLocalEmbeddings(controller)
				post := func(ctx context.Context) *httptest.ResponseRecorder {
					request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/embeddings/local", nil)
					response := httptest.NewRecorder()
					srv.Router().ServeHTTP(response, request)
					return response
				}
				status := func() LocalEmbeddingsStatus {
					response := httptest.NewRecorder()
					srv.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/embeddings/local", nil))
					var value LocalEmbeddingsStatus
					must.NoError(json.Unmarshal(response.Body.Bytes(), &value))
					return value
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				must.Equal(http.StatusAccepted, post(ctx).Code)
				jobCtx := <-controller.started
				synctest.Wait()
				switch behavior {
				case "request lifetime":
					cancel()
					must.NoError(jobCtx.Err())
					checks.Equal(int64(5), status().DoneBytes)
				case "admission":
					must.Equal(http.StatusAccepted, post(t.Context()).Code)
					checks.Equal(int32(1), controller.calls.Load())
					checks.True(status().Running)
				case "retry after failure":
					close(controller.release)
					synctest.Wait()
					checks.Equal("build interrupted", status().Error)
					controller.release = make(chan struct{})
					must.Equal(http.StatusAccepted, post(t.Context()).Code)
					<-controller.started
					synctest.Wait()
					checks.Empty(status().Error)
					checks.Equal(int32(2), controller.calls.Load())
				case "shutdown join":
					must.NoError(srv.Shutdown(t.Context()))
					must.ErrorIs(jobCtx.Err(), context.Canceled)
					checks.Equal(int32(1), controller.calls.Load())
				}
			})
		})
	}
}

func TestLocalEmbeddingsHostDisplay(t *testing.T) {
	for _, host := range []string{"localhost:8080", "archive.example.test:8080"} {
		t.Run(host, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			server := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger()})
			t.Cleanup(func() { require.NoError(server.Shutdown(context.WithoutCancel(t.Context()))) })
			server.SetLocalEmbeddings(&localSetupTestController{})
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/embeddings/local", nil)
			request.Host = host
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			require.Equal(http.StatusOK, response.Code)
			var status LocalEmbeddingsStatus
			require.NoError(json.Unmarshal(response.Body.Bytes(), &status))
			if host == "archive.example.test:8080" {
				assert.Equal("archive.example.test", status.Host)
			} else {
				assert.Empty(status.Host)
			}
		})
	}
}
