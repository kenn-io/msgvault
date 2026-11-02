package cmd

import (
	"errors"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/synctechsms"
)

func newImportSynctechSMSCmd() *cobra.Command {
	opts := synctechsms.ImportOptions{
		IncludeSMS:         true,
		IncludeMMS:         true,
		IncludeCalls:       true,
		IncludeAttachments: true,
	}
	cmd := &cobra.Command{
		Use:     "import-synctech-sms <path>",
		Short:   "Import SMS Backup & Restore exports",
		Example: `  msgvault import synctech-sms ./sms-backup.xml --owner-phone +15551234567`,
		Long: `Import SMS, MMS, and call logs from XML or ZIP files produced by
SMS Backup & Restore by SyncTech Pty Ltd. --owner-phone is your E.164 number
and identifies outgoing messages. For scheduled imports from Google Drive, use
source synctech-sms add instead.`,
		Args: sourceImportArgs("owner-phone", "your E.164 number (for example +15551234567)", "msgvault import synctech-sms ./sms-backup.xml --owner-phone +15551234567"),
		RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			cfg := state.cfg
			if opts.OwnerPhone == "" {
				return errors.New("--owner-phone is required")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			opts.AttachmentsDir = cfg.AttachmentsDir()
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			summary, err := synctechsms.NewImporter(st, opts).ImportPath(args[0])
			if err != nil {
				return err
			}
			cmd.Printf("Imported %d SMS, %d MMS, %d calls, %d attachments from %d files\n",
				summary.SMSImported, summary.MMSImported, summary.CallsImported,
				summary.AttachmentsImported, summary.FilesImported)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.OwnerPhone, "owner-phone", "", "Owner phone number in E.164 format")
	cmd.Flags().BoolVar(&opts.IncludeSMS, "sms", true, "Import SMS records")
	cmd.Flags().BoolVar(&opts.IncludeMMS, "mms", true, "Import MMS records")
	cmd.Flags().BoolVar(&opts.IncludeCalls, "calls", true, "Import call log records")
	cmd.Flags().BoolVar(&opts.IncludeAttachments, "attachments", true, "Import MMS attachments")
	return cmd
}

func init() {
	rootCmd.AddCommand(newImportSynctechSMSCmd())
}
