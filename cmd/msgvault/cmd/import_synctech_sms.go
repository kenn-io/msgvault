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
		Use:   "import-synctech-sms <path>",
		Short: "Import SMS Backup & Restore exports",
		Long: `Import SMS, MMS, and call logs from XML or ZIP files produced by
SMS Backup & Restore by SyncTech Pty Ltd.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.OwnerPhone == "" {
				return errors.New("--owner-phone is required")
			}
			opts.AttachmentsDir = cfg.AttachmentsDir()
			st, err := openStoreAndInitForIngest()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
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
