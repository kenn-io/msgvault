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
			go s.cron.Entry(s.embed.entry).Job.Run()
			synctest.Wait()
			status := s.EmbeddingStatus()
			assert.Equal("queued", status.Job.State)
			assert.NotNil(status.Job.QueuedAt)
			release()
			synctest.Wait()
			status = s.EmbeddingStatus()
			assert.Equal("idle", status.Job.State)
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
			require.NoError(s.AddAccount("synthetic@example.com", "0 0 * * *"))
			require.NoError(s.TriggerSync("synthetic@example.com"))
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
	t.Run("overlapping runs report the earliest start", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assert := assert.New(t)
			s := New(nil)
			defer func() { <-s.Stop().Done() }()
			firstRelease, secondRelease := make(chan struct{}), make(chan struct{})
			blocking := func(release chan struct{}) func(context.Context) error {
				return func(context.Context) error {
					<-release
					return nil
				}
			}
			go func() { _ = s.runEmbeddingJob(blocking(firstRelease), false) }()
			synctest.Wait()
			firstStarted := *s.EmbeddingStatus().Job.StartedAt
			time.Sleep(time.Minute)
			go func() { _ = s.runEmbeddingJob(blocking(secondRelease), false) }()
			synctest.Wait()
			close(secondRelease)
			synctest.Wait()
			status := s.EmbeddingStatus()
			assert.Equal("running", status.Job.State)
			assert.Equal(firstStarted, *status.Job.StartedAt, "the run still going started first")
			close(firstRelease)
			synctest.Wait()
			assert.Equal("idle", s.EmbeddingStatus().Job.State)
		})
	})
}
