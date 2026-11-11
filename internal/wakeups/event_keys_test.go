package wakeups

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventKeySealsToArchiveAndSubscription(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	key, err := LoadEventKey(home, false)
	require.NoError(err)
	sealed, err := key.Seal("archive", "subscription", []byte("secret"))
	require.NoError(err)
	plain, err := key.Open("archive", "subscription", sealed)
	require.NoError(err)
	assert.Equal([]byte("secret"), plain)
	_, err = key.Open("other", "subscription", sealed)
	assert.Error(err)
	_, err = key.Open("archive", "other", sealed)
	assert.Error(err)
	again, err := LoadEventKey(home, true)
	require.NoError(err)
	plain, err = again.Open("archive", "subscription", sealed)
	require.NoError(err)
	assert.Equal([]byte("secret"), plain)
}

func TestEventKeyDoesNotReplaceMissingOrCorruptKeyWithSubscriptions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	_, err := LoadEventKey(home, true)
	require.Error(err)
	path := filepath.Join(home, "mcp-events.key")
	_, err = os.Stat(path)
	assert.True(os.IsNotExist(err))
	require.NoError(os.WriteFile(path, []byte("bad"), 0600))
	_, err = LoadEventKey(home, true)
	require.Error(err)
	got, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal([]byte("bad"), got)
}
