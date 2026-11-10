package tumblr

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func zipFixture(t *testing.T, root, wrapper string) string {
	t.Helper()
	require := require.New(t)
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	require.NoError(filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		w, err := z.Create(wrapper + filepath.ToSlash(rel))
		if err != nil {
			return fmt.Errorf("create fixture ZIP entry: %w", err)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}))
	require.NoError(z.Close())
	name := filepath.Join(t.TempDir(), "export.zip")
	require.NoError(os.WriteFile(name, b.Bytes(), 0600))
	return name
}
func TestTumblrReaderParity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, name := range []string{"testdata/observed-shape", zipFixture(t, "testdata/observed-shape", ""), zipFixture(t, "testdata/observed-shape", "wrapper/")} {
		r, err := openExport(t.Context(), name)
		require.NoError(err)
		require.Len(r.files, 1)
		c, err := r.conversation(t.Context(), r.files[0])
		require.NoError(err)
		assert.Len(c.Messages, 4)
		require.NoError(r.close())
	}
	wrapper := t.TempDir()
	require.NoError(os.CopyFS(filepath.Join(wrapper, "blog"), os.DirFS("testdata/observed-shape")))
	r, err := openExport(t.Context(), wrapper)
	require.NoError(err)
	require.NoError(r.close())
	require.NoError(os.MkdirAll(filepath.Join(wrapper, "other", "conversations"), 0700))
	_, err = openExport(t.Context(), wrapper)
	require.ErrorContains(err, "expected one root")
}
func TestTumblrUnsafeZIP(t *testing.T) {
	require := require.New(t)
	for _, names := range [][]string{{"../escape"}, {"/absolute"}, {"conversations/a.html", "conversations/a.html"}, {"conversations\\a.html"}, {"conversations/a.html", "conversations/a.html/child"}} {
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		for _, name := range names {
			_, err := z.Create(name)
			require.NoError(err)
		}
		require.NoError(z.Close())
		name := filepath.Join(t.TempDir(), "bad.zip")
		require.NoError(os.WriteFile(name, b.Bytes(), 0600))
		r, err := openExport(t.Context(), name)
		if r != nil {
			require.NoError(r.close())
		}
		require.Error(err)
	}
}
func TestTumblrSymlinkAndBounds(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "conversations"), 0700))
	require.NoError(os.Symlink("outside", filepath.Join(root, "conversations", "link")))
	_, err := openExport(t.Context(), root)
	require.ErrorContains(err, "nonregular")
	require.NoError(os.Remove(filepath.Join(root, "conversations", "link")))
	f, err := os.Create(filepath.Join(root, "conversations", "large.html"))
	require.NoError(err)
	require.NoError(f.Truncate(maxHTMLBytes + 1))
	require.NoError(f.Close())
	r, err := openExport(t.Context(), root)
	require.NoError(err)
	defer func() { require.NoError(r.close()) }()
	_, err = r.conversation(t.Context(), r.files[0])
	require.ErrorIs(err, errHTMLTooLarge)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = openExport(ctx, root)
	require.ErrorIs(err, context.Canceled)
}

func TestTumblrZIPSymlinkRejected(t *testing.T) {
	require := require.New(t)
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	h := &zip.FileHeader{Name: "conversations/link.html"}
	h.SetMode(os.ModeSymlink | 0600)
	w, err := z.CreateHeader(h)
	require.NoError(err)
	_, err = w.Write([]byte("../../outside"))
	require.NoError(err)
	require.NoError(z.Close())
	file := filepath.Join(t.TempDir(), "symlink.zip")
	require.NoError(os.WriteFile(file, b.Bytes(), 0600))
	_, err = openExport(t.Context(), file)
	require.ErrorContains(err, "unsafe")
}
