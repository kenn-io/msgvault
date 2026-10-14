package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalDAVSyncCommandAndCachePolicy(t *testing.T) {
	cmd := newSyncCalDAVCmd()
	require.NotNil(t, cmd.Flags().Lookup("build-cache"))
	assert.True(t, manualSyncCLICommand([]string{"sync-caldav", "personal"}))
}
func TestReadCalDAVPassword(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "password")
	requirements.NoError(os.WriteFile(path, []byte("synthetic\n"), 0600))
	password, err := readCalDAVPassword(path)
	requirements.NoError(err)
	assertions.Equal("synthetic", password)
	if runtime.GOOS != "windows" {
		requirements.NoError(os.Chmod(path, 0644))
		_, err = readCalDAVPassword(path)
		requirements.Error(err)
	}
}

func TestCalDAVSyncForwardsToDaemon(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assertions.Equal([]string{"sync-caldav", "--full", "personal"}, req.Args)
	}, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(newSyncCalDAVCmd())
	root.SetContext(ctx)
	root.SetArgs([]string{"sync-caldav", "personal", "--full"})
	requirements.NoError(root.Execute())
	assertions.Equal(int32(1), requests.Load())
}
