package maildir

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeMailbox(t *testing.T, root string) {
	t.Helper()
	for _, dir := range []string{"cur", "new", "tmp"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0700))
	}
}

func TestDiscoverReadsDeliveredMessagesAndNestedFolders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	makeMailbox(t, root)
	makeMailbox(t, filepath.Join(root, ".Projects.Go"))
	makeMailbox(t, filepath.Join(root, "Archive", "2024"))
	for _, file := range []string{
		"cur/one", "new/two", "tmp/incomplete", ".Projects.Go/cur/three", "Archive/2024/new/four",
		"dovecot-uidlist", "cur/.DS_Store", "new/.nfs0001",
	} {
		require.NoError(os.WriteFile(filepath.Join(root, file), []byte("Subject: test\r\n\r\nbody"), 0600))
	}
	boxes, err := Discover(root)
	require.NoError(err)
	require.Len(boxes, 3)
	labels := map[string]int{}
	for _, box := range boxes {
		labels[box.Label] = len(box.Files)
	}
	assert.Equal(map[string]int{"INBOX": 2, "Projects/Go": 1, "Archive/2024": 1}, labels)
}

func TestDiscoverRejectsNonMaildirAndDoesNotFollowSymlinks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	_, err := Discover(root)
	require.Error(err)
	makeMailbox(t, root)
	outside := t.TempDir()
	makeMailbox(t, outside)
	require.NoError(os.WriteFile(filepath.Join(outside, "new", "secret"), []byte("private"), 0600))
	require.NoError(os.Symlink(outside, filepath.Join(root, ".Linked")))
	require.NoError(os.Symlink(filepath.Join(outside, "new", "secret"), filepath.Join(root, "cur", "link")))
	boxes, err := Discover(root)
	require.NoError(err)
	require.Len(boxes, 1)
	assert.Empty(boxes[0].Files)
}

func TestFlags(t *testing.T) {
	for _, tt := range []struct {
		path string
		want []string
	}{
		{"new/one", []string{"UNREAD"}},
		{"cur/one:2,S", nil},
		{"cur/one:2,DFPRST", []string{"DRAFT", "STARRED", "PASSED", "REPLIED", "TRASH"}},
		{"cur/one:2,FX", []string{"UNREAD", "STARRED"}},
	} {
		t.Run(tt.path, func(t *testing.T) { assert.Equal(t, tt.want, Flags(tt.path)) })
	}
}

func TestDiscoverDoesNotDescendIntoMessageOrTemporaryDirectories(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	makeMailbox(t, root)
	for _, dir := range []string{"cur/nested", "new/nested", "tmp/.Hidden"} {
		makeMailbox(t, filepath.Join(root, dir))
		require.NoError(os.WriteFile(filepath.Join(root, dir, "new/message"), []byte("body"), 0600))
	}
	boxes, err := Discover(root)
	require.NoError(err)
	require.Len(boxes, 1)
	assert.Empty(boxes[0].Files)
}
