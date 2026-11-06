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
	calls        atomic.Int32
	started      chan context.Context
	release      chan struct{}
	failurePhase string
}

func (c *localSetupTestController) Inspect(context.Context) (LocalEmbeddingsStatus, error) {
	return LocalEmbeddingsStatus{Host: "archive.example.test", Phase: "idle", DownloadBytes: 290000000}, nil
}
func (c *localSetupTestController) Enable(ctx context.Context, report func(LocalEmbeddingsStatus)) error {
	c.calls.Add(1)
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
	for _, behavior := range []string{"request lifetime", "admission", "retry after failure", "shutdown join", "installation failure", "publication failure"} {
		t.Run(behavior, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				checks, must := assert.New(t), require.New(t)
				srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger()})
				defer func() { _ = srv.Shutdown(context.Background()) }()
				controller := &localSetupTestController{started: make(chan context.Context, 2), release: make(chan struct{})}
				if behavior == "installation failure" {
					controller.failurePhase = "downloading"
				}
				if behavior == "publication failure" {
					controller.failurePhase = "idle"
				}
				srv.SetLocalEmbeddings(controller)
				post := func(ctx context.Context) *httptest.ResponseRecorder {
					request := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings/local", nil).WithContext(ctx)
					response := httptest.NewRecorder()
					srv.Router().ServeHTTP(response, request)
					return response
				}
				status := func() LocalEmbeddingsStatus {
					response := httptest.NewRecorder()
					srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/local", nil))
					var value LocalEmbeddingsStatus
					must.NoError(json.Unmarshal(response.Body.Bytes(), &value))
					return value
				}
				ctx, cancel := context.WithCancel(context.Background())
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
				case "installation failure", "publication failure":
					close(controller.release)
					synctest.Wait()
					checks.Equal("build interrupted", status().Error)
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
	for _, host := range []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080", "archive.example.test:8080"} {
		t.Run(host, func(t *testing.T) {
			server := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger()})
			t.Cleanup(func() { require.NoError(t, server.Shutdown(context.Background())) })
			server.SetLocalEmbeddings(&localSetupTestController{})
			request := httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/local", nil)
			request.Host = host
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code)
			var status LocalEmbeddingsStatus
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
			if host == "archive.example.test:8080" {
				assert.Equal(t, "archive.example.test", status.Host)
			} else {
				assert.Empty(t, status.Host)
			}
		})
	}
}
