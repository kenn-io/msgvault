package tumblr

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func zipFixture(t *testing.T, root, wrapper string) string {
	t.Helper()
	var entries []testutil.ArchiveEntry
	require.NoError(t, filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
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
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		entries = append(entries, testutil.ArchiveEntry{Name: wrapper + filepath.ToSlash(rel), Content: string(data)})
		return nil
	}))
	filename := filepath.Join(t.TempDir(), "export.zip")
	testutil.CreateZip(t, filename, entries)
	return filename
}

func TestTumblrReaderParity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	data, err := os.ReadFile("testdata/observed-shape/conversations/convo_700.html")
	require.NoError(err)
	file := testutil.CreateTempZip(t, map[string]string{"blog/conversations/a.html": string(data), "../escape": "ignored", "__MACOSX/conversations/metadata": "ignored"})
	wrapper := t.TempDir()
	require.NoError(os.CopyFS(filepath.Join(wrapper, "blog"), os.DirFS("testdata/observed-shape")))
	for _, name := range []string{"testdata/observed-shape", zipFixture(t, "testdata/observed-shape", ""), file, wrapper} {
		r, err := openExport(t.Context(), name)
		require.NoError(err)
		require.Len(r.files, 1)
		c, err := r.conversation(t.Context(), r.files[0])
		require.NoError(err)
		assert.Len(c.Messages, 4)
		require.NoError(r.close())
	}
	require.NoError(os.MkdirAll(filepath.Join(wrapper, "other", "conversations"), 0700))
	_, err = openExport(t.Context(), wrapper)
	require.ErrorContains(err, "expected one root")
}

func TestTumblrSymlinkAndBounds(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "conversations"), 0700))
	outside := filepath.Join(t.TempDir(), "outside.html")
	require.NoError(os.WriteFile(outside, []byte("<h2>Conversation ID: escaped</h2>"), 0600))
	require.NoError(os.Symlink(outside, filepath.Join(root, "conversations", "link.html")))
	r, err := openExport(t.Context(), root)
	require.NoError(err)
	_, err = r.conversation(t.Context(), "conversations/link.html")
	require.ErrorContains(err, "path escapes from parent")
	require.NoError(r.close())
	require.NoError(os.Remove(filepath.Join(root, "conversations", "link.html")))
	f, err := os.Create(filepath.Join(root, "conversations", "large.html"))
	require.NoError(err)
	require.NoError(f.Truncate(maxHTMLBytes + 1))
	require.NoError(f.Close())
	r, err = openExport(t.Context(), root)
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
	r, err := openExport(t.Context(), file)
	require.NoError(err)
	defer func() { require.NoError(r.close()) }()
	_, err = r.conversation(t.Context(), "conversations/link.html")
	require.ErrorContains(err, "nonregular")
}

func TestTumblrDiscoveryLimits(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "conversations"), 0700))
	require.NoError(os.WriteFile(filepath.Join(root, "conversations", "a.html"), nil, 0600))
	for _, input := range []string{root, zipFixture(t, root, "")} {
		for _, tc := range []struct {
			entries, bytes int
			ok             bool
		}{{1, 20, true}, {0, 100, false}, {1, 19, false}} {
			r, err := openExport(t.Context(), input)
			require.NoError(err)
			r.files = nil
			err = r.discover(t.Context(), tc.entries, tc.bytes)
			if tc.ok {
				require.NoError(err)
			} else {
				require.ErrorContains(err, "metadata limit")
			}
			require.NoError(r.close())
		}
	}
}

func TestTumblrZIPPreflight(t *testing.T) {
	checks := require.New(t)
	file := testutil.CreateTempZip(t, map[string]string{"conversations/a.html": ""})
	original, err := os.ReadFile(file)
	checks.NoError(err)
	footer := len(original) - 22
	for _, tc := range []struct {
		name         string
		count, bytes int
		mutate       func([]byte)
		ok           bool
	}{
		{"exact", 1, 66, nil, true},
		{"count limit", 0, 1000, nil, false},
		{"metadata limit", 1, 64, nil, false},
		{"declared count", 1, 1000, func(b []byte) { binary.LittleEndian.PutUint16(b[footer+10:], 2) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := bytes.Clone(original)
			if tc.mutate != nil {
				tc.mutate(b)
			}
			err := checkZIP(t.Context(), bytes.NewReader(b), int64(len(b)), tc.count, tc.bytes)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "ZIP metadata limit exceeded")
			}
		})
	}
	// A prefix and a long comment use the same directory as archive/zip.
	var b bytes.Buffer
	b.WriteString("prefix")
	z := zip.NewWriter(&b)
	_, err = z.Create("conversations/a.html")
	checks.NoError(err)
	checks.NoError(z.SetComment(strings.Repeat("comment", 300)))
	checks.NoError(z.Close())
	checks.NoError(checkZIP(t.Context(), bytes.NewReader(b.Bytes()), int64(b.Len()), 1, 1000))
	_, err = zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	checks.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	checks.ErrorIs(checkZIP(ctx, bytes.NewReader(original), int64(len(original)), 1, 1000), context.Canceled)
}
