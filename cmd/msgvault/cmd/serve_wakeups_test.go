package cmd

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWakeupJobOnlyForUsableEntries(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	sched := scheduler.New(nil)
	require.NoError(configureWakeupJob(t.Context(), sched, nil, st, &config.Config{}, nil))
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
	require.NoError(configureWakeupJob(t.Context(), sched, nil, st, &config.Config{Wakeups: entries[:1]}, nil))
	assert.False(sched.IsJobScheduled("wakeups"))
	_, raw, err := st.LoadWakeupState(t.Context(), "bad")
	require.NoError(err)
	assert.NotNil(raw)
	_, raw, err = st.LoadWakeupState(t.Context(), "removed")
	require.NoError(err)
	assert.Nil(raw)
	require.NoError(configureWakeupJob(t.Context(), sched, nil, st, cfg, nil))
	assert.True(sched.IsJobScheduled("wakeups"))
	_, raw, err = st.LoadWakeupState(t.Context(), "good")
	require.NoError(err)
	assert.NotNil(raw)
}

func TestWakeupJobLoadsRelativeSecret(t *testing.T) {
	require := Require.New(t)
	dir := t.TempDir()
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	require.NoError(fileutil.SecureWriteFile(filepath.Join(dir, "secret"), []byte(secret), 0600))
	path := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(path, []byte("[[wakeups]]\nname='file'\nurl='https://example.com/'\nsecret_file='secret'\n"), 0600))
	cfg, err := config.Load(path, "")
	require.NoError(err)
	sched := scheduler.New(nil)
	require.NoError(configureWakeupJob(t.Context(), sched, nil, testutil.NewTestStore(t), cfg, nil))
	Assert.True(t, sched.IsJobScheduled("wakeups"))
}

// wakeupSetupGate refuses setup once, then admits the scheduled retry.
type wakeupSetupGate struct {
	*api.SerialOperationGate
	refuse bool
}

func (g *wakeupSetupGate) BeginLabeledWorkContext(ctx context.Context, label string) (func(), bool) {
	if g.refuse {
		return nil, false
	}
	return g.SerialOperationGate.BeginLabeledWorkContext(ctx, label)
}

func TestWakeupJobRecoversAfterSetupGateFailure(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	t.Setenv("MSGVAULT_TEST_WAKEUP_KEY", "whsec_"+base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32))))
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[[wakeups]]\nname='good'\nurl='https://example.com/'\nsecret_env='MSGVAULT_TEST_WAKEUP_KEY'\n"), 0600))
	cfg, err := config.Load(path, "")
	require.NoError(err)
	st := testutil.NewTestStore(t)
	gate := &wakeupSetupGate{SerialOperationGate: api.NewSerialOperationGate(), refuse: true}
	sched := scheduler.New(nil)
	require.NoError(configureWakeupJob(t.Context(), sched, gate, st, cfg, nil))
	assert.True(sched.IsJobScheduled("wakeups"))
	_, raw, err := st.LoadWakeupState(t.Context(), "good")
	require.NoError(err)
	assert.Nil(raw)
	gate.refuse = false
	sched.Start()
	defer func() { <-sched.Stop().Done() }()
	require.NoError(sched.TriggerJob("wakeups"))
	_, raw, err = st.LoadWakeupState(t.Context(), "good")
	require.NoError(err)
	assert.NotNil(raw)
}
