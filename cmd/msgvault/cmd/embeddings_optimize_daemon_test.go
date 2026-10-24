//go:build sqlite_vec

package cmd

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func TestOptimizeGateLeaseStopsDuringYield(t *testing.T) {
	for _, variant := range []struct{ drain, request bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		t.Run(strconv.FormatBool(variant.drain)+"/request="+strconv.FormatBool(variant.request), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate := api.NewSerialOperationGate()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				lease, err := acquireOptimizeGate(ctx, gate)
				require.NoError(t, err)
				defer lease.close()
				releaseOther := make(chan struct{})
				otherEntered := make(chan struct{})
				go func() {
					var done func()
					var ok bool
					if variant.request {
						done, ok = gate.BeginRequestWorkContext(t.Context(), "import")
					} else {
						done, ok = gate.BeginLabeledWorkContext(t.Context(), "import")
					}
					defer done()
					if ok {
						close(otherEntered)
						<-releaseOther
					}
				}()
				synctest.Wait()
				assert.Equal(t, variant.request, gate.HasRequestWaiters())
				yielded := make(chan error, 1)
				go func() { yielded <- lease.yield() }()
				synctest.Wait()
				select {
				case <-otherEntered:
				default:
					require.FailNow(t, "import must enter at the batch boundary")
				}
				assert.False(t, gate.HasRequestWaiters(), "optimize reacquisition remains background work")
				if variant.drain {
					gate.StartDrain()
				} else {
					cancel()
				}
				synctest.Wait()
				require.Len(t, yielded, 1)
				require.Error(t, <-yielded)
				lease.close()
				label, _, held := gate.Holder()
				assert.True(t, held, "failed reacquisition cannot release another operation's lease")
				assert.Equal(t, "import", label)
				close(releaseOther)
				synctest.Wait()
				_, _, held = gate.Holder()
				assert.False(t, held)
			})
		})
	}
}

func FuzzOptimizeGateInitialAdmissionRegistersRequestWaiter(f *testing.F) {
	for _, backgroundCount := range []uint8{0, 1, 4, 8, 255} {
		f.Add(backgroundCount)
	}
	f.Fuzz(func(t *testing.T, backgroundCount uint8) {
		count := min(int(backgroundCount), 8)
		synctest.Test(t, func(t *testing.T) {
			gate := api.NewSerialOperationGate()
			releaseScheduled, ok := gate.BeginLabeledWorkContext(t.Context(), "scheduled embedding pass")
			require.True(t, ok)
			defer releaseScheduled()

			backgroundDone := make(chan struct{}, count)
			for range count {
				go func() {
					release, admitted := gate.BeginLabeledWorkContext(t.Context(), "scheduled archive work")
					if admitted {
						release()
					}
					backgroundDone <- struct{}{}
				}()
			}

			type acquisition struct {
				lease *optimizeGateLease
				err   error
			}
			optimizeAcquired := make(chan acquisition, 1)
			go func() {
				lease, err := acquireOptimizeGate(t.Context(), gate)
				optimizeAcquired <- acquisition{lease: lease, err: err}
			}()

			synctest.Wait()
			require.True(t, gate.HasRequestWaiters(), "queued optimize must prompt scheduled work to yield")

			releaseScheduled()
			synctest.Wait()
			optimized := <-optimizeAcquired
			require.NoError(t, optimized.err)
			require.NotNil(t, optimized.lease)
			assert.Empty(t, backgroundDone, "scheduled work waits while optimize holds the gate")
			assert.False(t, gate.HasRequestWaiters(), "an admitted optimize is no longer a waiter")

			optimized.lease.close()
			synctest.Wait()
			assert.Len(t, backgroundDone, count)
		})
	})
}

func newDaemonOptimizeFixture(t *testing.T) (*config.Config, vector.GenerationID) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(dir, "vectors.db")
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.Search.ANNThreads = 1
	main := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	require.NoError(t, main.Close())
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, Dimension: 4})
	require.NoError(t, err)
	gen, err := backend.CreateGeneration(t.Context(), "model", 4, "model:4")
	require.NoError(t, err)
	chunks := make([]vector.Chunk, 512)
	for i := range chunks {
		chunks[i] = vector.Chunk{MessageID: int64(i + 1), Vector: []float32{float32(i % 17), 1, 2, 3}}
	}
	require.NoError(t, backend.Upsert(t.Context(), gen, chunks))
	require.NoError(t, backend.Close())
	return cfg, gen
}

func TestDaemonOptimizeYieldsAndRestartsChangedSource(t *testing.T) {
	cfg, gen := newDaemonOptimizeFixture(t)
	owner, err := tryAcquireWriteOwnerLock(cfg.Data.DataDir)
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close()) }()
	gate := api.NewSerialOperationGate()
	adapter := &storeAPIAdapter{config: cfg, operationGate: gate}
	originalWorker := runAcceleratorWorkerSubprocess
	defer func() { runAcceleratorWorkerSubprocess = originalWorker }()
	runAcceleratorWorkerSubprocess = func(ctx context.Context, _ string, path string, generation vector.GenerationID, threads int, _ io.Writer) error {
		label, _, held := gate.Holder()
		assert.True(t, held, "training remains exclusive")
		assert.Equal(t, "msgvault embeddings optimize", label)
		return sqlitevec.RunAcceleratorWorker(ctx, path, generation, threads)
	}
	queued := false
	imported := make(chan error, 1)
	var output strings.Builder
	err = adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "--verbose", "optimize", strconv.FormatInt(int64(gen), 10)}}, func(event api.CLIRunEvent) error {
		output.WriteString(event.Data)
		if strings.HasPrefix(event.Data, "Preparing accelerator:") && !queued {
			queued = true
			go func() {
				done, ok := gate.BeginRequestWorkContext(t.Context(), "import")
				defer done()
				if !ok {
					imported <- errors.New("import admission failed")
					return
				}
				backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, Dimension: 4})
				if err != nil {
					imported <- err
					return
				}
				defer func() { _ = backend.Close() }()
				imported <- backend.Upsert(t.Context(), gen, []vector.Chunk{{MessageID: 999, Vector: []float32{1, 2, 3, 4}}})
			}()
			require.Eventually(t, gate.HasRequestWaiters, time.Second, time.Millisecond)
		}
		return nil
	})
	require.NoError(t, err)
	require.True(t, queued)
	require.NoError(t, <-imported)
	assert.Contains(t, output.String(), "accelerator ready (513 vectors)")
	_, _, held := gate.Holder()
	assert.False(t, held)
	require.NoError(t, adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "optimize", "--drop", strconv.FormatInt(int64(gen), 10)}}, nil))
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, Dimension: 4})
	require.NoError(t, err)
	defer func() { require.NoError(t, backend.Close()) }()
	status, err := backend.Accelerator(t.Context(), gen)
	require.NoError(t, err)
	assert.Nil(t, status)
	stats, err := backend.Stats(t.Context(), gen)
	require.NoError(t, err)
	assert.Equal(t, int64(513), stats.EmbeddingCount)
}

func TestDaemonOptimizeCancellationKeepsGateUntilWorkerReturns(t *testing.T) {
	cfg, gen := newDaemonOptimizeFixture(t)
	gate := api.NewSerialOperationGate()
	adapter := &storeAPIAdapter{config: cfg, operationGate: gate}
	originalWorker := runAcceleratorWorkerSubprocess
	defer func() { runAcceleratorWorkerSubprocess = originalWorker }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runAcceleratorWorkerSubprocess = func(ctx context.Context, _ string, _ string, _ vector.GenerationID, _ int, _ io.Writer) error {
		duplicate := adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "optimize", "--drop"}}, nil)
		require.ErrorContains(t, duplicate, "already running")
		cancel()
		<-ctx.Done()
		_, _, held := gate.Holder()
		assert.True(t, held, "cancellation retains the lease until the worker is reaped")
		return ctx.Err()
	}
	err := adapter.RunCLICommand(ctx, api.CLIRunRequest{Args: []string{"embeddings", "optimize", strconv.FormatInt(int64(gen), 10)}}, nil)
	require.ErrorIs(t, err, context.Canceled)
	_, _, held := gate.Holder()
	assert.False(t, held, "worker cleanup releases its final lease")
}

func TestDaemonOptimizeRejectsInvalidInvocationBeforeGateAdmission(t *testing.T) {
	for _, tail := range [][]string{
		{"--drop=invalid"}, {"1", "2"}, {"--unknown"},
		{"--log-level=invalid"}, {"--config=other.toml"}, {"--home=/tmp"}, {"--local"}, {"--log-sql"},
	} {
		t.Run(strings.Join(tail, " "), func(t *testing.T) {
			gate := api.NewSerialOperationGate()
			gate.StartDrain()
			adapter := &storeAPIAdapter{operationGate: gate}
			err := adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: append([]string{"embeddings", "optimize"}, tail...)}, nil)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "shutting down", "real parser rejects the invocation before admission")
		})
	}
}

// Every recognized optimize spelling must stay out of the generic subprocess
// runner, even when its flag values or positional arguments are invalid.
func FuzzDaemonOptimizeNeverFallsBackToSubprocess(f *testing.F) {
	f.Add(uint8(0), "debug")
	f.Add(uint8(1), "invalid")
	f.Add(uint8(2), "--help")
	f.Add(uint8(3), "debug")
	f.Fuzz(func(t *testing.T, shape uint8, value string) {
		var args []string
		switch shape % 4 {
		case 0:
			args = []string{"embeddings", "--log-level=" + value, "optimize"}
		case 1:
			args = []string{"embeddings", "--config=" + value, "optimize"}
		case 2:
			args = []string{"embeddings", "optimize", value}
		default:
			args = []string{"embeddings", "-" + strings.Repeat("v", int(shape)+1), "optimize", value}
		}
		adapter := &storeAPIAdapter{}
		ranSubprocess := false
		_ = adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: args}, nil,
			func(context.Context, []string, map[string]string, string, func(string, string) error) error {
				ranSubprocess = true
				return nil
			})
		assert.False(t, ranSubprocess, "self-gated command validation must never fall back to an ungated process")
	})
}

func TestDaemonOptimizeOutputFailureReleasesGate(t *testing.T) {
	cfg, gen := newDaemonOptimizeFixture(t)
	gate := api.NewSerialOperationGate()
	adapter := &storeAPIAdapter{config: cfg, operationGate: gate}
	disconnected := errors.New("stream disconnected")
	err := adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "optimize", strconv.FormatInt(int64(gen), 10)}}, func(event api.CLIRunEvent) error {
		if strings.HasPrefix(event.Data, "Preparing accelerator:") {
			return disconnected
		}
		return nil
	})
	require.ErrorIs(t, err, disconnected)
	_, _, held := gate.Holder()
	assert.False(t, held)
	require.NoError(t, adapter.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"embeddings", "optimize", "--drop", strconv.FormatInt(int64(gen), 10)}}, nil), "stream failure also releases the duplicate guard")
}

func TestDaemonOptimizeReportsInitialGateWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := api.NewSerialOperationGate()
		release, ok := gate.BeginWorkContext(t.Context())
		require.True(t, ok)
		defer release()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		adapter := &storeAPIAdapter{operationGate: gate}
		waiting := false
		err := adapter.RunCLICommand(ctx, api.CLIRunRequest{Args: []string{"embeddings", "optimize"}}, func(event api.CLIRunEvent) error {
			if strings.Contains(event.Data, "Waiting for archive operation gate") {
				waiting = true
				cancel()
			}
			return nil
		})
		require.ErrorIs(t, err, context.Canceled, "waiting status reaches the client before admission")
		assert.True(t, waiting)
		_, _, held := gate.Holder()
		assert.True(t, held, "cancelling a queued optimize cannot release another holder")
	})
}
