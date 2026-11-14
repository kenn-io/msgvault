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

func init() {
	nonregularInputs = append(nonregularInputs, struct {
		name  string
		check func(*testing.T, string, string)
	}{"fifo", func(t *testing.T, root, name string) {
		t.Helper()
		path := filepath.Join(root, name)
		require.NoError(t, os.Remove(path))
		require.NoError(t, syscall.Mkfifo(path, 0600))
		ctx := t.Context()
		done := make(chan error, 1)
		go func() {
			r, err := openReader(ctx, root)
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
	}})
}
