package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/whatsapp"
)

func runWhatsAppTextImport(cmd *cobra.Command, path string, opts whatsapp.TextOptions) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	opts.AttachmentsDir = state.cfg.AttachmentsDir()
	summary, importErr := whatsapp.NewImporter(st, nil).ImportText(cmd.Context(), path, opts)
	// Committed rows and their source must remain visible after cancellation.
	post := context.WithoutCancel(cmd.Context())
	var migrationErr error
	if summary.SourceID != 0 {
		if !noDefaultIdentityImportWhatsApp {
			confirmDefaultIdentity(post, cmd.OutOrStdout(), st, summary.SourceID, opts.Phone, opts.Phone, "phone-e164", state.logger)
		}
		migrationErr = runPostSourceCreateMigrationsForInvocation(post, st, state)
	}
	cacheErr := rebuildCacheAfterWrite(post, state.cfg.DatabaseDSN(), state)
	if err := errors.Join(importErr, migrationErr, cacheErr); err != nil {
		return fmt.Errorf("import WhatsApp TXT: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Import complete: %d records, %d notices, %d added, %d unchanged\n", summary.Records, summary.Notices, summary.Added, summary.Unchanged)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Identical-record ambiguity: %d additional occurrences\n", summary.DuplicateAmbiguity)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Media: %d stored, %d missing, %d omitted, %d unsafe/unsupported, %d oversized\n", summary.MediaStored, summary.MediaMissing, summary.MediaOmitted, summary.MediaSkipped, summary.MediaOversized)
	return nil
}
