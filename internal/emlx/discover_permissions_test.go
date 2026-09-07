package emlx

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func denyDirectory(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.Chmod(path, 0))
	t.Cleanup(func() { require.NoError(t, os.Chmod(path, 0700)) })
	if _, err := os.ReadDir(path); err == nil {
		t.Skip("requires a user subject to filesystem permissions")
	}
}

func TestDiscoverMailboxes_UnreadableRoot(t *testing.T) {
	for _, name := range []string{"Mail", "Inbox.mbox"} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), name)
			mkMailbox(t, root, "1.emlx")
			denyDirectory(t, root)
			mailboxes, err := DiscoverMailboxes(root)
			require.ErrorIs(t, err, os.ErrPermission)
			assert.Empty(t, mailboxes)
		})
	}
}

func TestDiscoverMailboxes_UnreadableDescendant(t *testing.T) {
	for _, denied := range []string{
		"Blocked", "Blocked.mbox", "Blocked.mbox/Messages",
		"Blocked.mbox/Container", "Blocked.mbox/Container/Data",
		"Blocked.mbox/Container/Data/Messages",
		"Blocked.mbox/Container/Data/0",
		"Blocked.mbox/Container/Data/0/Messages",
	} {
		for _, readable := range []bool{false, true} {
			name := denied + "/alone"
			if readable {
				name = denied + "/partial"
			}
			t.Run(name, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				root := t.TempDir()
				blocked := filepath.Join(root, filepath.FromSlash(denied))
				require.NoError(os.MkdirAll(blocked, 0700))
				if readable {
					mkMailbox(t, filepath.Join(root, "Readable.mbox"), "1.emlx")
				}
				denyDirectory(t, blocked)
				mailboxes, err := DiscoverMailboxes(root)
				require.ErrorIs(err, os.ErrPermission)
				if readable {
					require.Len(mailboxes, 1)
					assert.Equal("Readable", mailboxes[0].Label)
				} else {
					assert.Empty(mailboxes)
				}
			})
		}
	}
}

func TestDiscoverMailboxes_UnreadablePartitionPreservesFiles(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "Inbox.mbox")
	mkV10Mailbox(t, root, "1.emlx")
	partition := filepath.Join(root, testMailboxGUID, "Data", "0", "Messages")
	require.NoError(os.MkdirAll(partition, 0700))
	denyDirectory(t, partition)
	mailboxes, err := DiscoverMailboxes(root)
	require.ErrorIs(err, os.ErrPermission)
	require.Len(mailboxes, 1)
	assert.Len(t, mailboxes[0].Files, 1)
}
