package cmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
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

func TestPackAttachmentsReportsRepairStats(t *testing.T) {
	assert := assert.New(t)
	var out bytes.Buffer
	writePackAttachmentsStats(&out, packstore.PackStats{
		MappingsPruned:         2,
		LooseOrphansRemoved:    3,
		PacksQuarantined:       4,
		PacksUnreadable:        5,
		BlobsDeferredOversized: 6,
		PacksDeferredOversized: 7,
	})

	assert.Contains(out.String(), "Pruned 2 stale packed blob mapping(s).")
	assert.Contains(out.String(), "Removed 3 unreferenced loose file(s).")
	assert.Contains(out.String(), "Quarantined 4 damaged orphan pack(s).")
	assert.Contains(out.String(), "Found 5 unreadable orphan pack(s).")
	assert.Contains(out.String(), "Left 6 large canonical blob(s) loose")
	assert.Contains(out.String(), "64 MiB maintenance limit")
	assert.Contains(out.String(), "Deferred 7 oversized orphan pack(s)")
}

func TestPackAttachmentsOmitsZeroDeferredStats(t *testing.T) {
	var out bytes.Buffer
	writePackAttachmentsStats(&out, packstore.PackStats{})

	assert.Equal(t, "Packed 0 blob(s) (0B) into 0 pack(s).\n", out.String())
}
