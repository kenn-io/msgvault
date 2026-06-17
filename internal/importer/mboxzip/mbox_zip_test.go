package mboxzip

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

const validMbox = "From sender@example.com Mon Jan 1 12:00:00 2024\n" +
	"From: sender@example.com\n" +
	"Subject: Test\n" +
	"\n" +
	"Body.\n"

func TestResolveMboxExport_NoExtension(t *testing.T) {
	require := requirepkg.New(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "maildata")
	require.NoError(os.WriteFile(p, []byte(validMbox), 0600))

	files, err := ResolveMboxExport(p, dir, slog.Default())
	require.NoError(err)
	require.Len(files, 1)
	abs, _ := filepath.Abs(p)
	assertpkg.Equal(t, abs, files[0])
}

func TestResolveMboxExport_NonStandardExtension(t *testing.T) {
	require := requirepkg.New(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "archive.mail")
	require.NoError(os.WriteFile(p, []byte(validMbox), 0600))

	files, err := ResolveMboxExport(p, dir, slog.Default())
	require.NoError(err)
	require.Len(files, 1)
	abs, _ := filepath.Abs(p)
	assertpkg.Equal(t, abs, files[0])
}

func TestResolveMboxExport_StandardExtensionsStillWork(t *testing.T) {
	for _, ext := range []string{".mbox", ".mbx"} {
		t.Run(ext, func(t *testing.T) {
			require := requirepkg.New(t)
			dir := t.TempDir()
			p := filepath.Join(dir, "archive"+ext)
			require.NoError(os.WriteFile(p, []byte(validMbox), 0600))

			files, err := ResolveMboxExport(p, dir, slog.Default())
			require.NoError(err)
			require.Len(files, 1)
			abs, _ := filepath.Abs(p)
			assertpkg.Equal(t, abs, files[0])
		})
	}
}
