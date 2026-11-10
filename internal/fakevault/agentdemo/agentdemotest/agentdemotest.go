// Package agentdemotest supplies private copies of the synthetic agent demo.
package agentdemotest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/fakevault/agentdemo"
	"go.kenn.io/msgvault/internal/store"
)

var template struct {
	once   sync.Once
	builds int
	files  map[string][]byte
	err    error
}

func build(ctx context.Context) (files map[string][]byte, err error) {
	dir, err := os.MkdirTemp("", "msgvault-agent-demo-")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	if _, err = agentdemo.GenerateDir(ctx, agentdemo.Options{Dir: dir, Seed: 1}); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	files = map[string][]byte{}
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := root.ReadFile(path)
		if err != nil {
			return err
		}
		if path == "msgvault.db-wal" && len(data) > 0 {
			return errors.New("demo WAL not folded into database")
		}
		files[path] = data
		return nil
	})
	return files, err
}

// Archive builds once per test binary and copies every file into a private directory.
func Archive(tb testing.TB) (string, *agentdemo.Manifest) {
	tb.Helper()
	template.once.Do(func() {
		template.builds++
		template.files, template.err = build(tb.Context())
	})
	require.NoError(tb, template.err, "build agent demo")
	dir := tb.TempDir()
	for rel, data := range template.files {
		path := filepath.Join(dir, rel)
		require.NoError(tb, os.MkdirAll(filepath.Dir(path), 0700), "copy demo directory")
		require.NoError(tb, os.WriteFile(path, data, 0600), "copy demo file")
	}
	m, err := agentdemo.Load(dir)
	require.NoError(tb, err, "load demo manifest")
	return dir, m
}

// Store opens a private copy and closes it when the test finishes.
func Store(tb testing.TB) (*store.Store, *agentdemo.Manifest) {
	tb.Helper()
	dir, m := Archive(tb)
	st, err := store.OpenForTest(tb.Context(), filepath.Join(dir, "msgvault.db"))
	require.NoError(tb, err, "open demo store")
	tb.Cleanup(func() { require.NoError(tb, st.Close(), "close demo store") })
	return st, m
}
func Now(m *agentdemo.Manifest) time.Time { return m.AsOf }
