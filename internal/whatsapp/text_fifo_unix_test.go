//go:build unix

package whatsapp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestLoadTextRejectsNamedPipe(t *testing.T) {
	require := require.New(t)
	if path := os.Getenv("MSGVAULT_TEST_TEXT_FIFO"); path != "" {
		_, err := LoadText(t.Context(), path, textOptions())
		require.ErrorContains(err, "TXT input must be a regular file")
		return
	}
	path := filepath.Join(t.TempDir(), "chat.txt")
	require.NoError(unix.Mkfifo(path, 0600))
	executable, err := os.Executable()
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLoadTextRejectsNamedPipe$")
	cmd.Env = append(os.Environ(), "MSGVAULT_TEST_TEXT_FIFO="+path)
	output, err := cmd.CombinedOutput()
	require.NoError(ctx.Err(), "loading a named pipe blocked")
	require.NoError(err, "%s", output)
}
