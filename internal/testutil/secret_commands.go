package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// SecretCommand builds a small external store executable. Using the CLI test
// binary would load its database/UI dependencies on every credential operation.
func SecretCommand(tb testing.TB, mode string) []string {
	tb.Helper()
	exe := os.Getenv("MSGVAULT_TEST_SECRET_EXECUTABLE")
	if exe == "" {
		exe = filepath.Join(tb.TempDir(), "secret-store")
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		cmd := exec.Command("go", "build", "-p=1", "-o", exe, "go.kenn.io/msgvault/internal/testutil/secretcommand/cmd") //nolint:gosec // Build the fixed fixture package in the test's private directory.
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		output, err := cmd.CombinedOutput()
		require.NoError(tb, err, "%s", output)
		tb.Setenv("MSGVAULT_TEST_SECRET_EXECUTABLE", exe)
	}
	return []string{exe, mode}
}

type SecretStoreCommands struct {
	ReadCommand   []string
	WriteCommand  []string
	DeleteCommand []string
	ListCommand   []string
}

func SecretStoreFixture(tb testing.TB) SecretStoreCommands {
	tb.Helper()
	tb.Setenv("MSGVAULT_TEST_SECRET_ROOT", tb.TempDir())
	return SecretStoreCommands{ReadCommand: SecretCommand(tb, "read"), WriteCommand: SecretCommand(tb, "write"), DeleteCommand: SecretCommand(tb, "delete"), ListCommand: SecretCommand(tb, "list")}
}
