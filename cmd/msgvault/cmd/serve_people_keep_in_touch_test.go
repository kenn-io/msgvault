package cmd

import (
	"context"
	"errors"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/keepintouch"
	"go.kenn.io/msgvault/internal/scheduler"
	"testing"
)

func TestKeepInTouchScheduleIsExplicitAndUsesCronParser(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	for _, enabled := range []bool{false, true} {
		sched := scheduler.New(nil)
		cfg := keepintouch.DefaultConfig()
		cfg.Enabled = enabled
		cfg.Owner = "operator"
		require.NoError(addKeepInTouchJob(sched, cfg, func(context.Context) error { return nil }))
		assert.Equal(enabled, sched.IsJobScheduled(keepInTouchJobName))
	}
	sched := scheduler.New(nil)
	cfg := keepintouch.DefaultConfig()
	cfg.Enabled = true
	cfg.Owner = "operator"
	cfg.Schedule = "not a cron"
	require.Error(addKeepInTouchJob(sched, cfg, func(context.Context) error { return nil }))
	assert.False(sched.IsJobScheduled(keepInTouchJobName))
}

func TestKeepInTouchScheduledFailureKeepsSafeActionableCause(t *testing.T) {
	assert := Assert.New(t)

	result := keepintouch.Result{Failures: []keepintouch.Failure{{PersonID: 7, Error: "private provider output must not be logged", Summary: "inference consent is missing or revoked"}}}
	err := keepInTouchScheduledResult(result, errors.New("keep-in-touch could not process 1 people"))
	Require.Error(t, err)
	assert.Contains(err.Error(), "person 7")
	assert.Contains(err.Error(), "consent is missing or revoked")
	assert.NotContains(err.Error(), "private provider output")
}
