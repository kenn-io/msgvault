package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
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
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWakeupJobOnlyForUsableEntries(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	sched := scheduler.New(nil)
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	t.Setenv("MSGVAULT_TEST_WAKEUP_KEY", secret)
	dir := t.TempDir()
	require.NoError(fileutil.SecureWriteFile(filepath.Join(dir, "secret"), []byte(secret), 0600))
	path := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(path, []byte("[[wakeups]]\nname='bad'\nurl='https://example.com/'\nsecret_env='MISSING_WAKEUP_TEST'\n[[wakeups]]\nname='good'\nurl='https://example.com/'\nsecret_env='MSGVAULT_TEST_WAKEUP_KEY'\n[[wakeups]]\nname='file'\nurl='https://example.com/'\nsecret_file='secret'\n"), 0600))
	cfg, err := config.Load(path, "")
	require.NoError(err)
	entries := cfg.Wakeups
	t.Setenv("MISSING_WAKEUP_TEST", "")
	for _, name := range []string{"bad", "removed"} {
		ok, err := st.AdvanceWakeupStateKey(t.Context(), store.WakeupStateKey(name), nil, store.WakeupState{At: time.Now().UTC()})
		require.NoError(err)
		require.True(ok)
	}
	require.NoError(configureWakeupJob(t.Context(), sched, nil, st, &config.Config{Wakeups: entries[:1]}, nil, nil))
	assert.False(sched.IsJobScheduled("wakeups"))
	_, raw, err := st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("bad"))
	require.NoError(err)
	assert.NotNil(raw)
	_, raw, err = st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("removed"))
	require.NoError(err)
	assert.Nil(raw)
	require.NoError(configureWakeupJob(t.Context(), sched, nil, st, cfg, nil, nil))
	assert.True(sched.IsJobScheduled("wakeups"))
	_, raw, err = st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("good"))
	require.NoError(err)
	assert.NotNil(raw)
	_, raw, err = st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("file"))
	require.NoError(err)
	assert.NotNil(raw)
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
	require.NoError(configureWakeupJob(t.Context(), sched, gate, st, cfg, nil, nil))
	assert.True(sched.IsJobScheduled("wakeups"))
	_, raw, err := st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("good"))
	require.NoError(err)
	assert.Nil(raw)
	gate.refuse = false
	defer func() { <-sched.Stop().Done() }()
	require.NoError(sched.TriggerJob("wakeups"))
	_, raw, err = st.LoadWakeupStateKey(t.Context(), store.WakeupStateKey("good"))
	require.NoError(err)
	assert.NotNil(raw)
}

func TestWakeupSetupSafeDiagnostics(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	t.Setenv("WAKEUP_BAD_SECRET", "short")
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[[wakeups]]\nname='missing'\nurl='https://example.com/'\nsecret_env='WAKEUP_MISSING_SECRET'\n[[wakeups]]\nname='bad'\nurl='https://example.com/'\nsecret_env='WAKEUP_BAD_SECRET'\n"), 0600))
	cfg, err := config.Load(path, "")
	require.NoError(err)
	t.Setenv("WAKEUP_MISSING_SECRET", "")
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	sched := scheduler.New(nil)
	require.NoError(configureWakeupJob(t.Context(), sched, nil, testutil.NewTestStore(t), cfg, logger, nil))
	text := logs.String()
	assert.Contains(text, "category=secret_unavailable")
	assert.Contains(text, "category=invalid_secret")
	assert.NotContains(text, "short")
}

func TestWakeupJobWithOnlyEvents(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("imap", "events@example.test")
	require.NoError(err)
	now := time.Now().UTC()
	service := &mcpevents.Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
	in, err := mcpevents.DecodeSubscribe([]byte(`{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`))
	require.NoError(err)
	_, err = service.Subscribe(t.Context(), in)
	require.NoError(err)
	service.Enabled = false
	now = now.Add(25 * time.Hour)
	sched := scheduler.New(nil)
	defer func() { <-sched.Stop().Done() }()
	require.NoError(configureWakeupJob(t.Context(), sched, nil, st, &config.Config{}, nil, service))
	require.True(sched.IsJobScheduled("wakeups"))
	require.NoError(sched.TriggerJob("wakeups"))
	catalog, err := st.LoadMCPEventsCatalog(t.Context())
	require.NoError(err)
	assert.JSONEq(`[]`, string(catalog))

	withoutEvents := scheduler.New(nil)
	require.NoError(configureWakeupJob(t.Context(), withoutEvents, nil, st, &config.Config{}, nil, nil))
	assert.False(withoutEvents.IsJobScheduled("wakeups"))
	emptyEvents := scheduler.New(nil)
	require.NoError(configureWakeupJob(t.Context(), emptyEvents, nil, st, &config.Config{}, nil, service))
	assert.False(emptyEvents.IsJobScheduled("wakeups"))
	service.Enabled = true
	enabledEvents := scheduler.New(nil)
	require.NoError(configureWakeupJob(t.Context(), enabledEvents, nil, st, &config.Config{}, nil, service))
	assert.True(enabledEvents.IsJobScheduled("wakeups"))
	broken := testutil.NewTestStore(t)
	require.NoError(broken.Close())
	unreadable := scheduler.New(nil)
	require.NoError(configureWakeupJob(t.Context(), unreadable, nil, broken, &config.Config{}, nil, &mcpevents.Service{Store: broken}))
	assert.True(unreadable.IsJobScheduled("wakeups"))
}
