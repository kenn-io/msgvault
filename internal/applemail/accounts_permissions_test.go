package applemail

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestV10AccountDir_PartiallyReadableNewest(t *testing.T) {
	require := require.New(t)
	mailDir := t.TempDir()
	guid := "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	for _, version := range []string{"V9", "V10"} {
		writeTestEmlx(t, filepath.Join(mailDir, version, guid, "Inbox.mbox", "Messages"), "2.emlx")
	}
	newest := filepath.Join(mailDir, "V10", guid)
	blocked := filepath.Join(newest, "Blocked.mbox", "Messages")
	require.NoError(os.MkdirAll(blocked, 0700))
	require.NoError(os.Chmod(blocked, 0))
	t.Cleanup(func() { require.NoError(os.Chmod(blocked, 0700)) })
	if _, err := os.ReadDir(blocked); err == nil {
		t.Skip("requires a user subject to filesystem permissions")
	}
	got, err := V10AccountDir(mailDir, guid)
	require.NoError(err)
	assert.Equal(t, newest, got)
}

func TestV10AccountDir_UnreadableNewest(t *testing.T) {
	require := require.New(t)
	mailDir := t.TempDir()
	guid := "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	for _, version := range []string{"V9", "V10"} {
		writeTestEmlx(t, filepath.Join(mailDir, version, guid, "Inbox.mbox", "Messages"), "2.emlx")
	}
	newest := filepath.Join(mailDir, "V10", guid)
	blocked := filepath.Join(newest, "Inbox.mbox", "Messages")
	require.NoError(os.Chmod(blocked, 0))
	t.Cleanup(func() { require.NoError(os.Chmod(blocked, 0700)) })
	if _, err := os.ReadDir(blocked); err == nil {
		t.Skip("requires a user subject to filesystem permissions")
	}
	got, err := V10AccountDir(mailDir, guid)
	require.NoError(err)
	assert.Equal(t, newest, got)
}
