package store_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func policyFenceStore(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.OpenForTest(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}
func TestSQLiteConversationPolicyFenceAcrossStoreInstances(t *testing.T) {
	requirements := require.New(t)

	path := filepath.Join(t.TempDir(), "archive.db")
	a := policyFenceStore(t, path)
	b := policyFenceStore(t, path)
	r1, err := a.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyShared)
	requirements.NoError(err)
	r2, err := b.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyShared)
	requirements.NoError(err)
	r1()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond) //nolint:kennlint // the deadline is the expected result; the second Store's shared policy fence remains held
	defer cancel()
	_, err = a.AcquireSQLiteConversationPolicyFence(ctx, store.ConversationPolicyExclusive)
	requirements.Error(err)
	r2()
	w, err := a.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyExclusive)
	requirements.NoError(err)
	w()
	w()
}
func TestSQLiteConversationPolicyFenceCancellation(t *testing.T) {
	s := policyFenceStore(t, filepath.Join(t.TempDir(), "archive.db"))
	r, err := s.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyShared)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond) //nolint:kennlint // the deadline is the expected result; the shared policy fence is held until cancellation returns
	defer cancel()
	_, err = s.AcquireSQLiteConversationPolicyFence(ctx, store.ConversationPolicyExclusive)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	r()
	w, err := s.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyExclusive)
	require.NoError(t, err)
	w()
}
func TestSQLiteConversationPolicyFenceCanonicalPath(t *testing.T) {
	requirements := require.New(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "archive.db")
	a := policyFenceStore(t, path)
	alias := filepath.Join(dir, "alias.db")
	requirements.NoError(os.Symlink(path, alias))
	b := policyFenceStore(t, "file:"+alias+"?mode=rw")
	cwd, err := os.Getwd()
	requirements.NoError(err)
	relative, err := filepath.Rel(cwd, path)
	requirements.NoError(err)
	relativeStore := policyFenceStore(t, relative)
	readOnly, err := store.OpenReadOnly(path)
	requirements.NoError(err)
	defer func() { require.NoError(t, readOnly.Close()) }()

	r, err := a.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyShared)
	requirements.NoError(err)
	defer r()
	for _, aliased := range []*store.Store{b, relativeStore, readOnly} {
		aliasCtx, aliasCancel := context.WithTimeout(t.Context(), 100*time.Millisecond) //nolint:kennlint // the deadline is the expected result; the canonical archive's shared policy fence is held across every alias
		_, err = aliased.AcquireSQLiteConversationPolicyFence(aliasCtx, store.ConversationPolicyExclusive)
		aliasCancel()
		requirements.ErrorIs(err, context.DeadlineExceeded)
	}
	assert.FileExists(t, path+".conversation-policy.lock")
	other := policyFenceStore(t, filepath.Join(dir, "other.db"))
	w, err := other.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyExclusive)
	requirements.NoError(err)
	w()
}
func TestSQLiteConversationPolicyFenceAcrossProcess(t *testing.T) {
	requirements := require.New(t)

	if path := os.Getenv("MSGVAULT_POLICY_FENCE_CHILD"); path != "" {
		s := policyFenceStore(t, path)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond) //nolint:kennlint // the deadline is the expected result; the parent process holds the same archive's shared policy fence
		defer cancel()
		_, err := s.AcquireSQLiteConversationPolicyFence(ctx, store.ConversationPolicyExclusive)
		requirements.ErrorIs(err, context.DeadlineExceeded)
		return
	}
	path := filepath.Join(t.TempDir(), "archive.db")
	s := policyFenceStore(t, path)
	r, err := s.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyShared)
	requirements.NoError(err)
	defer r()
	executable, err := os.Executable()
	requirements.NoError(err)
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestSQLiteConversationPolicyFenceAcrossProcess$")
	cmd.Env = append(os.Environ(), "MSGVAULT_POLICY_FENCE_CHILD="+path)
	out, err := cmd.CombinedOutput()
	requirements.NoError(err, string(out))
}

func TestSQLiteConversationPolicyFenceMemoryScopedRoot(t *testing.T) {
	s := policyFenceStore(t, ":memory:")
	r, err := s.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyShared)
	require.NoError(t, err)
	scoped := s.ScopedToSync(1, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond) //nolint:kennlint // the deadline is the expected result; the root Store holds the shared in-memory policy fence
	defer cancel()
	_, err = scoped.AcquireSQLiteConversationPolicyFence(ctx, store.ConversationPolicyExclusive)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	r()
	w, err := scoped.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyExclusive)
	require.NoError(t, err)
	w()
}

func TestSQLiteConversationPolicyFenceNamedMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	s := policyFenceStore(t, "file:"+path+"?mode=memory&cache=shared")
	release, err := s.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyExclusive)
	require.NoError(t, err)
	release()
	assert.NoFileExists(t, path+".conversation-policy.lock")
}
