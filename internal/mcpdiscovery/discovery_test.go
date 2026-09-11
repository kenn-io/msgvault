package mcpdiscovery

import (
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Listener publication, status, and cleanup are the client discovery contract.
func TestPublishedListenerStatusAndCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(os.Chmod(dir, 0o700))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(listener.Close()) })
	cleanup, err := Publish(dir, listener.Addr().String(), "test-listener-token", "http://127.0.0.1:4321")
	require.NoError(err)
	rows, err := List(dir)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal("http://"+listener.Addr().String()+"/mcp", rows[0].URL)
	assert.Equal("http://127.0.0.1:4321", rows[0].BackendURL)
	token, err := os.ReadFile(rows[0].TokenPath)
	require.NoError(err)
	assert.Equal("test-listener-token", string(token))
	require.NoError(cleanup())
	rows, err = List(dir)
	require.NoError(err)
	assert.Empty(rows)
}
