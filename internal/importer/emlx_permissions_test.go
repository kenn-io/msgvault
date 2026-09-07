package importer

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func TestImportEmlxDir_DiscoveryPermissions(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "no readable mailboxes"
		if partial {
			name = "partial import"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, tmp := openTestStore(t)
			root := filepath.Join(tmp, "Mail")
			blocked := filepath.Join(root, "Blocked.mbox", "Messages")
			require.NoError(os.MkdirAll(blocked, 0700))
			require.NoError(os.Chmod(blocked, 0))
			t.Cleanup(func() { require.NoError(os.Chmod(blocked, 0700)) })
			if _, err := os.ReadDir(blocked); err == nil {
				t.Skip("requires a user subject to filesystem permissions")
			}
			if partial {
				mkMailboxDir(t, filepath.Join(root, "Readable.mbox"), map[string][]byte{
					"1.emlx": email.NewMessage().From("user@example.com").
						To("other@example.com").Subject("Permission fixture").
						Body("Synthetic message.").Bytes(),
				})
			}
			var logs bytes.Buffer
			summary, err := ImportEmlxDir(context.Background(), st, root, EmlxImportOptions{
				Identifier: "user@example.com",
				Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
			})
			if !partial {
				require.ErrorIs(err, os.ErrPermission)
				assert.Nil(summary)
				return
			}
			require.NoError(err)
			require.NotNil(summary)
			assert.Equal(int64(1), summary.MessagesAdded)
			assert.Equal(int64(1), summary.Errors)
			assert.False(summary.HardErrors)
			assert.Contains(logs.String(), "WARN")
			assert.Contains(logs.String(), blocked)
			var errorsCount int64
			require.NoError(st.DB().QueryRow("SELECT errors_count FROM sync_runs WHERE source_id = ?", summary.SourceID).Scan(&errorsCount))
			assert.Equal(int64(1), errorsCount)
		})
	}
}

func TestImportEmlxDir_DiscoveryErrorsOnResume(t *testing.T) {
	for _, initiallyDenied := range []bool{false, true} {
		name := "new denial on resume"
		if initiallyDenied {
			name = "repeated denial"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, tmp := openTestStore(t)
			root := filepath.Join(tmp, "Mail")
			blocked := filepath.Join(root, "Blocked.mbox", "Messages")
			require.NoError(os.MkdirAll(blocked, 0700))
			t.Cleanup(func() { require.NoError(os.Chmod(blocked, 0700)) })
			deny := func() {
				t.Helper()
				require.NoError(os.Chmod(blocked, 0))
				if _, err := os.ReadDir(blocked); err == nil {
					t.Skip("requires a user subject to filesystem permissions")
				}
			}
			if initiallyDenied {
				deny()
			}
			mkMailboxDir(t, filepath.Join(root, "Readable.mbox"), map[string][]byte{
				"1.emlx": email.NewMessage().From("user@example.com").
					Subject("Resume fixture").Body("Synthetic message.").Bytes(),
			})
			opts := EmlxImportOptions{Identifier: "user@example.com"}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			first, err := ImportEmlxDir(ctx, st, root, opts)
			require.NoError(err)
			require.NotNil(first)
			var firstErrors int64
			if initiallyDenied {
				firstErrors = 1
			}
			assert.Equal(firstErrors, first.Errors)
			deny()

			resumed, err := ImportEmlxDir(context.Background(), st, root, opts)
			require.NoError(err)
			require.NotNil(resumed)
			assert.True(resumed.WasResumed)
			assert.Equal(int64(1), resumed.MessagesAdded)
			// The user sees one failure in this invocation, whether the same
			// path failed before or permissions changed during the interruption.
			assert.Equal(int64(1), resumed.Errors)
			var totalErrors int64
			require.NoError(st.DB().QueryRow("SELECT errors_count FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1", resumed.SourceID).Scan(&totalErrors))
			// Persistent progress includes failed attempts, like ingestion and
			// checkpoint errors, rather than unique paths across invocations.
			assert.Equal(firstErrors+1, totalErrors)
		})
	}
}
