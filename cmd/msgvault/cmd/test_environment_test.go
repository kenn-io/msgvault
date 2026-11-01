package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMain(m *testing.M) {
	// Default configs must never inherit the caller's archive, including an
	// explicit MSGVAULT_HOME. Individual fixtures can still choose their own paths.
	testHome, err := os.MkdirTemp("", "msgvault-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create CLI test home:", err)
		os.Exit(1)
	}
	if err := os.Setenv("MSGVAULT_HOME", testHome); err != nil {
		_ = os.RemoveAll(testHome)
		fmt.Fprintln(os.Stderr, "set CLI test home:", err)
		os.Exit(1)
	}
	localEmbeddingsDir = func() (string, error) { return filepath.Join(testHome, "embeddings"), nil }
	// Tests that run serve must never launch a real daemon.
	startServeBackgroundProcessForRun = func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		return nil, errors.New("test reached the real background daemon launcher; stub it with stubStartServeBackgroundProcess or disable daemon auto-start")
	}
	// Single-route remote stubs omit /api/v1/health. Schema-probe tests re-enable it.
	remoteAPISchemaCheckEnabled = false
	exitCode := m.Run()
	if err := os.RemoveAll(testHome); err != nil {
		fmt.Fprintln(os.Stderr, "remove CLI test home:", err)
		exitCode = 1
	}
	os.Exit(exitCode)
}

func TestCLITestsDoNotPurgeCallerAttachments(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	if os.Getenv("MSGVAULT_TEST_ISOLATION_CHILD") == "1" {
		cfg := config.NewDefaultConfig()
		// Reproduce an incomplete fixture override: DataDir still uses the
		// process default. TestMain must keep that default away from the caller.
		cfg.HomeDir = t.TempDir()
		hash := strings.Repeat("ab", 32)
		path := seedAttachmentFile(t, cfg.AttachmentsDir(), hash[:2]+"/"+hash, "test-owned orphan")
		fixture := purgeMediaFixture{store: testutil.NewTestStore(t), config: cfg}
		command := newPurgeExcludedMediaLocalCmd(fixture.deps())
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		command.SetArgs([]string{"--yes"})
		require.NoError(command.Execute())
		assert.NoFileExists(path, "the real purge must execute inside the test process")
		return
	}

	callerHome := t.TempDir()
	hash := strings.Repeat("cd", 32)
	path := seedAttachmentFile(t, filepath.Join(callerHome, "attachments"), hash[:2]+"/"+hash, "caller-owned recording")
	executable, err := os.Executable()
	require.NoError(err)
	child := exec.Command(executable, "-test.run=^TestCLITestsDoNotPurgeCallerAttachments$")
	child.Env = append(os.Environ(), "MSGVAULT_HOME="+callerHome, "MSGVAULT_TEST_ISOLATION_CHILD=1")
	output, err := child.CombinedOutput()
	require.NoError(err, "%s", output)
	payload, err := os.ReadFile(path)
	require.NoError(err, "running the tests must not delete the caller's attachments")
	assert.Equal("caller-owned recording", string(payload))
}
