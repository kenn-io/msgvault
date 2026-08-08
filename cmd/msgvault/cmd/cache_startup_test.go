package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestStartupCacheBuildIntentFromEnv(t *testing.T) {
	t.Setenv(daemonStartupCacheBuildEnv, string(startupCacheBuildIntentFull))

	intent, err := startupCacheBuildIntentFromEnv()

	require.NoError(t, err)
	assert.Equal(t, startupCacheBuildIntentFull, intent)
}

func TestStartupCacheBuildIntentFromEnvRejectsUnknownValue(t *testing.T) {
	t.Setenv(daemonStartupCacheBuildEnv, "surprise")

	_, err := startupCacheBuildIntentFromEnv()

	require.ErrorContains(t, err, "invalid daemon startup cache build intent")
}

func TestWithStartupCacheBuildIntentReplacesStaleValue(t *testing.T) {
	base := []string{"KEEP=1", daemonStartupCacheBuildEnv + "=default", "TAIL=2"}

	got := withStartupCacheBuildIntent(base, startupCacheBuildIntentFull)

	assert.Equal(t, []string{
		"KEEP=1",
		daemonStartupCacheBuildEnv + "=full",
		"TAIL=2",
	}, got)
}

func TestStartupCacheBuildOutcomeFromRuntime(t *testing.T) {
	rt := &DaemonRuntime{Record: daemon.RuntimeRecord{Metadata: map[string]string{
		runtimeStartupCacheBuildOutcome: string(startupCacheBuildOutcomeFailed),
	}}}

	assert.Equal(t, startupCacheBuildOutcomeFailed, startupCacheBuildOutcomeFromRuntime(rt))
	assert.Equal(t, startupCacheBuildOutcomeNone, startupCacheBuildOutcomeFromRuntime(nil))
}
