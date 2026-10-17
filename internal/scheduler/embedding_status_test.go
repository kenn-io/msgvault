package scheduler

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

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
			queuedAt := time.Now().UTC()
			go s.cron.Entry(s.genericJobs[embedJobName]).Job.Run()
			synctest.Wait()
			status := s.EmbeddingStatus()
			assert.Equal("queued", status.Job.State)
			require.NotNil(status.Job.QueuedAt)
			assert.Equal(queuedAt, *status.Job.QueuedAt)
			release()
			synctest.Wait()
			status = s.EmbeddingStatus()
			assert.Equal("idle", status.Job.State)
		})
	})
	t.Run("post-sync run starts after the sync releases its slot", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			gate := newSerialWorkTracker()
			runner := &slowRunner{gate: make(chan struct{}), release: make(chan struct{})}
			backend := &fakeBackend{active: vector.Generation{ID: 1}}
			s := New(func(context.Context, string) error { return nil }).WithWorkTracker(gate)
			defer func() { <-s.Stop().Done() }()
			require.NoError(s.SetEmbedJob(&EmbedJob{Worker: runner, Backend: backend, BackstopInterval: -1}, "", true))
			require.NoError(s.AddAccount("synthetic@example.com", "0 0 * * *"))
			require.NoError(s.TriggerSync("synthetic@example.com"))
			<-runner.gate
			status := s.EmbeddingStatus()
			assert.Equal("running", status.Job.State)
			require.NotNil(status.Job.StartedAt)
			accounts := s.Status()
			require.Len(accounts, 1)
			assert.False(accounts[0].Running, "the sync finished before indexing took the slot")
			close(runner.release)
			synctest.Wait()
			assert.Equal("idle", s.EmbeddingStatus().Job.State)
		})
	})
}
