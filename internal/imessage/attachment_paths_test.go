package imessage

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Removing confinement or using transfer paths directly exposes outside bytes.
func TestAttachmentPaths(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "voice.caf"), []byte("inside"), 0600))
	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	for _, tc := range []struct {
		path          string
		rebase, valid bool
	}{
		{"voice.caf", false, true}, {filepath.Join(root, "voice.caf"), false, true},
		{"~/Library/Messages/Attachments/voice.caf", true, true},
		{"/Users/example/Library/Messages/Attachments/voice.caf", true, true},
		{"../secret", false, false}, {outside, false, false}, {"escape", false, false},
		{"~other/voice.caf", true, false}, {"https://example.test/voice.caf", true, false},
		{"~/Library/Messages/Attachments/../secret", true, false}, {".", false, false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, err := openAttachment(root, tc.path, tc.rebase)
			if !tc.valid {
				require.Error(err)
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

// Any accepted database path must read only the selected tree's bytes.
func FuzzAttachmentPathConfinement(f *testing.F) {
	root := f.TempDir()
	require.NoError(f, os.WriteFile(filepath.Join(root, "voice"), []byte("inside"), 0600))
	outside := filepath.Join(f.TempDir(), "outside")
	require.NoError(f, os.WriteFile(outside, []byte("outside"), 0600))
	require.NoError(f, os.Symlink(outside, filepath.Join(root, "link")))
	for _, p := range []string{"voice", "../outside", outside, "link", "~/Library/Messages/Attachments/voice", "\x00"} {
		f.Add(p, true)
	}
	f.Fuzz(func(t *testing.T, p string, rebase bool) {
		file, err := openAttachment(root, p, rebase)
		if err != nil {
			return
		}
		defer func() { _ = file.Close() }()
		b, err := io.ReadAll(file)
		require.NoError(t, err)
		assert.Equal(t, "inside", string(b))
	})
}
