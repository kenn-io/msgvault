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
	checks := assert.New(t)
	must := require.New(t)
	sched := scheduler.New(nil)
	cfg := importantdate.Config{}
	cfg.ApplyDefaults()
	run := func(context.Context) error { return nil }
	must.NoError(addImportantDateJob(sched, cfg, run))
	checks.False(sched.IsJobScheduled(importantDateJobName))
	cfg.Enabled = true
	cfg.PrivateDestination = true
	cfg.Project = "private-example"
	cfg.Recipient = "owner/agent"
	must.NoError(addImportantDateJob(sched, cfg, run))
	checks.True(sched.IsJobScheduled(importantDateJobName))
}
