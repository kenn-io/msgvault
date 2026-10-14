//go:build linux || darwin

package emlx

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmlxFingerprintSiblingChanges(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	r.NoError(err)
	p := filepath.Join(root, "Messages", "1.partial.emlx")
	r.NoError(os.MkdirAll(filepath.Dir(p), 0700))
	r.NoError(os.WriteFile(p, []byte("synthetic"), 0600))
	first, ok, err := Fingerprint(t.Context(), p)
	r.NoError(err)
	r.True(ok)
	same, ok, err := Fingerprint(t.Context(), p)
	r.NoError(err)
	r.True(ok)
	a.Equal(first, same)
	att := filepath.Join(root, "Attachments", "1", "2", "part.bin")
	r.NoError(os.MkdirAll(filepath.Dir(att), 0700))
	r.NoError(os.WriteFile(att, []byte("one"), 0600))
	added, ok, err := Fingerprint(t.Context(), p)
	r.NoError(err)
	r.True(ok)
	a.NotEqual(first, added)
	r.NoError(os.WriteFile(att, []byte("changed"), 0600))
	changed, ok, err := Fingerprint(t.Context(), p)
	r.NoError(err)
	r.True(ok)
	a.NotEqual(added, changed)
	r.NoError(os.Remove(att))
	removed, ok, err := Fingerprint(t.Context(), p)
	r.NoError(err)
	r.True(ok)
	a.NotEqual(changed, removed)
	// Changes in another message's dependency tree must not change this signature.
	sibling := filepath.Join(root, "Attachments", "3", "2", "part.bin")
	r.NoError(os.MkdirAll(filepath.Dir(sibling), 0700))
	r.NoError(os.WriteFile(sibling, []byte("other"), 0600))
	after, ok, err := Fingerprint(t.Context(), p)
	r.NoError(err)
	r.True(ok)
	a.Equal(removed, after)
}

func TestEmlxFingerprintLinksAndCancellation(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	r.NoError(err)
	p := filepath.Join(root, "Messages", "1.partial.emlx")
	r.NoError(os.MkdirAll(filepath.Dir(p), 0700))
	r.NoError(os.WriteFile(p, []byte("synthetic"), 0600))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = Fingerprint(ctx, p)
	r.ErrorIs(err, context.Canceled)
	r.NoError(os.Symlink(filepath.Dir(p), filepath.Join(root, "Linked")))
	_, ok, err := Fingerprint(t.Context(), filepath.Join(root, "Linked", "1.partial.emlx"))
	r.NoError(err)
	a.False(ok)
	r.NoError(os.Symlink(t.TempDir(), filepath.Join(root, "Attachments")))
	_, ok, err = Fingerprint(t.Context(), p)
	r.NoError(err)
	a.False(ok)
}
