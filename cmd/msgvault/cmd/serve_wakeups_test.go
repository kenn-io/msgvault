package cmd

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWakeupJobOnlyForUsableEntries(t *testing.T) {
	st := testutil.NewTestStore(t)
	sched := scheduler.New(nil)
	require.NoError(t, configureWakeupJob(t.Context(), sched, nil, st, nil, nil))
	assert.False(t, sched.IsJobScheduled("wakeups"))
	t.Setenv("MSGVAULT_TEST_WAKEUP_KEY", "whsec_"+base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32))))
	entries := []config.WakeupConfig{{Name: "bad", URL: "https://example.com/", SecretEnv: "MISSING_WAKEUP_TEST"}, {Name: "good", URL: "https://example.com/", SecretEnv: "MSGVAULT_TEST_WAKEUP_KEY"}}
	require.NoError(t, configureWakeupJob(t.Context(), sched, nil, st, entries, nil))
	assert.True(t, sched.IsJobScheduled("wakeups"))
}
