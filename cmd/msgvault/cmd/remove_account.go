package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	imaplib "github.com/wesm/msgvault/internal/imap"
	"github.com/wesm/msgvault/internal/microsoft"
	"github.com/wesm/msgvault/internal/oauth"
	"github.com/wesm/msgvault/internal/store"
)

func newRemoveAccountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove-account <email>",
		Short: "Remove an account and all its data",
		Long: `Remove an account and all associated messages, labels, and sync data
from the local database. This is irreversible.

If the same identifier exists for multiple source types (e.g., gmail
and mbox), use --type to specify which one to remove.

The Parquet analytics cache is deleted because it is shared across accounts
and must be rebuilt. Run 'msgvault build-cache' afterward to rebuild it.

Attachment files on disk that are not shared with another account are deleted.
Shared attachments (same content hash across multiple accounts) are kept.

Examples:
  msgvault remove-account you@gmail.com
  msgvault remove-account you@gmail.com --yes
  msgvault remove-account you@gmail.com --type mbox`,
		Args: cobra.ExactArgs(1),
		RunE: runRemoveAccount,
	}
	cmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
	cmd.Flags().String(
		"type", "",
		"Source type to remove (gmail, mbox, etc.)",
	)
	return cmd
}

func runRemoveAccount(cmd *cobra.Command, args []string) error {
	if err := MustBeLocal("remove-account"); err != nil {
		return err
	}

	yes, err := cmd.Flags().GetBool("yes")
	if err != nil {
		return fmt.Errorf("read --yes flag: %w", err)
	}
	sourceType, err := cmd.Flags().GetString("type")
	if err != nil {
		return fmt.Errorf("read --type flag: %w", err)
	}

	email := args[0]

	dbPath := cfg.DatabaseDSN()
	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.InitSchema(); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	if err := runStartupMigrations(s); err != nil {
		return fmt.Errorf("startup migrations: %w", err)
	}

	source, err := resolveSource(s, email, sourceType)
	if err != nil {
		return err
	}

	activeSync, err := s.GetActiveSync(source.ID)
	if err != nil {
		return fmt.Errorf("check active sync: %w", err)
	}
	if activeSync != nil && !yes {
		return fmt.Errorf(
			"account %s has an active sync in progress\n"+
				"Use --yes to force removal", email,
		)
	}

	msgCount, err := s.CountMessagesForSource(source.ID)
	if err != nil {
		return fmt.Errorf("count messages: %w", err)
	}

	fmt.Printf("Account:  %s\n", email)
	fmt.Printf("Type:     %s\n", source.SourceType)
	fmt.Printf("Messages: %s\n", formatCount(msgCount))

	if !yes {
		fmt.Print("\nRemove this account and all its data? [y/N] ")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read confirmation: %w", err)
			}
			return fmt.Errorf(
				"no confirmation input (stdin closed); use --yes",
			)
		}
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		if answer != "y" && answer != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	// Collect attachment paths unique to this source before the cascade deletes them.
	attachmentPaths, err := s.AttachmentPathsUniqueToSource(source.ID)
	if err != nil {
		return fmt.Errorf("collect attachment paths: %w", err)
	}

	// RemoveSourceSerialized runs the active-sync check and the cascade
	// under a single exclusive write lock. StartSync blocks on that lock,
	// so a sync started between our check and the delete is either seen
	// as active (we skip file deletion) or fails after we commit because
	// the source is gone.
	hadActiveSync, err := s.RemoveSourceSerialized(cmd.Context(), source.ID)
	if err != nil {
		return fmt.Errorf("remove account: %w", err)
	}

	var deletedFiles, preservedFiles int
	switch {
	case hadActiveSync:
		if len(attachmentPaths) > 0 {
			fmt.Fprintf(os.Stderr,
				"Warning: a sync is in progress; "+
					"attachment files were not deleted.\n"+
					"Orphaned files may remain in %s\n",
				cfg.AttachmentsDir(),
			)
		}
	default:
		deletedFiles, preservedFiles = deleteOrphanedAttachmentFiles(
			cmd.Context(), s, attachmentPaths, cfg.AttachmentsDir(),
		)
	}

	// Remove credentials for the source type.
	switch source.SourceType {
	case "gmail":
		tokenPath := oauth.TokenFilePath(
			cfg.TokensDir(), source.Identifier,
		)
		if err := os.Remove(tokenPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr,
				"Warning: could not remove token file %s: %v\n",
				tokenPath, err,
			)
		}
	case "imap":
		if source.SyncConfig.Valid && source.SyncConfig.String != "" {
			imapCfg, parseErr := imaplib.ConfigFromJSON(source.SyncConfig.String)
			if parseErr == nil {
				switch imapCfg.EffectiveAuthMethod() {
				case imaplib.AuthXOAuth2:
					msMgr := microsoft.NewManager(
						cfg.Microsoft.ClientID,
						cfg.Microsoft.EffectiveTenantID(),
						cfg.TokensDir(),
						logger,
					)
					if err := msMgr.DeleteToken(imapCfg.Username); err != nil {
						fmt.Fprintf(os.Stderr,
							"Warning: could not remove Microsoft token: %v\n", err,
						)
					}
				default:
					credPath := imaplib.CredentialsPath(
						cfg.TokensDir(), source.Identifier,
					)
					if err := os.Remove(credPath); err != nil && !os.IsNotExist(err) {
						fmt.Fprintf(os.Stderr,
							"Warning: could not remove credentials file %s: %v\n",
							credPath, err,
						)
					}
				}
			}
		} else {
			// No sync_config — try removing credential file as fallback.
			credPath := imaplib.CredentialsPath(
				cfg.TokensDir(), source.Identifier,
			)
			if err := os.Remove(credPath); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr,
					"Warning: could not remove credentials file %s: %v\n",
					credPath, err,
				)
			}
		}
	}

	// Remove analytics cache (shared across accounts, needs full rebuild)
	analyticsDir := cfg.AnalyticsDir()
	if err := os.RemoveAll(analyticsDir); err != nil {
		fmt.Fprintf(os.Stderr,
			"Warning: could not remove analytics cache %s: %v\n",
			analyticsDir, err,
		)
	}

	fmt.Printf("\nAccount %s removed.\n", email)
	if deletedFiles > 0 {
		fmt.Printf("Deleted %d attachment file(s) from disk.\n", deletedFiles)
	}
	if preservedFiles > 0 {
		fmt.Printf(
			"Preserved %d attachment file(s) shared with other accounts.\n",
			preservedFiles,
		)
	}
	fmt.Println(
		"Run 'msgvault build-cache' to rebuild the analytics cache.",
	)

	return nil
}

// deleteOrphanedAttachmentFiles removes files in paths that are no longer
// referenced by any attachment row. Returns the count of files actually
// deleted and the count preserved because a concurrent reference appeared
// after the candidate list was collected.
//
// The work runs under an exclusive DB write lock so that no new sync can
// insert an attachment row (and place a file on disk) between the
// IsAttachmentPathReferenced check and os.Remove. The inside-lock
// HasAnyActiveSync recheck catches any sync on a different source that
// started between RemoveSourceSerialized releasing its lock and this
// helper acquiring its own; the per-file reference check handles the
// narrower race where a sync inserts a row for one of our candidate hashes.
func deleteOrphanedAttachmentFiles(
	ctx context.Context,
	s *store.Store,
	paths []string,
	attachmentsDir string,
) (deleted, preserved int) {
	if len(paths) == 0 {
		return 0, 0
	}

	cleanDir, err := filepath.Abs(attachmentsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"Warning: could not resolve attachments dir; "+
				"skipping file deletion: %v\n"+
				"Orphaned files may remain in %s\n",
			err, attachmentsDir,
		)
		return 0, 0
	}

	lockErr := s.WithExclusiveLock(ctx, func() error {
		running, err := s.HasAnyActiveSync()
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"Warning: could not check for active syncs: %v; "+
					"attachment files were not deleted.\n"+
					"Orphaned files may remain in %s\n",
				err, attachmentsDir,
			)
			return nil
		}
		if running {
			fmt.Fprintf(os.Stderr,
				"Warning: a sync is in progress; "+
					"attachment files were not deleted.\n"+
					"Orphaned files may remain in %s\n",
				attachmentsDir,
			)
			return nil
		}

		var failed int
		for _, relPath := range paths {
			d, p, ok := deleteOneAttachmentFile(s, cleanDir, relPath)
			if !ok {
				failed++
				continue
			}
			deleted += d
			preserved += p
		}
		if failed > 0 {
			fmt.Fprintf(os.Stderr,
				"Warning: could not remove %d attachment file(s) "+
					"from disk.\n",
				failed,
			)
		}
		return nil
	})
	if lockErr != nil {
		fmt.Fprintf(os.Stderr,
			"Warning: could not acquire exclusive lock; "+
				"skipping file deletion: %v\n"+
				"Orphaned files may remain in %s\n",
			lockErr, attachmentsDir,
		)
	}
	return deleted, preserved
}

// deleteOneAttachmentFile checks that relPath is safe to delete and either
// removes it, preserves it (still referenced), or reports a failure via ok=false.
func deleteOneAttachmentFile(
	s *store.Store, cleanDir, relPath string,
) (deleted, preserved int, ok bool) {
	absPath := filepath.Join(cleanDir, relPath)

	rel, err := filepath.Rel(cleanDir, absPath)
	if err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		fmt.Fprintf(os.Stderr,
			"Warning: attachment path %q escapes attachments "+
				"directory, skipping\n",
			relPath,
		)
		return 0, 0, false
	}

	referenced, err := s.IsAttachmentPathReferenced(relPath)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"Warning: could not verify attachment %s is unreferenced: %v\n",
			relPath, err,
		)
		return 0, 0, false
	}
	if referenced {
		return 0, 1, true
	}
	if err := os.Remove(absPath); err != nil && !os.IsNotExist(err) {
		return 0, 0, false
	}
	return 1, 0, true
}

// resolveSource finds the unique source for the given identifier.
// If multiple source types share the identifier, sourceType is
// required to disambiguate.
func resolveSource(
	s *store.Store, identifier, sourceType string,
) (*store.Source, error) {
	sources, err := s.GetSourcesByIdentifierOrDisplayName(identifier)
	if err != nil {
		return nil, fmt.Errorf("look up account: %w", err)
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("account %q not found", identifier)
	}

	if sourceType != "" {
		for _, src := range sources {
			if src.SourceType == sourceType {
				return src, nil
			}
		}
		return nil, fmt.Errorf(
			"account %q with type %q not found",
			identifier, sourceType,
		)
	}

	if len(sources) == 1 {
		return sources[0], nil
	}

	// Multiple matches — require --type to disambiguate
	var types []string
	for _, src := range sources {
		types = append(types, src.SourceType)
	}
	return nil, fmt.Errorf(
		"multiple accounts found for %q (types: %s)\n"+
			"Use --type to specify which one to remove",
		identifier, strings.Join(types, ", "),
	)
}

func init() {
	rootCmd.AddCommand(newRemoveAccountCmd())
}
