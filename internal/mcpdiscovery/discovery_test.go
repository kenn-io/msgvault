package mcpdiscovery

import (
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
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

func TestListOmitsUnverifiedProcessRecords(t *testing.T) {
	for _, kind := range []string{"mismatched", "missing", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := t.TempDir()
			require.NoError(os.Chmod(dir, 0o700))
			cleanup, err := Publish(dir, "127.0.0.1:9876", "test-listener-token", "")
			require.NoError(err)
			t.Cleanup(func() { require.NoError(cleanup()) })
			store := daemon.RuntimeStore{Dir: dir, Prefix: "mcp"}
			records, err := store.List()
			require.NoError(err)
			require.Len(records, 1)
			rec := records[0]
			require.Equal(daemon.ProcessIdentityMatch, daemon.CompareRuntimeProcessIdentity(rec))
			switch kind {
			case "mismatched":
				// Change the creation value while retaining its platform encoding.
				// The PID stays live, as it would after the OS reuses a stale PID.
				if rec.ProcessIdentityV2 != "" {
					rec.ProcessIdentityV2 += "0"
				} else {
					rec.ProcessIdentity += "0"
				}
				require.Equal(daemon.ProcessIdentityMismatch, daemon.CompareRuntimeProcessIdentity(rec))
			case "missing":
				rec.ProcessIdentity = ""
				rec.ProcessIdentityV2 = ""
			case "malformed":
				rec.ProcessIdentityV2 = "malformed"
			}
			recordPath, err := store.Write(rec)
			require.NoError(err)
			rows, err := List(dir)
			require.NoError(err)
			assert.Empty(rows)
			// Status observes records; it must not prune them or their tokens.
			assert.FileExists(recordPath)
			assert.FileExists(rec.Metadata["token_path"])
		})
	}
}
