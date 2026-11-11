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

func TestReaderRejectsNonregularInput(t *testing.T) {
	if root := os.Getenv("SIGNAL_TEST_NONREGULAR_ROOT"); root != "" {
		r, err := openReader(t.Context(), root)
		if r != nil {
			require.NoError(t, r.Close())
		}
		require.ErrorContains(t, err, os.Getenv("SIGNAL_TEST_NONREGULAR_NAME"))
		require.ErrorContains(t, err, "regular file")
		return
	}
	for _, name := range []string{"metadata.json", "main.jsonl"} {
		for _, kind := range []string{"directory", "fifo"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				require := require.New(t)
				root := fixture(t)
				path := filepath.Join(root, name)
				require.NoError(os.Remove(path))
				if kind == "directory" {
					require.NoError(os.Mkdir(path, 0700))
				} else {
					require.NoError(syscall.Mkfifo(path, 0600))
				}
				// Isolate the blocking-open regression so its failure cannot hang the suite.
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				executable, err := os.Executable()
				require.NoError(err)
				cmd := exec.CommandContext(ctx, executable, "-test.run=^TestReaderRejectsNonregularInput$")
				cmd.Env = append(os.Environ(), "SIGNAL_TEST_NONREGULAR_ROOT="+root, "SIGNAL_TEST_NONREGULAR_NAME="+name)
				output, err := cmd.CombinedOutput()
				require.NoError(err, "%s", output)
			})
		}
	}
}
