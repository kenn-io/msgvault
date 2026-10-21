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
	t.Run("keeps the first queue time while triggers coalesce", func(t *testing.T) {
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
			assert.Equal(JobStarted, mustStartJob(t, s, embedJobName))
			synctest.Wait()
			queuedAt := time.Now().UTC()
			time.Sleep(time.Minute)
			assert.Equal(JobCoalesced, mustStartJob(t, s, embedJobName))
			status := s.EmbeddingStatus()
			assert.Equal("queued", status.Job.State)
			require.NotNil(status.Job.QueuedAt)
			assert.Equal(queuedAt, *status.Job.QueuedAt)
			release()
			synctest.Wait()
			assert.Equal("idle", s.EmbeddingStatus().Job.State)
		})
	})
	t.Run("post-sync run waits for the sync to release the slot", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			gate := newSerialWorkTracker()
			runner := &slowRunner{gate: make(chan struct{}), release: make(chan struct{})}
			backend := &fakeBackend{active: vector.Generation{ID: 1}}
			s := New(func(context.Context, string) error { return nil }).WithWorkTracker(gate)
			defer func() { <-s.Stop().Done() }()
			require.NoError(s.SetEmbedJob(&EmbedJob{Worker: runner, Backend: backend}, "", true))
			require.NoError(s.AddAccount("synthetic@example.com", "0 0 1 1 *"))
			require.NoError(s.TriggerSync("synthetic@example.com", false))
			<-runner.gate
			status := s.EmbeddingStatus()
			assert.Equal("running", status.Job.State)
			require.NotNil(status.Job.StartedAt)
			synctest.Wait()
			accounts := s.Status()
			require.Len(accounts, 1)
			assert.False(accounts[0].Running, "the sync finished before indexing took the slot")
			close(runner.release)
			synctest.Wait()
			assert.Equal("idle", s.EmbeddingStatus().Job.State)
		})
	})
}

func TestJobStatusIncludesPostSyncOnlyCallbacks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s := New(nil)
	defer func() { <-s.Stop().Done() }()
	require.NoError(s.SetEmbedJob(&EmbedJob{}, "", true))
	require.NoError(s.SetDocumentVectorJob(func(context.Context) error { return nil }, "", true))

	statuses := s.JobStatus()
	require.Len(statuses, 2)
	assert.Equal([]string{documentVectorJobName, embedJobName}, []string{statuses[0].Name, statuses[1].Name})
	for _, status := range statuses {
		assert.Empty(status.Schedule)
		assert.True(status.NextRun.IsZero())
		assert.False(s.IsJobScheduled(status.Name), "callback registration does not create a cron schedule")
	}
}
