package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/circleback"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

const (
	removeAccountCommandName   = "remove-account"
	removeAccountConfirmedFlag = "confirmed"
)

func newRemoveAccountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   removeAccountCommandName + " <email>",
		Short: "Remove an account and all its data",
		Long: `Remove an account and all associated messages, labels, and sync data
from the local database. This is irreversible.

If the same identifier exists for multiple source types (e.g., gmail
and mbox), use --type to specify which one to remove.

The Parquet analytics cache is deleted because it is shared across accounts
and must be rebuilt. Run 'msgvault build-cache' afterward to rebuild it.

Attachment files on disk that are not shared with another account are deleted.
Shared attachments (same content hash across multiple accounts) are kept.
Unique packed attachments become unreachable immediately; their immutable pack
bytes are reclaimed by attachment maintenance.

Examples:
  msgvault remove-account you@gmail.com
  msgvault remove-account you@gmail.com --yes
  msgvault remove-account you@gmail.com --type mbox`,
		Args: cobra.ExactArgs(1),
		RunE: runRemoveAccount,
	}
	cmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
	cmd.Flags().Bool(removeAccountConfirmedFlag, false, "Internal: confirmation was already accepted by the frontend CLI")
	if err := cmd.Flags().MarkHidden(removeAccountConfirmedFlag); err != nil {
		panic(err)
	}
	cmd.Flags().String(
		"type", "",
		"Source type to remove (gmail, mbox, etc.)",
	)
	return cmd
}

func runRemoveAccount(cmd *cobra.Command, args []string) error {
	if !isDaemonCLISubprocess() {
		return runRemoveAccountHTTP(cmd, args)
	}
	return runRemoveAccountLocal(cmd, args)
}

func runRemoveAccountHTTP(cmd *cobra.Command, args []string) error {
	yes, err := cmd.Flags().GetBool("yes")
	if err != nil {
		return fmt.Errorf("read --yes flag: %w", err)
	}
	if !yes {
		ok, err := confirmRemoveAccount(cmd.InOrStdin(), cmd.OutOrStdout())
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := cmd.Flags().Set(removeAccountConfirmedFlag, "true"); err != nil {
			return fmt.Errorf("set --%s after confirmation: %w", removeAccountConfirmedFlag, err)
		}
	}
	return runDaemonCLICommandHTTPFromCobra(cmd, args)
}

func confirmRemoveAccount(r io.Reader, w io.Writer) (bool, error) {
	_, _ = fmt.Fprint(w, "\nRemove this account and all its data? [y/N] ")
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("read confirmation: %w", err)
		}
		return false, errors.New("no confirmation input (stdin closed); use --yes")
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	if !isYesAnswer(answer) {
		_, _ = fmt.Fprintln(w, "Aborted.")
		return false, nil
	}
	return true, nil
}

func runRemoveAccountLocal(cmd *cobra.Command, args []string) error {
	yes, err := cmd.Flags().GetBool("yes")
	if err != nil {
		return fmt.Errorf("read --yes flag: %w", err)
	}
	sourceType, err := cmd.Flags().GetString("type")
	if err != nil {
		return fmt.Errorf("read --type flag: %w", err)
	}
	confirmed, err := cmd.Flags().GetBool(removeAccountConfirmedFlag)
	if err != nil {
		return fmt.Errorf("read --%s flag: %w", removeAccountConfirmedFlag, err)
	}
	email := args[0]

	s, cleanup, err := openWritableStoreAndInit()
	if err != nil {
		return err
	}
	defer cleanup()

	source, err := resolveSource(s, email, sourceType)
	if err != nil {
		return err
	}

	activeSync, err := s.GetActiveSync(source.ID)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
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

	if !yes && !confirmed {
		fmt.Print("\nRemove this account and all its data? [y/N] ")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read confirmation: %w", err)
			}
			return errors.New("no confirmation input (stdin closed); use --yes")
		}
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		if !isYesAnswer(answer) {
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
	hadActiveSync, packedMappingsRemoved, err := s.RemoveSourceSerialized(cmd.Context(), source.ID)
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
	case sourceTypeGmail:
		tokenPath := oauth.TokenFilePath(
			cfg.TokensDir(), source.Identifier,
		)
		if err := os.Remove(tokenPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr,
				"Warning: could not remove token file %s: %v\n",
				tokenPath, err,
			)
		}
	case sourceTypeTeams:
		graphMgr := microsoft.NewGraphManager(
			cfg.Microsoft.ClientID,
			cfg.Microsoft.EffectiveTenantID(),
			cfg.TokensDir(),
			logger,
		)
		if err := graphMgr.DeleteToken(source.Identifier); err != nil {
			fmt.Fprintf(os.Stderr,
				"Warning: could not remove Microsoft Graph token: %v\n", err,
			)
		}
	case sourceTypeBeeper:
		// The token is shared by every Beeper network-account source; only
		// remove it when the last one is gone. The source row was already
		// removed above, so any remaining rows belong to other accounts.
		remaining, lerr := s.ListSources(sourceTypeBeeper)
		if lerr != nil {
			fmt.Fprintf(os.Stderr,
				"Warning: could not check remaining beeper sources: %v\n", lerr,
			)
		} else if len(remaining) == 0 {
			if err := beeper.DeleteToken(cfg.TokensDir()); err != nil {
				fmt.Fprintf(os.Stderr,
					"Warning: could not remove Beeper token: %v\n", err,
				)
			}
		}
	case sourceTypeCircleback:
		circlebackMgr := circleback.NewManager("", cfg.TokensDir(), logger)
		if err := circlebackMgr.DeleteToken(source.Identifier); err != nil {
			fmt.Fprintf(os.Stderr,
				"Warning: could not remove Circleback token: %v\n", err,
			)
		}
	case sourceTypeIMAP:
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
	if packedMappingsRemoved > 0 {
		fmt.Printf(
			"Removed %d packed blob mapping(s); physical pack bytes will be reclaimed by repack.\n",
			packedMappingsRemoved,
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
