package wakeups

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventKeySealsToArchiveAndSubscription(t *testing.T) {
	home := t.TempDir()
	key, err := LoadEventKey(home, false)
	require.NoError(t, err)
	sealed, err := key.Seal("archive", "subscription", []byte("secret"))
	require.NoError(t, err)
	plain, err := key.Open("archive", "subscription", sealed)
	require.NoError(t, err)
	assert.Equal(t, []byte("secret"), plain)
	_, err = key.Open("other", "subscription", sealed)
	assert.Error(t, err)
	_, err = key.Open("archive", "other", sealed)
	assert.Error(t, err)
	again, err := LoadEventKey(home, true)
	require.NoError(t, err)
	plain, err = again.Open("archive", "subscription", sealed)
	require.NoError(t, err)
	assert.Equal(t, []byte("secret"), plain)
}

func TestEventKeyDoesNotReplaceMissingOrCorruptKeyWithSubscriptions(t *testing.T) {
	home := t.TempDir()
	_, err := LoadEventKey(home, true)
	require.Error(t, err)
	path := filepath.Join(home, "mcp-events.key")
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
	require.NoError(t, os.WriteFile(path, []byte("bad"), 0600))
	_, err = LoadEventKey(home, true)
	require.Error(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("bad"), got)
}
