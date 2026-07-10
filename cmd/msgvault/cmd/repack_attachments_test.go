package cmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/repacker"
)

func TestRepackAttachmentsAlwaysProxiesThroughDaemonCLIRunner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t,
		func(req daemonCLIRunTestRequest) {
			assert.Equal([]string{"repack-attachments"}, req.Args)
		},
		`{"type":"stdout","data":"Repacked 0 blob(s) (0B) from 0 pack(s) into 0 pack(s); removed 0 old pack(s).\n"}`,
		`{"type":"complete"}`,
	)
	configureRemoteDaemonForTest(t, server.URL)

	var stdout bytes.Buffer
	cmd := &cobra.Command{
		Use: repackAttachmentsCmd.Use, Args: repackAttachmentsCmd.Args, RunE: repackAttachmentsCmd.RunE,
	}
	cmd.SetOut(&stdout)
	require.NoError(cmd.Execute())
	assert.Equal(1, int(requests.Load()))
	assert.Contains(stdout.String(), "Repacked 0 blob(s)")
}

func TestWriteRepackAttachmentsStats(t *testing.T) {
	assert := assert.New(t)
	var out bytes.Buffer
	writeRepackAttachmentsStats(&out, repacker.Stats{
		MappingsPruned: 2, PacksSelected: 3, PacksRewritten: 2,
		PacksSealed: 1, PacksRemoved: 3, BlobsRepacked: 4,
		BytesRepacked: 1024, PacksDeferredOversized: 5, BudgetExhausted: true,
	})
	assert.Contains(out.String(), "Repacked 4 blob(s) (1.0K) from 2 pack(s) into 1 pack(s); removed 3 old pack(s).")
	assert.Contains(out.String(), "Pruned 2 stale packed blob mapping(s).")
	assert.Contains(out.String(), "Deferred 5 oversized pack(s)")
	assert.Contains(out.String(), "Repack byte budget reached")
}

func TestRepackAttachmentsOmitsZeroDeferredStats(t *testing.T) {
	var out bytes.Buffer
	writeRepackAttachmentsStats(&out, repacker.Stats{})

	assert.Equal(t,
		"Repacked 0 blob(s) (0B) from 0 pack(s) into 0 pack(s); removed 0 old pack(s).\n",
		out.String())
}
