package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
