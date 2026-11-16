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

func TestVerifyAttachmentsTextKeepsJSONReasons(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason, stop string
		showReason                 bool
	}{
		{"corruption", attachmentverify.Corrupt, "physical reader rejected content or container integrity", "", false},
		{"cancellation", attachmentverify.NotChecked, "cancelled or timeout before verification completed", "cancelled or timeout", false},
		{"byte limit", attachmentverify.NotChecked, "stream did not reach verified EOF within byte limit", "byte limit", false},
		{"unfinished without stop", attachmentverify.NotChecked, "additional verification detail", "", true},
		{"catalogue", attachmentverify.Inconclusive, "catalogue could not be resolved", "", true},
		{"missing", attachmentverify.MissingLoose, "selected recorded loose paths are missing", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			ref := attachmentverify.Reference{AttachmentID: 7, MessageID: 11, Kind: "content", Hash: strings.Repeat("a", 64), Status: tc.status, Reason: tc.reason}
			text := string(appendAttachmentVerificationReference(nil, ref, tc.stop))
			assert.Contains(text, "attachment=7 message=11 content "+ref.Hash+": "+tc.status)
			if tc.showReason {
				assert.Contains(text, "("+tc.reason+")")
			} else {
				assert.NotContains(text, tc.reason)
			}
			encoded, err := json.Marshal(ref)
			require.NoError(err)
			var decoded attachmentverify.Reference
			require.NoError(json.Unmarshal(encoded, &decoded))
			assert.Equal(tc.reason, decoded.Reason, "text formatting must preserve the machine-readable reason")
			assert.Equal(tc.status, decoded.Status)
		})
	}
}
