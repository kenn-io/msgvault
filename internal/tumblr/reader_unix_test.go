//go:build unix

package tumblr

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/sys/unix"
)

func TestTumblrFIFORejectedBeforeOpen(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.Mkdir(filepath.Join(root, "conversations"), 0700))
	fifo := filepath.Join(root, "conversations", "pipe.html")
	require.NoError(unix.Mkfifo(fifo, 0600))
	r, err := openExport(t.Context(), root)
	require.NoError(err)
	defer func() { require.NoError(r.close()) }()
	done := make(chan [2]error, 1)
	s := &ImportSummary{}
	st := testutil.NewTestStore(t)
	opts := ImportOptions{AttachmentsDir: t.TempDir()}
	go func() {
		_, conversationErr := r.conversation(t.Context(), "conversations/pipe.html")
		_, _, mediaErr := mediaWrites(t.Context(), st, r, 0, "conversations/c.html", &Message{Media: []string{"pipe.html"}}, opts, s)
		done <- [2]error{conversationErr, mediaErr}
	}()
	var result [2]error
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		// Release a regressed blocking open, and keep the writer until both reads end.
		fd, err := unix.Open(fifo, unix.O_RDWR|unix.O_NONBLOCK, 0600)
		require.NoError(err)
		defer func() { require.NoError(unix.Close(fd)) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			require.FailNow("FIFO reader did not finish after unblocking")
		}
		require.FailNow("FIFO opened before nonregular rejection")
	}
	require.ErrorIs(result[0], errNonregular)
	require.NoError(result[1])
	require.Equal(1, s.MediaInvalid)
}
