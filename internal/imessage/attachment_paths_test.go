package imessage

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Removing confinement or using transfer paths directly exposes outside bytes. Every
// refused target exists, so only the path checks can be what refuses it.
func TestAttachmentPaths(t *testing.T) {
	setup := require.New(t)
	root := t.TempDir()
	setup.NoError(os.MkdirAll(filepath.Join(root, "Attachments"), 0700))
	setup.NoError(os.MkdirAll(filepath.Join(root, "StickerCache"), 0700))
	setup.NoError(os.WriteFile(filepath.Join(root, "Attachments", "voice.caf"), []byte("inside"), 0600))
	setup.NoError(os.WriteFile(filepath.Join(root, "StickerCache", "s.png"), []byte("inside"), 0600))
	setup.NoError(os.WriteFile(filepath.Join(root, "chat.db"), []byte("inside"), 0600))
	setup.NoError(os.WriteFile(filepath.Join(root, "private.txt"), []byte("private"), 0600))
	setup.NoError(os.WriteFile(filepath.Join(filepath.Dir(root), "secret"), []byte("outside"), 0600))
	t.Cleanup(func() { _ = os.Remove(filepath.Join(filepath.Dir(root), "secret")) })
	outside := filepath.Join(filepath.Dir(root), "secret")
	setup.NoError(os.Symlink(outside, filepath.Join(root, "Attachments", "escape")))
	setup.NoError(os.Symlink("../private.txt", filepath.Join(root, "Attachments", "link")))
	for _, tc := range []struct {
		path          string
		rebase, valid bool
		invalid       bool // refused by the plain path check instead of the root check
	}{
		{"Attachments/voice.caf", false, true, false}, {filepath.Join(root, "Attachments", "voice.caf"), false, true, false},
		{"StickerCache/s.png", false, true, false},
		{"~/Library/Messages/Attachments/voice.caf", true, true, false},
		{"/Users/example/Library/Messages/Attachments/voice.caf", true, true, false},
		{"voice.caf", false, false, false}, {"chat.db", false, false, false}, {"~/Library/Messages/chat.db", true, false, false},
		{"/Users/example/Library/Messages/chat.db", true, false, false},
		{"Attachments/../chat.db", false, false, false},
		{outside, false, false, false}, {"../secret", false, false, false},
		{"Attachments/escape", false, false, false}, {"Attachments/link", false, false, false},
		{"~other/voice.caf", true, false, false}, {".", false, false, false},
		{"https://example.test/voice.caf", true, false, true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, err := openAttachment(root, tc.path, tc.rebase)
			if !tc.valid {
				require.Error(err)
				if !tc.invalid {
					require.ErrorIs(err, errOutsideAttachmentRoot)
				}
				return
			}
			require.NoError(err)
			defer func() { _ = f.Close() }()
			b, err := io.ReadAll(f)
			require.NoError(err)
			assert.Equal("inside", string(b))
		})
	}
}

// A copied Messages folder must not redirect a media folder elsewhere, while a user's own
// Messages folder may keep its Attachments folder on another drive.
func TestAttachmentPathsFollowSymlinkedMediaFolderOnlyForTheDefaultRoot(t *testing.T) {
	setup := require.New(t)
	root, elsewhere := t.TempDir(), t.TempDir()
	setup.NoError(os.WriteFile(filepath.Join(elsewhere, "voice.caf"), []byte("inside"), 0600))
	setup.NoError(os.Symlink(elsewhere, filepath.Join(root, "Attachments")))

	f, err := openAttachment(root, "Attachments/voice.caf", false)
	setup.NoError(err)
	_ = f.Close()

	_, err = openAttachment(root, "~/Library/Messages/Attachments/voice.caf", true)
	require.ErrorIs(t, err, errOutsideAttachmentRoot)
}
