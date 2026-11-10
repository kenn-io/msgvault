package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/whatsapp"
)

func runWhatsAppTextImport(cmd *cobra.Command, args []string, opts whatsapp.TextOptions) error {
	state := invocationFromCommand(cmd)
	if !isDaemonCLISubprocess() && (isRemoteModeFor(state) || isAgentMode(state)) {
		_, err := daemonCLIRunCwd(HTTPStoreInfo{Kind: HTTPStoreConfiguredRemote}, true)
		return err
	}
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	opts.AttachmentsDir = state.cfg.AttachmentsDir()
	input, err := whatsapp.LoadText(cmd.Context(), args[0], opts)
	if err != nil {
		return fmt.Errorf("import WhatsApp TXT: %w", err)
	}
	if !isDaemonCLISubprocess() {
		return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args)
	}
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	summary, importErr := whatsapp.NewImporter(st, nil).ImportText(cmd.Context(), input)
	if summary.SourceID == 0 {
		return fmt.Errorf("import WhatsApp TXT: %w", importErr)
	}
	// Committed rows and their source must remain visible after cancellation.
	post := context.WithoutCancel(cmd.Context())
	if !noDefaultIdentityImportWhatsApp {
		confirmDefaultIdentity(post, cmd.OutOrStdout(), st, summary.SourceID, opts.Phone, opts.Phone, "phone-e164", state.logger)
	}
	migrationErr := runPostSourceCreateMigrationsForInvocation(post, st, state)
	cacheErr := rebuildCacheAfterWrite(post, state.cfg.DatabaseDSN(), state)
	resultErr := errors.Join(importErr, migrationErr, cacheErr)
	status := "complete"
	if resultErr != nil {
		status = "incomplete"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Import %s: %d records, %d notices, %d added, %d unchanged\n", status, summary.Records, summary.Notices, summary.Added, summary.Unchanged)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Indistinguishable duplicates: %d\n", summary.DuplicateAmbiguity)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Media: %d stored, %d missing, %d omitted, %d unsafe/unsupported, %d oversized\n", summary.MediaStored, summary.MediaMissing, summary.MediaOmitted, summary.MediaSkipped, summary.MediaOversized)
	if resultErr != nil {
		return fmt.Errorf("import WhatsApp TXT: %w", resultErr)
	}
	return nil
}
