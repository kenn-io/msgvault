package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/config"
)

func TestLaneReadinessWithoutOwningReaderIsExplicitlyUnavailable(t *testing.T) {
	srv := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Logger: testLogger()})
	response := doGet(srv, "/api/v1/lanes/readiness")
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Contains(t, response.Body.String(), "lane_readiness_unavailable")
}

func TestLaneReadinessSnapshotsOwningRuntimeAndSanitizesReaderFailure(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	var observed LaneRuntimeSnapshot
	fail := false
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Logger: testLogger(), VectorStatus: VectorStatusInitializing, LaneReadinessReader: func(_ context.Context, runtime LaneRuntimeSnapshot) (LaneReadinessResponse, error) {
		observed = runtime
		if fail {
			return LaneReadinessResponse{}, errors.New("synthetic-private-host/path and private-key")
		}
		return LaneReadinessResponse{Lanes: []LaneReadiness{}, PendingRestart: runtime.PendingRestart}, nil
	}})
	response := doGet(srv, "/api/v1/lanes/readiness")
	requirements.Equal(http.StatusOK, response.Code)
	assertions.Equal(VectorStatusInitializing, observed.VectorStatus)
	assertions.False(observed.Initialized["text_search"])
	assertions.False(observed.Initialized["visual_search"])
	assertions.False(observed.Initialized["people_inference"])
	assertions.True(observed.Initialized["media_policy"])
	srv.settingsPendingRestart.Store(true)
	response = doGet(srv, "/api/v1/lanes/readiness")
	requirements.Equal(http.StatusOK, response.Code)
	var report LaneReadinessResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &report))
	assertions.True(report.PendingRestart)
	fail = true
	response = doGet(srv, "/api/v1/lanes/readiness")
	assertions.Equal(http.StatusServiceUnavailable, response.Code)
	assertions.Contains(response.Body.String(), "lane_readiness_unavailable")
	assertions.NotContains(response.Body.String(), "synthetic-private")
}
