//go:build linux || darwin

package groupme

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportRefusesFIFOWithoutBlocking(t *testing.T) {
	for _, tc := range []struct{ name, fifo, want string }{
		{"messages", "message.json", "message.json must be a regular file"},
		{"metadata", "conversation.json", "conversation.json must be a regular file"},
		{"media", "gallery/fictional-one_800x600.fictional-image.png", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := fixture(t)
			fifo := filepath.Join(root, "group", filepath.FromSlash(tc.fifo))
			if tc.name == "media" {
				require.NoError(os.Mkdir(filepath.Dir(fifo), 0700))
			} else {
				require.NoError(os.Remove(fifo))
			}
			require.NoError(syscall.Mkfifo(fifo, 0600))
			r, err := os.OpenRoot(root)
			require.NoError(err)
			defer func() { assert.NoError(r.Close()) }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type result struct {
				summary *ImportSummary
				err     error
			}
			done := make(chan result, 1)
			go func() {
				if tc.name == "media" {
					summary, err := Import(ctx, st, ImportOptions{RootDir: root, Me: "fictional-owner", AttachmentsDir: filepath.Join(root, "stored-media")})
					done <- result{summary, err}
				} else {
					done <- result{err: discover(ctx, r, root, func(chat) error { return nil })}
				}
			}()
			select {
			case got := <-done:
				if tc.want != "" {
					require.ErrorContains(got.err, tc.want)
				} else {
					require.NoError(got.err)
					assert.Equal(2, got.summary.AttachmentsMissing)
					assert.Zero(got.summary.AttachmentsStored)
				}
			case <-time.After(30 * time.Second):
				cancel()
				// A peer releases a regressed blocking Open; join it before cleanup.
				peer, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0600)
				require.NoError(err)
				<-done
				assert.NoError(peer.Close())
				require.FailNow("blocked opening FIFO")
			}
		})
	}
}
