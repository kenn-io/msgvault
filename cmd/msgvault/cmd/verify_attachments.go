package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentverify"
	"go.kenn.io/msgvault/internal/clirun"
)

func newVerifyAttachmentsCmd(run func(*cobra.Command, attachmentverify.Options, bool) error) *cobra.Command {
	opts := attachmentverify.Options{}
	var jsonOut bool
	command := &cobra.Command{
		Use:   "verify-attachments",
		Short: "Check a bounded page of retained attachment files",
		Long: `Read selected attachment content and thumbnails through the daemon's
existing loose/packed readers. Success requires verified EOF and successful
close. No repair, download, packing or cleanup is started by this command.

Selection includes retained rows on hidden, deleted and merged messages.
The report describes observations during the run, not a simultaneous snapshot
of SQL and files. Missing occurrences and RAW MIME are outside its scope.
Use --after-id with next_after_id to continue; pages can observe concurrent
changes. A partial or inconclusive report exits nonzero.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.Validate(); err != nil {
				return usageErr(cmd, err)
			}
			if run != nil {
				return run(cmd, opts, jsonOut)
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		},
	}
	command.Flags().Int64Var(&opts.AfterID, "after-id", 0, "Select attachment rows after this internal ID")
	command.Flags().IntVar(&opts.Limit, "limit", 100, "Maximum attachment rows (1 to 1000; each may also reference a thumbnail)")
	command.Flags().Int64Var(&opts.MaxBytes, "max-bytes", 1<<30, "Maximum decoded bytes read across distinct contents (one extra byte may establish exhaustion)")
	command.Flags().DurationVar(&opts.Timeout, "timeout", 2*time.Minute, "Maximum runtime (1s to 1h; reads observe cancellation between I/O calls)")
	command.Flags().BoolVar(&jsonOut, flagJSON, false, "Output the bounded report as JSON")
	return command
}

func (a *storeAPIAdapter) newDaemonVerifyAttachmentsCmd(cmdArgs []string, emit func(api.CLIRunEvent) error) *cobra.Command {
	command := newVerifyAttachmentsCmd(func(cmd *cobra.Command, opts attachmentverify.Options, jsonOut bool) error {
		if a.attachmentMaintenance == nil {
			return errors.New("attachment reader is unavailable")
		}
		report, err := attachmentverify.Run(cmd.Context(), a.store, a.attachmentMaintenance.blob, a.attachmentMaintenance.attachmentsDir, opts)
		if err != nil {
			return err
		}
		var output []byte
		if jsonOut {
			output, err = json.Marshal(report)
		} else {
			output = []byte(fmt.Sprintf("Attachment verification traversal: %s\nSelected %d rows, %d references, %d distinct contents.\nVerified %d contents and %d references; %d references affected.\nRead %d bytes. More rows: %t. Next after ID: %d.\n",
				report.State, report.RowsSelected, report.ReferencesSelected, report.DistinctContents, report.ContentsVerified,
				report.ReferencesVerified, report.ReferencesAffected, report.BytesRead, report.More, report.NextAfterID))
			output = fmt.Appendf(output, "Requested: after ID %d; limit %d rows; %d bytes; timeout %s.\n", opts.AfterID, opts.Limit, opts.MaxBytes, opts.Timeout)
			for _, ref := range report.References {
				output = appendAttachmentVerificationReference(output, ref, report.StopReason)
			}
			if report.StopReason != "" {
				output = fmt.Appendf(output, "Stopped: %s\n", report.StopReason)
			}
			output = append(output, "Only selected retained references were observed; this is not a global archive certificate.\n"...)
		}
		if err != nil {
			return err
		}
		if emit != nil {
			if err := emit(api.CLIRunEvent{Type: cliStreamStdout, Data: string(output) + "\n"}); err != nil {
				return err
			}
		}
		if report.State != "complete" || report.ReferencesAffected > 0 {
			return errors.New("attachment verification found problems or incomplete checks; see report")
		}
		return nil
	})
	clirun.AddLoggingFlags(command.Flags())
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SilenceUsage, command.SilenceErrors = true, true
	command.SetArgs(cmdArgs)
	return command
}

func init() { rootCmd.AddCommand(newVerifyAttachmentsCmd(nil)) }

// The status and final stop line already explain corrupt and unfinished slots.
// Keep the detailed reasons in the report used by JSON, without repeating them.
func appendAttachmentVerificationReference(output []byte, ref attachmentverify.Reference, stopReason string) []byte {
	output = fmt.Appendf(output, "  attachment=%d message=%d %s %s: %s", ref.AttachmentID, ref.MessageID, ref.Kind, ref.Hash, ref.Status)
	if ref.Reason != "" && ref.Status != attachmentverify.Corrupt &&
		(ref.Status != attachmentverify.NotChecked || stopReason == "") {
		output = fmt.Appendf(output, " (%s)", ref.Reason)
	}
	return append(output, '\n')
}
