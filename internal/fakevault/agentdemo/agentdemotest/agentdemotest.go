// Package agentdemotest supplies private copies of the synthetic agent demo.
package agentdemotest

import (
	"context"
	"fmt"
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

func build(ctx context.Context) (map[string][]byte, error) {
	dir, err := os.MkdirTemp("", "msgvault-agent-demo-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if _, err = agentdemo.GenerateDir(ctx, agentdemo.Options{Dir: dir, Seed: 1}); err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if rel == "msgvault.db-wal" && len(data) > 0 {
			return fmt.Errorf("demo WAL not folded into database")
		}
		files[rel] = data
		return nil
	})
	return files, err
}

// Archive builds once per test binary and copies every file into a private directory.
func Archive(t testing.TB) (string, *agentdemo.Manifest) {
	t.Helper()
	template.once.Do(func() {
		template.builds++
		template.files, template.err = build(t.Context())
	})
	require.NoError(t, template.err, "build agent demo")
	dir := t.TempDir()
	for rel, data := range template.files {
		path := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700), "copy demo directory")
		require.NoError(t, os.WriteFile(path, data, 0600), "copy demo file")
	}
	m, err := agentdemo.Load(dir)
	require.NoError(t, err, "load demo manifest")
	return dir, m
}

// Store opens a private copy and closes it when the test finishes.
func Store(t testing.TB) (*store.Store, *agentdemo.Manifest) {
	t.Helper()
	dir, m := Archive(t)
	st, err := store.OpenForTest(t.Context(), filepath.Join(dir, "msgvault.db"))
	require.NoError(t, err, "open demo store")
	t.Cleanup(func() { require.NoError(t, st.Close(), "close demo store") })
	return st, m
}
func Now(m *agentdemo.Manifest) time.Time { return m.AsOf }
