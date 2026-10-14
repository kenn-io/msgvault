package cmd

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// The real scheduler's external work gate lets the test observe one completed
// pass before permitting a follow-up. It does not replace the Store callback.
type filenameSchedulerGate struct {
	waiting chan struct{}
	permit  chan struct{}
}

func (g *filenameSchedulerGate) BeginWork() (func(), bool) {
	return g.BeginWorkContext(context.Background())
}

func (g *filenameSchedulerGate) BeginWorkContext(ctx context.Context) (func(), bool) {
	select {
	case g.waiting <- struct{}{}:
	case <-ctx.Done():
		return func() {}, false
	}
	select {
	case <-g.permit:
		return func() {}, true
	case <-ctx.Done():
		return func() {}, false
	}
}

// A callback that drains bootstrap instead of yielding would produce1201
// entries before returning to this gate, and would never queue a next pass.
func TestFilenameDiscoverySchedulerOneBatch(t *testing.T) {
	f := storetest.New(t)
	message := f.CreateMessage("filename-scheduler-batches")
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	for i := range 1201 {
		_, err = tx.Exec(f.Store.Rebind(`INSERT INTO attachments
			(message_id, filename, storage_path) VALUES (?, ?, '')`), message, fmt.Sprintf("report-%04d.pdf", i))
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		gate := &filenameSchedulerGate{waiting: make(chan struct{}), permit: make(chan struct{})}
		sched := scheduler.New(nil).WithWorkTracker(gate)
		t.Cleanup(func() { <-sched.Stop().Done() })
		require.NoError(registerFilenameDiscoveryJob(sched, f.Store))
		var count int
		require.NoError(f.Store.DB().QueryRow(`SELECT count(*) FROM filename_discovery_entries`).Scan(&count))
		assert.Zero(count, "registration never bootstraps the archive")
		disposition, err := sched.StartJob("filename-discovery")
		require.NoError(err)
		assert.Equal(scheduler.JobStarted, disposition)
		<-gate.waiting
		gate.permit <- struct{}{}
		<-gate.waiting // follow-up must release and reacquire the scheduler gate
		require.NoError(f.Store.DB().QueryRow(`SELECT count(*) FROM filename_discovery_entries`).Scan(&count))
		assert.Equal(500, count)
		status, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
		require.NoError(err)
		assert.Equal("bootstrap", status.Phase)
		<-sched.Stop().Done()
		synctest.Wait()
		require.NoError(f.Store.DB().QueryRow(`SELECT count(*) FROM filename_discovery_entries`).Scan(&count))
		assert.Equal(500, count, "shutdown cancels the queued next batch")
	})
}

// A registration that starts work outside scheduler ownership would mutate
// state even though shutdown cancels the gate before the first batch.
func TestFilenameDiscoverySchedulerCancellation(t *testing.T) {
	f := storetest.New(t)
	f.CreateMessage("filename-scheduler-cancel")
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		gate := &filenameSchedulerGate{waiting: make(chan struct{}), permit: make(chan struct{})}
		sched := scheduler.New(nil).WithWorkTracker(gate)
		require.NoError(registerFilenameDiscoveryJob(sched, f.Store))
		_, err := sched.StartJob("filename-discovery")
		require.NoError(err)
		<-gate.waiting
		<-sched.Stop().Done()
		synctest.Wait()
		status, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
		require.NoError(err)
		assert.Zero(status.Generation)
		var count int
		require.NoError(f.Store.DB().QueryRow(`SELECT count(*) FROM filename_discovery_entries`).Scan(&count))
		assert.Zero(count)
	})
}

func TestFilenameDiscoveryWarmMaintenanceDoesNotExtendIdleTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := storetest.New(t)
		message := f.CreateMessage("filename-idle-maintenance")
		_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,storage_path)
			VALUES(?,'report.pdf','')`), message)
		require.NoError(err)
		ready := false
		for range 100 {
			batch, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
			require.NoError(err)
			if batch.Status.Phase == "ready" && !batch.More {
				ready = true
				break
			}
		}
		require.True(ready, "filename discovery must finish warming before the maintenance pass")

		timeout := 100 * time.Millisecond
		fired := make(chan struct{})
		idle := api.NewIdleTracker(timeout, func() { close(fired) })
		sched, media := newServeSchedulers(nil, testDiscardLogger(), idle, nil)
		t.Cleanup(func() { <-sched.Stop().Done(); <-media.Stop().Done() })
		require.NoError(registerFilenameDiscoveryJob(sched, f.Store))
		started := time.Now()
		go idle.Run(t.Context())
		synctest.Wait()
		time.Sleep(timeout / 4)
		synctest.Wait()
		require.NoError(sched.TriggerJob(filenameDiscoveryJobName))
		synctest.Wait()
		// Advance the synctest clock to the original idle deadline.
		time.Sleep(timeout - timeout/4)
		synctest.Wait()
		select {
		case <-fired:
		default:
			require.FailNow("idle timeout did not fire after the warmed maintenance pass")
		}
		assert.LessOrEqual(time.Since(started), timeout+time.Millisecond)
	})
}
