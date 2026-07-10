package packer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil"
)

func TestRunRetriesLooseOrphanRemoval(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	content := []byte("unreferenced loose cleanup retry")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	path := filepath.Join(dir, hash[:2], hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(os.WriteFile(path, content, 0o600))

	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	oldRemove := removeLooseFile
	removeLooseFile = func(candidate string) error {
		if candidate == path {
			return errors.New("injected remove failure")
		}
		return oldRemove(candidate)
	}
	t.Cleanup(func() { removeLooseFile = oldRemove })

	first, err := Run(context.Background(), st, dir, Options{})
	require.NoError(err)
	assert.Zero(first.LooseOrphansRemoved)
	assert.FileExists(path)
	assert.Contains(logs.String(), "injected remove failure")

	removeLooseFile = oldRemove
	second, err := Run(context.Background(), st, dir, Options{})
	require.NoError(err)
	assert.Equal(1, second.LooseOrphansRemoved)
	assert.NoFileExists(path)
}
