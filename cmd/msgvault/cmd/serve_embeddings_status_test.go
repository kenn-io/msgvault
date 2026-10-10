package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/vector"
)

func TestDaemonSchedulerAdapterEmbeddingStatus(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s := scheduler.New(nil)
	require.NoError(s.SetEmbedJob(&scheduler.EmbedJob{}, "0 * * * *", true))
	server := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler), Scheduler: &schedulerAdapter{scheduler: s}})
	defer func() { require.NoError(server.Shutdown(context.Background())) }()
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/status", nil))
	require.Equal(http.StatusOK, response.Code)
	var status vector.EmbeddingStatus
	require.NoError(json.Unmarshal(response.Body.Bytes(), &status))
	assert.True(status.Scheduler.Registered)
	assert.Equal("0 * * * *", status.Scheduler.Schedule)
	assert.True(status.Scheduler.RunAfterSync)
}
