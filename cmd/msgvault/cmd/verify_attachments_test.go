package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentverify"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestVerifyAttachmentsArgumentValidation(t *testing.T) {
	require := require.New(t)
	for _, args := range [][]string{{"--limit=0"}, {"--limit=1001"}, {"--after-id=-1"}, {"--timeout=0s"}, {"--max-bytes=0"}, {"--repair"}, {"unexpected"}} {
		command := newVerifyAttachmentsCmd(func(*cobra.Command, attachmentverify.Options, bool) error {
			require.FailNow("invalid arguments reached diagnostic")
			return nil
		})
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		command.SilenceUsage = true
		command.SilenceErrors = true
		command.SetArgs(args)
		require.Error(command.ExecuteContext(t.Context()))
	}
}

func TestVerifyAttachmentsDaemonUsesSharedReaderWithoutChildOrMaintenance(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	maintenance, err := newAttachmentMaintenance(st, t.TempDir(), nil, true)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(maintenance.close()) })
	maintenance.packPending.Store(false)
	maintenance.verifyPending.Store(false)
	adapter := &storeAPIAdapter{store: st, attachmentMaintenance: maintenance}
	var stdout strings.Builder
	err = adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{"verify-attachments", "--json"}}, func(event api.CLIRunEvent) error {
		_, err := stdout.WriteString(event.Data)
		require.NoError(err)
		return nil
	}, func(context.Context, []string, map[string]string, string, func(string, string) error) error {
		require.FailNow("diagnostic must run with shared daemon reader")
		return nil
	})
	require.NoError(err)
	var report attachmentverify.Report
	require.NoError(json.Unmarshal([]byte(stdout.String()), &report))
	assert.Equal("complete", report.State)
	assert.Zero(report.RowsSelected)
	assert.False(maintenance.packPending.Load())
	assert.False(maintenance.verifyPending.Load())
	// Transport/write failures must be returned rather than becoming success.
	failure := errors.New("output rejected")
	err = adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{"verify-attachments", "--json"}}, func(api.CLIRunEvent) error { return failure }, nil)
	require.ErrorIs(err, failure)
}
