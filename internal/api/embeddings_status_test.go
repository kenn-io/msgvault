package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/circleback"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/plaud"
	"go.kenn.io/msgvault/internal/synctechsms"
	"go.kenn.io/msgvault/internal/twenty"
	"go.kenn.io/msgvault/internal/twilio"
	"go.kenn.io/msgvault/internal/vector"
)

func TestEmbeddingStatusReturnsUnavailableAfterVectorInitFailure(t *testing.T) {
	for _, state := range []VectorStatus{VectorStatusError, VectorStatusInitializing, VectorStatusReady} {
		t.Run(string(state), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv, _ := newTestServerWithMockStore(t)
			srv.vectorStatus = state
			var logs bytes.Buffer
			srv.logger = slog.New(slog.NewTextHandler(&logs, nil))
			if state == VectorStatusReady {
				srv.SetEmbeddingStatus(func(context.Context, int64) (vector.EmbeddingStatus, error) {
					return vector.EmbeddingStatus{}, errors.New("synthetic archive read failure")
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
			if state == VectorStatusReady {
				assert.Contains(logs.String(), "synthetic archive read failure")
			}
		})
	}
}

func TestEmbeddingStatusJobStateFromGateHolder(t *testing.T) {
	tests := []struct {
		name     string
		holder   string
		path     string
		code     int
		snapshot func(t *testing.T, ctx context.Context, sourceID int64, now time.Time) vector.EmbeddingStatus
		check    func(t *testing.T, body string, status vector.EmbeddingStatus)
	}{
		{
			name: "malformed escape is invalid",
			path: "/api/v1/embeddings/status?source_id=7%",
			code: http.StatusBadRequest,
		},
		{
			name: "malformed encoded key is invalid",
			path: "/api/v1/embeddings/status?source%_id=7",
			code: http.StatusBadRequest,
		},
		{
			name: "raw semicolon is invalid",
			path: "/api/v1/embeddings/status?source_id=7;x",
			code: http.StatusBadRequest,
		},
		{
			name: "valid source with malformed other pair is invalid",
			path: "/api/v1/embeddings/status?source_id=7&other=%",
			code: http.StatusBadRequest,
		},
		{
			name: "empty source ID is invalid",
			path: "/api/v1/embeddings/status?source_id=",
			code: http.StatusBadRequest,
			snapshot: func(*testing.T, context.Context, int64, time.Time) vector.EmbeddingStatus {
				return vector.EmbeddingStatus{}
			},
		},
		{
			name:   "sync holder bypasses gate and is sanitized",
			holder: "scheduled sync of private@example.test",
			path:   "/api/v1/embeddings/status?source_id=7",
			snapshot: func(t *testing.T, ctx context.Context, sourceID int64, _ time.Time) vector.EmbeddingStatus {
				t.Helper()
				assert.Equal(t, int64(7), sourceID)
				_, hasDeadline := ctx.Deadline()
				assert.True(t, hasDeadline)
				return vector.EmbeddingStatus{Job: vector.EmbeddingJobState{State: "idle"}}
			},
			check: func(t *testing.T, body string, status vector.EmbeddingStatus) {
				t.Helper()
				assert.True(t, status.Scheduler.SlotHeld)
				assert.Equal(t, "sync", status.Scheduler.SlotHolder)
				assert.NotContains(t, body, "private@example.test")
			},
		},
		{
			name: "running snapshot without gate holder is idle",
			path: "/api/v1/embeddings/status",
			snapshot: func(_ *testing.T, _ context.Context, _ int64, now time.Time) vector.EmbeddingStatus {
				return vector.EmbeddingStatus{Job: vector.EmbeddingJobState{State: "running", StartedAt: &now}, Diagnostics: &vector.EmbeddingDiagnostics{StartedAt: now, CurrentBatch: &vector.EmbeddingBatch{Phase: "provider"}, RecentBatches: []vector.EmbeddingBatch{{StartedAt: now.Add(-time.Second), FinishedAt: now, Completed: 1}}}}
			},
			check: func(t *testing.T, body string, status vector.EmbeddingStatus) {
				t.Helper()
				require.NotNil(t, status.Diagnostics)
				assert.Nil(t, status.Diagnostics.CurrentBatch)
				assert.Len(t, status.Diagnostics.RecentBatches, 1)
				assert.Nil(t, status.MessagesPerMinute)
				assert.Zero(t, status.WindowBatches)
				assert.Contains(t, body, `"state":"idle"`, "orphaned manual run must not stay live")
			},
		},
		{
			name:   "manual build holder is running",
			holder: "msgvault embeddings build",
			path:   "/api/v1/embeddings/status",
			snapshot: func(_ *testing.T, _ context.Context, _ int64, now time.Time) vector.EmbeddingStatus {
				started := time.Now().UTC()
				return vector.EmbeddingStatus{Failed: 2, ActiveRunID: 8, ActiveRunStartedAt: &started, LatestRunStartedAt: &started, Diagnostics: &vector.EmbeddingDiagnostics{
					RunID: 8, StartedAt: now, CurrentBatch: &vector.EmbeddingBatch{Phase: "write", PhaseStartedAt: now},
				}}
			},
			check: func(t *testing.T, body string, status vector.EmbeddingStatus) {
				t.Helper()
				assert.Equal(t, int64(2), status.Failed)
				assert.Contains(t, body, `"state":"running"`)
				assert.Equal(t, "write", status.Job.Phase)
				assert.NotContains(t, body, `"source_id"`, "an unfiltered response omits source_id")
			},
		},
		{
			name:   "legacy build-embeddings label is running",
			holder: "msgvault build-embeddings",
			path:   "/api/v1/embeddings/status",
			snapshot: func(*testing.T, context.Context, int64, time.Time) vector.EmbeddingStatus {
				return vector.EmbeddingStatus{Job: vector.EmbeddingJobState{State: "idle"}}
			},
			check: func(t *testing.T, _ string, status vector.EmbeddingStatus) {
				t.Helper()
				assert.Equal(t, "running", status.Job.State)
				assert.True(t, status.Job.HoldingSchedulerSlot)
				assert.Equal(t, "embeddings", status.Scheduler.SlotHolder)
			},
		},
		{
			name:   "snapshot from an earlier run is not live",
			holder: "msgvault embeddings resume",
			path:   "/api/v1/embeddings/status",
			// An interrupted pass left an unfinished snapshot; this resume has not started its run yet.
			snapshot: func(_ *testing.T, _ context.Context, _ int64, now time.Time) vector.EmbeddingStatus {
				previous := now.Add(-500 * time.Millisecond)
				return vector.EmbeddingStatus{LatestRunStartedAt: &previous, Failed: 2, Pending: 10, Diagnostics: &vector.EmbeddingDiagnostics{
					RunID:         7,
					StartedAt:     previous,
					CurrentBatch:  &vector.EmbeddingBatch{Phase: "provider", PhaseStartedAt: previous},
					RecentBatches: []vector.EmbeddingBatch{{StartedAt: previous, FinishedAt: previous.Add(100 * time.Millisecond), Completed: 5}},
				}}
			},
			check: func(t *testing.T, _ string, status vector.EmbeddingStatus) {
				t.Helper()
				assert.Equal(t, "running", status.Job.State)
				assert.Equal(t, "preparing_or_converging", status.Job.Phase)
				assert.Zero(t, status.Failed)
				assert.Nil(t, status.ETASeconds)
				assert.Nil(t, status.MessagesPerMinute)
				assert.Zero(t, status.WindowBatches)
				assert.Zero(t, status.WindowSeconds)
				require.NotNil(t, status.Diagnostics)
				assert.Nil(t, status.Diagnostics.CurrentBatch)
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
					StartedAt:     abandoned,
					CurrentBatch:  &vector.EmbeddingBatch{Phase: "provider", PhaseStartedAt: abandoned},
					RecentBatches: []vector.EmbeddingBatch{{StartedAt: abandoned, FinishedAt: abandoned.Add(time.Second), Completed: 5}},
				}}
			},
			check: func(t *testing.T, _ string, status vector.EmbeddingStatus) {
				t.Helper()
				assert.Equal(t, "running", status.Job.State)
				assert.Equal(t, "preparing_or_converging", status.Job.Phase)
				assert.Zero(t, status.Failed)
				assert.Nil(t, status.ETASeconds)
				require.NotNil(t, status.Diagnostics)
				assert.Nil(t, status.Diagnostics.CurrentBatch)
			},
		},
		{
			name:   "generic source sync holder is sync",
			holder: "synctech-sms:+15555550100",
			path:   "/api/v1/embeddings/status",
			snapshot: func(*testing.T, context.Context, int64, time.Time) vector.EmbeddingStatus {
				return vector.EmbeddingStatus{}
			},
			check: func(t *testing.T, body string, status vector.EmbeddingStatus) {
				t.Helper()
				assert.Equal(t, "sync", status.Scheduler.SlotHolder)
				assert.NotContains(t, body, "5555550100")
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
			srv.SetEmbeddingStatus(func(ctx context.Context, sourceID int64) (vector.EmbeddingStatus, error) {
				return tt.snapshot(t, ctx, sourceID, now), nil
			}, 10)
			if tt.holder != "" {
				release, ok := gate.BeginLabeledWorkContext(t.Context(), tt.holder)
				require.True(ok)
				defer release()
			}
			response := doRequest(t, srv.Router(), http.MethodGet, tt.path, nil, nil)
			if tt.code != 0 {
				require.Equal(tt.code, response.Code)
				assert.Contains(t, response.Body.String(), `"error":"invalid_source_id"`)
				return
			}
			require.Equal(http.StatusOK, response.Code)
			var status vector.EmbeddingStatus
			require.NoError(json.Unmarshal(response.Body.Bytes(), &status))
			tt.check(t, response.Body.String(), status)
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
		{label: "scheduled sync of user@example.test", held: true, want: "sync"},
		{label: "post-sync multimodal indexing", held: true, want: "other"},
		{label: CardDAVJobNameForConnection("work"), held: true, want: "sync"},
	}
	for _, sourceType := range []string{
		synctechsms.SourceType, gcal.SourceType, granola.SourceType, plaud.SourceType, circleback.SourceType,
		notionmeetings.SourceType, twilio.SourceType, twenty.SourceType, muesli.SourceType,
		sourceTypeBeeper, sourceTypeMatrix, sourceTypeSlack,
	} {
		name, ok := SchedulerJobNameForSource(sourceType, "user@example.test/calendar")
		require.True(t, ok, sourceType)
		tests = append(tests, struct {
			label string
			held  bool
			want  string
		}{label: name, held: true, want: "sync"})
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
		// SQLite keeps milliseconds, so a pass started 200µs after the job reads back as base.
		{name: "same stored millisecond", run: base, want: true},
		{name: "later", run: base.Add(time.Millisecond), want: true},
		{name: "earlier millisecond", run: base.Add(-time.Millisecond), want: false},
	}
	for _, tt := range tests {
		status := vector.EmbeddingStatus{Job: vector.EmbeddingJobState{StartedAt: &jobStarted}}
		assert.Equal(t, tt.want, startedDuringJob(&tt.run, status), tt.name)
	}
	assert.False(t, startedDuringJob(nil, vector.EmbeddingStatus{Job: vector.EmbeddingJobState{StartedAt: &jobStarted}}))
}
