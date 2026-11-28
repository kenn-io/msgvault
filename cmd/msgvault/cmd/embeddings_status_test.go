package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestEmbeddingStatusRendering(t *testing.T) {
	now := time.Now().UTC()
	eta, largeETA := float64(90), float64(3.6e10)
	for _, tc := range []struct {
		name    string
		status  vector.EmbeddingStatus
		want    []string
		columns map[string]string
	}{
		{
			name: "provider and writes",
			status: vector.EmbeddingStatus{Generation: vector.EmbeddingGenerationStatus{ID: 4, State: "building"}, Eligible: 10, Current: 3, Pending: 7,
				Job:         vector.EmbeddingJobState{State: "running", Phase: "retry_wait", PhaseSeconds: 30, HoldingSchedulerSlot: true},
				Diagnostics: &vector.EmbeddingDiagnostics{RecentBatches: []vector.EmbeddingBatch{{Sequence: 1, Attempted: 3, Completed: 3, ProviderMS: 2000, DBWriteMS: 15, Requests: 2, Retries: 1, RateLimits: 1}}}},
			want:    []string{"Current: 3/10", "ETA: unknown", "phase: retry_wait (30.0s)"},
			columns: map[string]string{"PROVIDER ms": "2000.0", "DB WRITE ms": "15.0"},
		},
		{
			name:   "stale scoped coverage",
			status: vector.EmbeddingStatus{SourceID: 7, SampledAt: now, CoverageSampledAt: now.Add(-time.Minute), DiagnosticsStale: true},
			want:   []string{"(60.0s ago)", "Coverage source: 7; diagnostics and throughput: all sources in this run", "Batch diagnostics: stale"},
		},
		{
			name: "failures without diagnostics", status: vector.EmbeddingStatus{Failed: 2},
			want: []string{"Latest message-embedding pass failures: 2 (all sources)"},
		},
		{name: "ETA", status: vector.EmbeddingStatus{ETASeconds: &eta}, want: []string{"ETA: 1m30s"}},
		{name: "large ETA", status: vector.EmbeddingStatus{ETASeconds: &largeETA}, want: []string{"ETA: more than 10000h00m00s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var out bytes.Buffer
			require.NoError(writeEmbeddingStatus(&out, tc.status, false))
			for _, want := range tc.want {
				assert.Contains(out.String(), want)
			}
			if len(tc.columns) > 0 {
				rows := strings.Split(strings.TrimSpace(out.String()), "\n")
				require.GreaterOrEqual(len(rows), 2)
				header, row := rows[len(rows)-2], rows[len(rows)-1]
				for name, want := range tc.columns {
					start := strings.Index(header, name)
					require.GreaterOrEqual(start, 0)
					require.GreaterOrEqual(len(row), start+len(name))
					assert.Equal(want, strings.TrimSpace(row[start:start+len(name)]), name)
				}
			}
		})
	}
}

func TestEmbeddingStatusValidatesFlags(t *testing.T) {
	flag := embeddingsStatusCmd.Flags().Lookup("source")
	require.NotNil(t, flag)
	oldWatch, oldInterval, oldSource, oldChanged := embeddingsStatusWatch, embeddingsStatusInterval, embeddingsStatusSource, flag.Changed
	t.Cleanup(func() {
		embeddingsStatusWatch, embeddingsStatusInterval, embeddingsStatusSource, flag.Changed = oldWatch, oldInterval, oldSource, oldChanged
	})
	for _, tc := range []struct {
		name, want, source string
		watch              bool
		interval           time.Duration
	}{
		{name: "short watch interval", watch: true, interval: 500 * time.Millisecond, want: "--interval must be at least 1s"},
		{name: "explicit zero source", source: "0", interval: 5 * time.Second, want: "--source must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			embeddingsStatusWatch, embeddingsStatusInterval, embeddingsStatusSource, flag.Changed = tc.watch, tc.interval, 0, false
			if tc.source != "" {
				require.NoError(t, embeddingsStatusCmd.Flags().Set("source", tc.source))
			}
			require.ErrorContains(t, embeddingsStatusCmd.RunE(embeddingsStatusCmd, nil), tc.want)
		})
	}
}

// scriptedEmbeddingStatus fails its first status read, as a restarting
// daemon would, then cancels the watch on success.
type scriptedEmbeddingStatus struct {
	sourceID int64
	calls    int
	cancel   context.CancelFunc
}

func (s *scriptedEmbeddingStatus) EmbeddingStatus(_ context.Context, sourceID int64) (*vector.EmbeddingStatus, error) {
	s.sourceID = sourceID
	s.calls++
	if s.calls == 1 {
		return nil, errors.New("daemon returned 503")
	}
	s.cancel()
	return &vector.EmbeddingStatus{Generation: vector.EmbeddingGenerationStatus{ID: 4, State: "building"}}, nil
}

// unreachableEmbeddingStatus is a daemon that went away, such as one that
// restarted on a new port. It ends the watch after ten polls so a missing
// reconnect fails the test instead of hanging it.
type unreachableEmbeddingStatus struct {
	calls  int
	cancel context.CancelFunc
}

func (u *unreachableEmbeddingStatus) EmbeddingStatus(context.Context, int64) (*vector.EmbeddingStatus, error) {
	u.calls++
	if u.calls >= 10 {
		u.cancel()
	}
	return nil, errors.New("connection refused")
}

func TestEmbeddingStatusPollLoop(t *testing.T) {
	oldWatch, oldInterval, oldSource := embeddingsStatusWatch, embeddingsStatusInterval, embeddingsStatusSource
	t.Cleanup(func() {
		embeddingsStatusWatch, embeddingsStatusInterval, embeddingsStatusSource = oldWatch, oldInterval, oldSource
	})
	embeddingsStatusSource = 7
	for _, tc := range []struct {
		name                           string
		watch, reconnect, incompatible bool
		calls                          int
	}{
		{name: "single read"},
		{name: "failed polls", watch: true},
		{name: "daemon moves", watch: true, reconnect: true, calls: 1},
		{name: "incompatible daemon", watch: true, reconnect: true, incompatible: true, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			embeddingsStatusWatch, embeddingsStatusInterval = tc.watch, time.Millisecond
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var stdout, stderr bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(ctx)
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			reader := &scriptedEmbeddingStatus{calls: tc.calls, cancel: cancel}
			var client embeddingStatusReader = reader
			var stale *unreachableEmbeddingStatus
			var reconnect func(context.Context) (embeddingStatusReader, error)
			reconnects := 0
			if tc.reconnect {
				stale = &unreachableEmbeddingStatus{cancel: cancel}
				client = stale
				reconnect = func(context.Context) (embeddingStatusReader, error) {
					reconnects++
					if tc.incompatible {
						return nil, fmt.Errorf("%w: the restarted daemon is incompatible", errEmbeddingStatusUnsupported)
					}
					switch reconnects {
					case 1:
						return nil, errors.New("local daemon is not running")
					case 2:
						return stale, nil
					default:
						return reader, nil
					}
				}
			}
			err := pollEmbeddingStatus(cmd, client, reconnect)
			switch {
			case !tc.watch:
				require.ErrorContains(err, "daemon returned 503")
				assert.Equal(1, reader.calls)
				assert.Equal(int64(7), reader.sourceID)
			case tc.incompatible:
				require.ErrorIs(err, errEmbeddingStatusUnsupported)
			default:
				require.ErrorIs(err, context.Canceled, "the watch ends only when interrupted")
				assert.Contains(stdout.String(), "Generation #4: building")
				if tc.reconnect {
					assert.Equal(3, stale.calls, "the old connection is used until the daemon moves")
					assert.Equal(3, reconnects)
					assert.Contains(stderr.String(), "reconnect: local daemon is not running")
				} else {
					assert.Equal(2, reader.calls)
					assert.Contains(stderr.String(), "daemon returned 503")
				}
			}
		})
	}
}
