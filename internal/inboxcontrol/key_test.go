package inboxcontrol_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxPreviewKeyPersistsAcrossStartup(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	path := filepath.Join(t.TempDir(), "daemon", "inbox-preview.key")
	_, err := inboxcontrol.LoadPreviewKey(path)
	require.Error(t, err)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "preview must not create a missing key")
	first, err := inboxcontrol.EnsurePreviewKey(path)
	requirements.NoError(err)
	assertions.Len(first, 32)
	second, err := inboxcontrol.EnsurePreviewKey(path)
	requirements.NoError(err)
	assertions.Equal(first, second)
	loaded, err := inboxcontrol.LoadPreviewKey(path)
	requirements.NoError(err)
	assertions.Equal(first, loaded)
	info, err := os.Stat(path)
	requirements.NoError(err)
	if runtime.GOOS != "windows" {
		assertions.Equal(os.FileMode(0600), info.Mode().Perm())
	}
}

func TestInboxPreviewKeyRefusesInvalidExistingFiles(t *testing.T) {
	for _, shape := range []string{"short", "long", "directory", "symlink", "public"} {
		t.Run(shape, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			if runtime.GOOS == "windows" && (shape == "symlink" || shape == "public") {
				t.Skip("Unix filesystem policy")
			}
			path := filepath.Join(t.TempDir(), "inbox-preview.key")
			switch shape {
			case "short":
				requirements.NoError(os.WriteFile(path, []byte("short"), 0600))
			case "long":
				requirements.NoError(os.WriteFile(path, make([]byte, 33), 0600))
			case "directory":
				requirements.NoError(os.Mkdir(path, 0700))
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				requirements.NoError(os.WriteFile(target, make([]byte, 32), 0600))
				requirements.NoError(os.Symlink(target, path))
			case "public":
				requirements.NoError(os.WriteFile(path, make([]byte, 32), 0644))
			}
			_, err := inboxcontrol.EnsurePreviewKey(path)
			require.Error(t, err, "startup must not replace existing evidence")
			_, err = inboxcontrol.LoadPreviewKey(path)
			assertions.Error(err)
		})
	}
}
