package imessage

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// SF_DATALESS is synthetic and cannot be set with chflags on a real fixture.
// Exercise the native stat contract without hydrating any real iCloud file.
func TestAttachmentOffloaded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "synthetic")
	require.NoError(t, os.WriteFile(p, []byte("fixture"), 0600))
	info, err := os.Stat(p)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	stat.Flags = unix.UF_HIDDEN
	assert.False(t, attachmentOffloaded(info))
	stat.Flags |= unix.SF_DATALESS
	assert.True(t, attachmentOffloaded(info))
}
