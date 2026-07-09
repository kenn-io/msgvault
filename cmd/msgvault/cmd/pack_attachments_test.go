package cmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPackAttachmentsProxiesThroughDaemonCLIRunner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server, requests := newDaemonCLIRunnerTestServer(t,
		func(req daemonCLIRunTestRequest) {
			assert.Equal([]string{"pack-attachments"}, req.Args, "args")
		},
		`{"type":"stdout","data":"Packed 0 blob(s) (0B) into 0 pack(s).\n"}`,
		`{"type":"complete"}`,
	)
	configureRemoteDaemonForTest(t, server.URL)

	var stdout bytes.Buffer
	cmd := &cobra.Command{
		Use:  packAttachmentsCmd.Use,
		Args: packAttachmentsCmd.Args,
		RunE: packAttachmentsCmd.RunE,
	}
	cmd.SetOut(&stdout)

	require.NoError(cmd.Execute(), "pack-attachments")
	assert.Equal(1, int(requests.Load()), "runner endpoint calls")
	assert.Equal("Packed 0 blob(s) (0B) into 0 pack(s).\n", stdout.String(), "stdout")
}
