//go:build unix

package signalexport

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReaderRejectsFIFO(t *testing.T) {
	for _, name := range []string{"metadata.json", "main.jsonl"} {
		t.Run(name, func(t *testing.T) {
			root := fixture(t)
			path := filepath.Join(root, name)
			require.NoError(t, os.Remove(path))
			require.NoError(t, syscall.Mkfifo(path, 0600))
			done := make(chan error, 1)
			go func() {
				r, err := openReader(t.Context(), root)
				if r != nil {
					err = errors.Join(err, r.Close())
				}
				done <- err
			}()
			select {
			case err := <-done:
				require.ErrorContains(t, err, name)
				require.ErrorContains(t, err, "regular file")
			case <-time.After(10 * time.Second):
				require.FailNow(t, "opening the FIFO blocked")
			}
		})
	}
}
