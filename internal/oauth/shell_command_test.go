package oauth

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The child reports argv because the contract is preservation across a real shell.
func TestFormatShellCommandChild(t *testing.T) {
	if os.Getenv("MSGVAULT_SHELL_CHILD") != "1" {
		return
	}
	start := 0
	for i, arg := range os.Args {
		if arg == "--" {
			start = i + 1
			break
		}
	}
	encoded, err := json.Marshal(os.Args[start:])
	require.NoError(t, err)
	fmt.Print(string(encoded))
	os.Exit(0)
}

func TestFormatShellCommand(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "child's $ command"+filepath.Ext(binary))
	content, err := os.ReadFile(binary)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, content, 0700))
	want := []string{"invoice's $total with spaces", "--account=archive@example.com", "--limit", "100", ""}
	for _, shell := range []string{"sh", "pwsh"} {
		t.Run(shell, func(t *testing.T) {
			shellPath, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is unavailable: %v", shell, err)
			}
			args := append([]string{path, "-test.run=^TestFormatShellCommandChild$", "--"}, want...)
			command := FormatShellCommand(args, shell == "pwsh")
			flags := []string{"-c", command}
			if shell == "pwsh" {
				flags = []string{"-NoProfile", "-NonInteractive", "-Command", command}
			}
			child := exec.CommandContext(t.Context(), shellPath, flags...)
			child.Env = append(os.Environ(), "MSGVAULT_SHELL_CHILD=1")
			out, err := child.CombinedOutput()
			require.NoError(t, err, string(out))
			var got []string
			require.NoError(t, json.Unmarshal(out, &got), string(out))
			assert.Equal(t, want, got)
		})
	}
}
