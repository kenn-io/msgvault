package cmd

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const filenameDiscoveryJobName = "filename-discovery"

// Registration does no archive work. The normal scheduler owns each bounded
// batch's cancellation, yielding and retry after a writer-lock failure.
// Background keeps a pass that waits behind a long sync from asking that
// sync to yield every minute.
func registerFilenameDiscoveryJob(sched *scheduler.Scheduler, st *store.Store) error {
	return sched.AddJob(scheduler.Job{
		Name:        filenameDiscoveryJobName,
		Interval:    5 * time.Second,
		Background:  true,
		Preemptible: true,
		MaxRuntime:  30 * time.Second,
		Run: func(ctx context.Context) error {
			batch, err := st.FilenameDiscoveryBatchContext(ctx)
			if err != nil {
				return err
			}
			if batch.More {
				return scheduler.ErrReschedule
			}
			return nil
		},
	})
}
