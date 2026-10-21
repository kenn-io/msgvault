package cmd

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
)

func TestDaemonCLIPolicyMutationFencesVectorPublication(t *testing.T) {
	requirements := require.New(t)

	if path := os.Getenv("MSGVAULT_TEST_POLICY_ARCHIVE"); path != "" {
		requirements.True(isDaemonCLISubprocess())
		cfg := config.NewDefaultConfig()
		cfg.Data.DataDir = filepath.Dir(path)
		release, err := acquireDirectSQLiteWriteLock(cfg, nil)
		requirements.NoError(err)
		defer release()
		childStore, err := store.OpenForTest(path)
		requirements.NoError(err)
		defer func() { require.NoError(t, childStore.Close()) }()
		command := newPolicyLocalCmd(conversationPolicyDeps{
			open:   func(context.Context) (*store.Store, func(), error) { return childStore, func() {}, nil },
			config: func(context.Context) *config.Config { return cfg },
		})
		command.SetArgs([]string{"set", "1", "room", "full"})
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		ctx := t.Context()
		if os.Getenv("MSGVAULT_TEST_POLICY_BLOCKED") == "1" {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond) //nolint:kennlint // the deadline is the expected result; the parent holds the real archive policy read fence
			defer cancel()
		}
		err = command.ExecuteContext(ctx)
		if os.Getenv("MSGVAULT_TEST_POLICY_BLOCKED") == "1" {
			requirements.ErrorIs(err, context.DeadlineExceeded)
		} else {
			requirements.NoError(err)
		}
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.db")
	s, err := store.OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	requirements.NoError(s.InitSchemaContext(t.Context()))
	src, err := s.GetOrCreateSource("beeper", "account")
	requirements.NoError(err)
	_, err = s.EnsureConversation(src.ID, "room", "Room")
	requirements.NoError(err)
	owner, err := tryAcquireWriteOwnerLock(dir)
	requirements.NoError(err)
	defer func() { require.NoError(t, owner.Close()) }()
	r, err := s.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyShared)
	requirements.NoError(err)
	defer r()
	run := func(blocked string) {
		executable, err := os.Executable()
		require.NoError(t, err)
		child := exec.CommandContext(t.Context(), executable, "-test.run=^TestDaemonCLIPolicyMutationFencesVectorPublication$")
		child.Env = append(os.Environ(), "MSGVAULT_TEST_POLICY_ARCHIVE="+path, "MSGVAULT_TEST_POLICY_BLOCKED="+blocked, daemonCLISubprocessEnv+"="+strconv.Itoa(os.Getpid()))
		output, err := child.CombinedOutput()
		require.NoError(t, err, string(output))
	}
	run("1")
	r()
	run("0")
	rows, err := s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assert.Equal(t, conversationpolicy.Full, rows[0].EffectiveTier)
}
