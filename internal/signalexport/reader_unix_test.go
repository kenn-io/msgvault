//go:build unix

package signalexport

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func init() {
	nonregularInputs = append(nonregularInputs, struct {
		name  string
		check func(*testing.T, string, string)
	}{"fifo", func(t *testing.T, root, name string) {
		t.Helper()
		if childRoot := os.Getenv("SIGNAL_TEST_NONREGULAR_ROOT"); childRoot != "" {
			r, err := openReader(t.Context(), childRoot)
			if r != nil {
				require.NoError(t, r.Close())
			}
			require.ErrorContains(t, err, os.Getenv("SIGNAL_TEST_NONREGULAR_NAME"))
			require.ErrorContains(t, err, "regular file")
			return
		}
		path := filepath.Join(root, name)
		require.NoError(t, os.Remove(path))
		require.NoError(t, syscall.Mkfifo(path, 0600))
		// Isolate the blocking-open regression so its failure cannot hang the suite.
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		executable, err := os.Executable()
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestReaderRejectsNonregularInput$/"+name+"/fifo$")
		cmd.Env = append(os.Environ(), "SIGNAL_TEST_NONREGULAR_ROOT="+root, "SIGNAL_TEST_NONREGULAR_NAME="+name)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}})
}
