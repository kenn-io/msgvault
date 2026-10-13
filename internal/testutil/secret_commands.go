package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

var secretTemplate struct {
	once sync.Once
	data []byte
	err  error
}

// SecretCommand copies a fixture built once per process into the test's private directory.
func SecretCommand(tb testing.TB, mode string) []string {
	tb.Helper()
	secretTemplate.once.Do(func() {
		dir := tb.TempDir()
		exe := filepath.Join(dir, "secret-store")
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		cmd := exec.Command("go", "build", "-p=1", "-o", exe, "go.kenn.io/msgvault/internal/testutil/secretcommand/cmd") //nolint:gosec // Build the fixed fixture package in a private scratch directory.
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		output, err := cmd.CombinedOutput()
		if err != nil {
			secretTemplate.err = fmt.Errorf("build secret fixture: %w: %s", err, output)
			return
		}
		secretTemplate.data, secretTemplate.err = os.ReadFile(exe)
	})
	require.NoError(tb, secretTemplate.err)
	exe := filepath.Join(tb.TempDir(), "secret-store")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	require.NoError(tb, os.WriteFile(exe, secretTemplate.data, 0700)) //nolint:gosec // exe is inside the test's private directory.
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
	exe := SecretCommand(tb, "read")[0]
	return SecretStoreCommands{ReadCommand: []string{exe, "read"}, WriteCommand: []string{exe, "write"}, DeleteCommand: []string{exe, "delete"}, ListCommand: []string{exe, "list"}}
}
