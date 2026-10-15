package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestEmbeddingStatusReturnsUnavailable(t *testing.T) {
	for _, tt := range []struct {
		name  string
		state VectorStatus
		err   error
	}{
		{name: "disabled", state: VectorStatusDisabled},
		{name: "error", state: VectorStatusError},
		{name: "read failure", state: VectorStatusReady, err: errors.New("synthetic archive read failure")},
		{name: "timeout", state: VectorStatusReady, err: fmt.Errorf("count coverage: %w", context.DeadlineExceeded)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv, _ := newTestServerWithMockStore(t)
			srv.vectorStatus = tt.state
			var logs bytes.Buffer
			srv.logger = slog.New(slog.NewTextHandler(&logs, nil))
			if tt.state == VectorStatusReady {
				srv.SetEmbeddingStatus(func(context.Context, int64) (vector.EmbeddingStatus, error) {
					return vector.EmbeddingStatus{}, tt.err
				}, 10)
			}
			response := doRequest(t, srv.Router(), http.MethodGet, "/api/v1/embeddings/status", nil, nil)
			require.Equal(http.StatusServiceUnavailable, response.Code)
			var body struct {
				Error   string `json:"error"`
				Message string `json:"message"`
			}
			require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal("embeddings_status_unavailable", body.Error)
			assert.NotContains(body.Message, "synthetic archive read failure")
			if tt.state == VectorStatusDisabled {
				assert.Contains(body.Message, "vector search is disabled")
				assert.Contains(body.Message, "[vector] enabled = true")
			}
			if errors.Is(tt.err, context.DeadlineExceeded) {
				assert.Contains(body.Message, "took too long to read; retry")
				assert.NotContains(body.Message, "initialize or upgrade")
			}
			if tt.state == VectorStatusReady {
				assert.Contains(logs.String(), tt.err.Error())
			}
		})
	}
}

func TestEmbeddingStatusRejectsInvalidSource(t *testing.T) {
	for _, tt := range []struct {
		name    string
		path    string
		readErr error
	}{
		{
			name: "unknown source is invalid", path: "/api/v1/embeddings/status?source_id=7",
			readErr: vector.ErrEmbeddingStatusUnknownSource,
		},
		{
			name: "out of scope source is invalid", path: "/api/v1/embeddings/status?source_id=7",
			readErr: vector.ErrEmbeddingStatusSourceOutOfScope,
		},
		{
			name: "conflicting source IDs are invalid", path: "/api/v1/embeddings/status?source_id=7&source_id=0",
		},
		{
			name: "malformed escape is invalid",
			path: "/api/v1/embeddings/status?source_id=7%",
		},
		{
			name: "zero source ID is invalid",
			path: "/api/v1/embeddings/status?source_id=0",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newTestServerWithMockStore(t)
			srv.SetEmbeddingStatus(func(context.Context, int64) (vector.EmbeddingStatus, error) {
				return vector.EmbeddingStatus{}, tt.readErr
			}, 10)
			response := doRequest(t, srv.Router(), http.MethodGet, tt.path, nil, nil)
			require.Equal(t, http.StatusBadRequest, response.Code)
			assert.Contains(t, response.Body.String(), `"error":"invalid_source_id"`)
			if tt.readErr != nil {
				assert.Contains(t, response.Body.String(), tt.readErr.Error())
			}
		})
	}
}

func TestEmbeddingStatusQueryAndJobState(t *testing.T) {
	tests := []struct {
		name     string
		holder   string
		path     string
		setup    func(*Server, time.Time)
		snapshot func(t *testing.T, ctx context.Context, sourceID int64, now time.Time) vector.EmbeddingStatus
		check    func(t *testing.T, body string, status vector.EmbeddingStatus)
	}{
		{
			name: "forwards source and deadline",
			path: "/api/v1/embeddings/status?source_id=7",
			snapshot: func(t *testing.T, ctx context.Context, sourceID int64, _ time.Time) vector.EmbeddingStatus {
				t.Helper()
				assert.Equal(t, int64(7), sourceID)
				_, hasDeadline := ctx.Deadline()
				assert.True(t, hasDeadline)
				return vector.EmbeddingStatus{Job: vector.EmbeddingJobState{State: "idle"}}
			},
		},
		{
			name: "running snapshot without gate holder is idle",
			path: "/api/v1/embeddings/status",
			snapshot: func(_ *testing.T, _ context.Context, _ int64, now time.Time) vector.EmbeddingStatus {
				return vector.EmbeddingStatus{Job: vector.EmbeddingJobState{State: "running", StartedAt: &now}, Diagnostics: &vector.EmbeddingDiagnostics{StartedAt: now, CurrentBatch: &vector.EmbeddingBatch{Phase: "provider"}, RecentBatches: []vector.EmbeddingBatch{{StartedAt: now.Add(-time.Second), FinishedAt: now, Completed: 1}}}}
			},
			check: func(t *testing.T, _ string, status vector.EmbeddingStatus) {
				t.Helper()
				assert.Equal(t, "idle", status.Job.State, "orphaned manual run must not stay live")
			},
		},
		{
			name:   "manual build holder is running",
			holder: "msgvault embeddings build",
			path:   "/api/v1/embeddings/status",
			snapshot: func(_ *testing.T, _ context.Context, _ int64, now time.Time) vector.EmbeddingStatus {
				started := time.Now().UTC()
				return vector.EmbeddingStatus{ActiveRunID: 8, ActiveRunStartedAt: &started, LatestRunStartedAt: &started, Diagnostics: &vector.EmbeddingDiagnostics{
					RunID: 8, StartedAt: now, UpdatedAt: started, CurrentBatch: &vector.EmbeddingBatch{Phase: "retry_wait", PhaseStartedAt: now.Add(-30 * time.Second)},
				}}
			},
			check: func(t *testing.T, _ string, status vector.EmbeddingStatus) {
				t.Helper()
				assert.Equal(t, "running", status.Job.State)
				assert.Equal(t, "retry_wait", status.Job.Phase)
				assert.False(t, status.DiagnosticsStale)
				assert.GreaterOrEqual(t, status.Job.PhaseSeconds, float64(30))
			},
		},
		{
			name:   "finished pass in a running job keeps its failures",
			holder: "msgvault embeddings build",
			path:   "/api/v1/embeddings/status",
			// The message pass finished; person embedding keeps the job running.
			snapshot: func(_ *testing.T, _ context.Context, _ int64, _ time.Time) vector.EmbeddingStatus {
				started := time.Now().UTC()
				finished := started.Add(time.Millisecond)
				return vector.EmbeddingStatus{LatestRunStartedAt: &started, Failed: 3, Diagnostics: &vector.EmbeddingDiagnostics{
					RunID: 9, StartedAt: started, FinishedAt: &finished,
				}}
			},
			check: func(t *testing.T, _ string, status vector.EmbeddingStatus) {
				t.Helper()
				assert.Equal(t, "running", status.Job.State)
				assert.Equal(t, int64(3), status.Failed)
			},
		},
		{
			name:   "run abandoned by a killed process is not live",
			holder: "msgvault embeddings build",
			path:   "/api/v1/embeddings/status",
			// A killed process left run 5 marked running with an unfinished snapshot.
			snapshot: func(_ *testing.T, _ context.Context, _ int64, now time.Time) vector.EmbeddingStatus {
				abandoned := now.Add(-time.Hour)
				return vector.EmbeddingStatus{Failed: 4, ActiveRunID: 5, ActiveRunStartedAt: &abandoned, LatestRunStartedAt: &abandoned, Pending: 10, Diagnostics: &vector.EmbeddingDiagnostics{
					RunID:         5,
					StartedAt:     abandoned.Add(-time.Second),
					UpdatedAt:     now,
					CurrentBatch:  &vector.EmbeddingBatch{Phase: "provider", PhaseStartedAt: abandoned},
					RecentBatches: []vector.EmbeddingBatch{{StartedAt: now.Add(-2 * time.Second), FinishedAt: now.Add(-time.Second), Completed: 5}},
				}}
			},
			check: func(t *testing.T, _ string, status vector.EmbeddingStatus) {
				t.Helper()
				assert.Equal(t, "running", status.Job.State)
				assert.Equal(t, "preparing_or_converging", status.Job.Phase)
				assert.Zero(t, status.Failed)
			},
		},
		{
			name:   "scheduled job snapshot stopped updating",
			holder: "scheduled embedding",
			path:   "/api/v1/embeddings/status",
			setup: func(srv *Server, now time.Time) {
				jobStarted := now.Add(-4 * time.Minute)
				srv.scheduler = &embeddingStatusScheduler{mockScheduler: newMockScheduler(), status: vector.EmbeddingStatus{
					Job: vector.EmbeddingJobState{State: "running", StartedAt: &jobStarted},
				}}
			},
			snapshot: func(_ *testing.T, _ context.Context, _ int64, now time.Time) vector.EmbeddingStatus {
				jobStarted := now.Add(-4 * time.Minute)
				runStarted := jobStarted.Add(time.Second)
				return vector.EmbeddingStatus{
					Generation: vector.EmbeddingGenerationStatus{ID: 3}, Pending: 100, Failed: 1,
					ActiveRunID: 12, ActiveRunStartedAt: &runStarted, LatestRunStartedAt: &runStarted,
					Diagnostics: &vector.EmbeddingDiagnostics{
						GenerationID: 3, RunID: 12, StartedAt: runStarted, UpdatedAt: now.Add(-2 * time.Minute),
						CurrentBatch:  &vector.EmbeddingBatch{Phase: "provider", PhaseStartedAt: now.Add(-2 * time.Minute)},
						RecentBatches: []vector.EmbeddingBatch{{StartedAt: now.Add(-3 * time.Minute), FinishedAt: now.Add(-2 * time.Minute), Completed: 50}},
					},
				}
			},
			check: func(t *testing.T, _ string, status vector.EmbeddingStatus) {
				t.Helper()
				assert.Equal(t, "running", status.Job.State)
				assert.True(t, status.Job.HoldingSchedulerSlot)
				require.NotNil(t, status.Diagnostics)
				assert.Equal(t, "diagnostics_stale", status.Job.Phase)
				assert.True(t, status.DiagnosticsStale)
				assert.Zero(t, status.Job.PhaseSeconds)
				assert.Nil(t, status.Diagnostics.CurrentBatch)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			srv, _ := newTestServerWithMockStore(t)
			gate := NewSerialOperationGate()
			srv.operationGate = gate
			now := time.Now().UTC()
			if tt.setup != nil {
				tt.setup(srv, now)
			}
			srv.SetEmbeddingStatus(func(ctx context.Context, sourceID int64) (vector.EmbeddingStatus, error) {
				return tt.snapshot(t, ctx, sourceID, now), nil
			}, 10)
			if tt.holder != "" {
				release, ok := gate.BeginLabeledWorkContext(t.Context(), tt.holder)
				require.True(ok)
				defer release()
			}
			response := doRequest(t, srv.Router(), http.MethodGet, tt.path, nil, nil)
			require.Equal(http.StatusOK, response.Code)
			var status vector.EmbeddingStatus
			require.NoError(json.Unmarshal(response.Body.Bytes(), &status))
			if tt.check != nil {
				tt.check(t, response.Body.String(), status)
			}
		})
	}
}

func TestEmbeddingSlotKind(t *testing.T) {
	tests := []struct {
		label string
		held  bool
		want  string
	}{
		{label: "", held: false, want: "none"},
		{label: "", held: true, want: "other"},
		{label: "scheduled embedding", held: true, want: "embeddings"},
		{label: "msgvault embeddings build", held: true, want: "embeddings"},
		{label: "msgvault build-embeddings", held: true, want: "embeddings"},
		{label: "scheduled sync of user@example.com", held: true, want: "sync"},
		{label: "post-sync multimodal indexing", held: true, want: "other"},
		{label: CardDAVJobNameForConnection("work"), held: true, want: "sync"},
		{label: "synctech-sms:+15555550100", held: true, want: "sync"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, embeddingSlotKind(tt.label, tt.held), "label %q held=%t", tt.label, tt.held)
	}
}

func TestStartedDuringJobUsesStoredPrecision(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 123_000_000, time.UTC)
	jobStarted := base.Add(400 * time.Microsecond)
	tests := []struct {
		name string
		run  time.Time
		want bool
	}{
		// SQLite's millisecond precision makes base and the job start 400µs later compare equal.
		{name: "same stored millisecond", run: base, want: true},
		{name: "earlier millisecond", run: base.Add(-time.Millisecond), want: false},
	}
	for _, tt := range tests {
		status := vector.EmbeddingStatus{Job: vector.EmbeddingJobState{StartedAt: &jobStarted}}
		assert.Equal(t, tt.want, startedDuringJob(&tt.run, status), tt.name)
	}
	assert.False(t, startedDuringJob(nil, vector.EmbeddingStatus{Job: vector.EmbeddingJobState{StartedAt: &jobStarted}}))
}

// embeddingStatusScheduler reports a scheduled embedding job beside the
// generic scheduler mock.
type embeddingStatusScheduler struct {
	*mockScheduler

	status vector.EmbeddingStatus
}

func (s *embeddingStatusScheduler) EmbeddingStatus() vector.EmbeddingStatus { return s.status }
