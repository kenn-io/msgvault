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
	return LocalEmbeddingsStatus{Phase: "idle", DownloadBytes: 290000000}, nil
}
func (c *localSetupTestController) Enable(ctx context.Context, _ bool, report func(LocalEmbeddingsStatus)) error {
	c.calls.Add(1)
	c.started <- ctx
	report(LocalEmbeddingsStatus{Phase: "downloading", DoneBytes: 5, TotalBytes: 10})
	select {
	case <-c.release:
		return errors.New("download interrupted")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestLocalEmbeddingsAcceptedJobSurvivesRequestAndJoinsShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger()})
		defer func() { _ = srv.Shutdown(context.Background()) }()
		controller := &localSetupTestController{started: make(chan context.Context, 2), release: make(chan struct{})}
		srv.SetLocalEmbeddings(controller)
		requestCtx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings/local", strings.NewReader(`{"use_builtin":false}`)).WithContext(requestCtx)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		require.Equal(http.StatusAccepted, response.Code)
		cancel()
		jobCtx := <-controller.started
		require.NoError(jobCtx.Err())
		duplicate := httptest.NewRecorder()
		duplicateRequest := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings/local", strings.NewReader(`{}`))
		duplicateRequest.Header.Set("Content-Type", "application/json")
		srv.Router().ServeHTTP(duplicate, duplicateRequest)
		require.Equal(http.StatusAccepted, duplicate.Code)
		assert.Equal(int32(1), controller.calls.Load())
		statusResponse := httptest.NewRecorder()
		srv.Router().ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/local", nil))
		var status LocalEmbeddingsStatus
		require.NoError(json.Unmarshal(statusResponse.Body.Bytes(), &status))
		assert.Equal(int64(5), status.DoneBytes)
		close(controller.release)
		synctest.Wait()
		statusResponse = httptest.NewRecorder()
		srv.Router().ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/local", nil))
		require.NoError(json.Unmarshal(statusResponse.Body.Bytes(), &status))
		assert.Equal("download interrupted", status.Error)
		controller.release = make(chan struct{})
		retry := httptest.NewRecorder()
		retryRequest := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings/local", strings.NewReader(`{}`))
		retryRequest.Header.Set("Content-Type", "application/json")
		srv.Router().ServeHTTP(retry, retryRequest)
		require.Equal(http.StatusAccepted, retry.Code)
		secondCtx := <-controller.started
		require.NoError(srv.Shutdown(t.Context()))
		require.ErrorIs(secondCtx.Err(), context.Canceled)
		assert.Equal(int32(2), controller.calls.Load())
	})
}
