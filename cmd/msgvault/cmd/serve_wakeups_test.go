package cmd

import (
	"encoding/base64"
	"os"
	"path/filepath"
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
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[[wakeups]]\nname='bad'\nurl='https://example.com/'\nsecret_env='MISSING_WAKEUP_TEST'\n[[wakeups]]\nname='good'\nurl='https://example.com/'\nsecret_env='MSGVAULT_TEST_WAKEUP_KEY'\n"), 0600))
	cfg, err := config.Load(path, "")
	require.NoError(t, err)
	entries := cfg.Wakeups
	require.NoError(t, configureWakeupJob(t.Context(), sched, nil, st, entries, nil))
	assert.True(t, sched.IsJobScheduled("wakeups"))
}
