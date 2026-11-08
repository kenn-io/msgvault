package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importantdate"
	"go.kenn.io/msgvault/internal/scheduler"
)

func TestImportantDateScheduleIsOptIn(t *testing.T) {
	sched := scheduler.New(nil)
	cfg := importantdate.Config{}
	cfg.ApplyDefaults()
	run := func(context.Context) error { return nil }
	require.NoError(t, addImportantDateJob(sched, cfg, run))
	assert.False(t, sched.IsJobScheduled(importantDateJobName))
	cfg.Enabled = true
	cfg.PrivateDestination = true
	cfg.Project = "private-example"
	cfg.Recipient = "owner/agent"
	require.NoError(t, addImportantDateJob(sched, cfg, run))
	assert.True(t, sched.IsJobScheduled(importantDateJobName))
}
