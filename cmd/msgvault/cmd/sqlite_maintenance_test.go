package cmd

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRegisterSQLiteMaintenanceJobRunsDailyMaintenance(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := testutil.NewSQLiteTestStoreAt(t, filepath.Join(t.TempDir(), "archive.db"))

	sched := scheduler.New(func(context.Context, string) error { return nil })
	t.Cleanup(func() { <-sched.Stop().Done() })
	require.NoError(registerSQLiteMaintenanceJob(sched, s))
	require.True(sched.IsJobScheduled(sqliteMaintenanceJob))

	require.NoError(sched.TriggerJob(sqliteMaintenanceJob))
	jobs := sched.JobStatus()
	require.Len(jobs, 1)
	assert.Equal("29 4 * * *", jobs[0].Schedule)
	assert.Empty(jobs[0].LastError)
	assert.False(jobs[0].LastRun.IsZero(), "maintenance ran")
}
