package scheduler

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestEmbeddingJobStatus(t *testing.T) {
	t.Run("shows queue before slot acquisition", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			gate := newSerialWorkTracker()
			release, ok := gate.BeginWork()
			require.True(ok)
			s := New(nil).WithWorkTracker(gate)
			defer func() { <-s.Stop().Done() }()
			job := &EmbedJob{}
			require.NoError(s.SetEmbedJob(job, "0 0 * * *", true))
			go s.cron.Entry(s.embed.entry).Job.Run()
			synctest.Wait()
			status := s.EmbeddingStatus()
			assert.Equal("queued", status.Job.State)
			assert.NotNil(status.Job.QueuedAt)
			release()
			synctest.Wait()
			status = s.EmbeddingStatus()
			assert.Equal("idle", status.Job.State)
			assert.Equal("0 0 * * *", status.Scheduler.Schedule)
		})
	})
	t.Run("post-sync run inherits slot", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			gate := newSerialWorkTracker()
			runner := &slowRunner{gate: make(chan struct{}), release: make(chan struct{})}
			backend := &fakeBackend{active: vector.Generation{ID: 1}}
			s := New(func(context.Context, string) error { return nil }).WithWorkTracker(gate)
			defer func() { <-s.Stop().Done() }()
			require.NoError(s.SetEmbedJob(&EmbedJob{Worker: runner, Backend: backend}, "", true))
			require.NoError(s.AddAccount("synthetic@example.test", "0 0 * * *"))
			require.NoError(s.TriggerSync("synthetic@example.test"))
			<-runner.gate
			status := s.EmbeddingStatus()
			assert.Equal("running", status.Job.State)
			assert.Zero(s.embedQueued, "post-sync work must inherit its slot")
			require.NotNil(status.Job.StartedAt)
			close(runner.release)
			synctest.Wait()
			assert.Equal("idle", s.EmbeddingStatus().Job.State)
		})
	})
}
