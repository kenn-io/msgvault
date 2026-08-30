package eml

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverMailboxesFindsNestedEMLFilesDeterministically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	aDir := filepath.Join(root, "A.mailbox")
	bDir := filepath.Join(aDir, "B.mailbox")
	require.NoError(os.MkdirAll(bDir, 0o700))
	require.NoError(os.WriteFile(filepath.Join(aDir, "2.eml"), []byte("two"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(aDir, "1.EML"), []byte("one"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(aDir, "ignored.txt"), []byte("ignored"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(bDir, "3.eml"), []byte("three"), 0o600))

	mailboxes, err := DiscoverMailboxes(root)
	require.NoError(err)
	require.Len(mailboxes, 2)

	assert.Equal("A", mailboxes[0].Label)
	assert.Equal(aDir, mailboxes[0].Path)
	assert.Equal([]string{
		filepath.Join(aDir, "1.EML"),
		filepath.Join(aDir, "2.eml"),
	}, mailboxes[0].Files)
	assert.Equal("A/B", mailboxes[1].Label)
	assert.Equal(bDir, mailboxes[1].Path)
	assert.Equal([]string{filepath.Join(bDir, "3.eml")}, mailboxes[1].Files)
}

func TestDiscoverMailboxesAcceptsMailboxRoot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "Inbox.mailbox")
	require.NoError(os.Mkdir(root, 0o700))
	require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), []byte("message"), 0o600))

	mailboxes, err := DiscoverMailboxes(root)
	require.NoError(err)
	require.Len(mailboxes, 1)
	assert.Equal("Inbox", mailboxes[0].Label)
	assert.Equal([]string{filepath.Join(root, "message.eml")}, mailboxes[0].Files)
}

func TestDiscoverMailboxesRejectsTreeWithoutEML(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "Empty.mailbox"), 0o700))

	mailboxes, err := DiscoverMailboxes(root)

	assert.Nil(t, mailboxes)
	assert.ErrorContains(t, err, "no .eml files found")
}
