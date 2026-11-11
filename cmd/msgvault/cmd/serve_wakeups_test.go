package cmd

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWakeupJobOnlyForUsableEntries(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	sched := scheduler.New(nil)
	require.NoError(configureWakeupJob(t.Context(), sched, nil, st, nil, nil))
	assert.False(sched.IsJobScheduled("wakeups"))
	t.Setenv("MSGVAULT_TEST_WAKEUP_KEY", "whsec_"+base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32))))
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[[wakeups]]\nname='bad'\nurl='https://example.com/'\nsecret_env='MISSING_WAKEUP_TEST'\n[[wakeups]]\nname='good'\nurl='https://example.com/'\nsecret_env='MSGVAULT_TEST_WAKEUP_KEY'\n"), 0600))
	cfg, err := config.Load(path, "")
	require.NoError(err)
	entries := cfg.Wakeups
	t.Setenv("MISSING_WAKEUP_TEST", "")
	for _, name := range []string{"bad", "removed"} {
		ok, err := st.AdvanceWakeupState(t.Context(), name, nil, store.WakeupState{At: time.Now().UTC()})
		require.NoError(err)
		require.True(ok)
	}
	require.NoError(configureWakeupJob(t.Context(), sched, nil, st, entries[:1], nil))
	assert.False(sched.IsJobScheduled("wakeups"))
	_, raw, err := st.LoadWakeupState(t.Context(), "bad")
	require.NoError(err)
	assert.NotNil(raw)
	_, raw, err = st.LoadWakeupState(t.Context(), "removed")
	require.NoError(err)
	assert.Nil(raw)
	require.NoError(configureWakeupJob(t.Context(), sched, nil, st, entries, nil))
	assert.True(sched.IsJobScheduled("wakeups"))
}
