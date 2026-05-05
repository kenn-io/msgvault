package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/gmail"
	"github.com/wesm/msgvault/internal/mime"
	"github.com/wesm/msgvault/internal/oauth"
	"github.com/wesm/msgvault/internal/store"
	"golang.org/x/oauth2"
)

var (
	verifySampleSize  int
	verifySkipDBCheck bool
)

var verifyCmd = &cobra.Command{
	Use:   "verify <email>",
	Short: "Verify archive integrity against Gmail",
	Long: `Verify the local archive by comparing message counts with Gmail
and sampling messages to ensure raw MIME data is intact.

This command:
1. Runs SQLite integrity checks on the database (unless --skip-db-check)
2. Compares local message count with Gmail's reported total
3. Checks how many messages have raw MIME data stored
4. Samples random messages and verifies their MIME can be decompressed

Examples:
  msgvault verify you@gmail.com
  msgvault verify you@gmail.com --sample 200
  msgvault verify you@gmail.com --skip-db-check`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		email := args[0]

		// Open database
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

		// Run SQLite integrity check before any Gmail work. Users with a
		// corrupt database should see the repair hint even if their OAuth
		// token is expired or the network is down.
		var dbCorrupt bool
		if !verifySkipDBCheck {
			fmt.Println("Running database integrity check...")
			integrityErrors, err := runIntegrityCheck(s)
			if err != nil {
				return fmt.Errorf("integrity check failed: %w", err)
			}
			if len(integrityErrors) == 0 {
				fmt.Println("  Database integrity: OK")
			} else {
				dbCorrupt = true
				fmt.Printf("  Database integrity: FAILED (%d errors)\n", len(integrityErrors))
				for i, ie := range integrityErrors {
					if i >= 10 {
						fmt.Printf("  ... and %d more errors\n", len(integrityErrors)-10)
						break
					}
					fmt.Printf("  - %s\n", ie)
				}
				printIntegrityRecoveryHint(integrityErrors)
			}
			fmt.Println()
		}

		// Look up source to get OAuth app binding
		appName := ""
		src, srcErr := findGmailSource(s, email)
		if srcErr != nil {
			return fmt.Errorf("look up source for %s: %w", email, srcErr)
		}
		if src != nil {
			appName = sourceOAuthApp(src)
		}

		if !cfg.OAuth.HasAnyConfig() {
			return errOAuthNotConfigured()
		}

		// Set up context with cancellation
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		// Handle Ctrl+C gracefully
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			fmt.Println("\nInterrupted.")
			cancel()
		}()

		// Resolve a Gmail token source. Service-account-bound sources mint
		// a fresh JWT-based token on demand; browser-OAuth sources reuse
		// the stored refresh token (and may prompt for re-auth in TTY).
		var tokenSource oauth2.TokenSource
		if saKeyPath := cfg.OAuth.ServiceAccountKeyFor(appName); saKeyPath != "" {
			saMgr, saErr := oauth.NewServiceAccountManager(saKeyPath, oauth.Scopes)
			if saErr != nil {
				return fmt.Errorf("service account: %w", saErr)
			}
			tokenSource, err = saMgr.TokenSource(ctx, email)
			if err != nil {
				return fmt.Errorf("service account token for %s: %w", email, err)
			}
		} else {
			clientSecretsPath, secretsErr := cfg.OAuth.ClientSecretsFor(appName)
			if secretsErr != nil {
				return secretsErr
			}
			oauthMgr, mgrErr := oauth.NewManager(clientSecretsPath, cfg.TokensDir(), logger)
			if mgrErr != nil {
				return wrapOAuthError(fmt.Errorf("create oauth manager: %w", mgrErr))
			}
			interactive := isatty.IsTerminal(os.Stdin.Fd()) ||
				isatty.IsCygwinTerminal(os.Stdin.Fd())
			tokenSource, err = getTokenSourceWithReauth(ctx, oauthMgr, email, interactive)
			if err != nil {
				return err
			}
		}

		// Create Gmail client (no rate limiter needed for single call)
		client := gmail.NewClient(tokenSource, gmail.WithLogger(logger))
		defer func() { _ = client.Close() }()

		// Get Gmail profile
		profile, err := client.GetProfile(ctx)
		if err != nil {
			return fmt.Errorf("get Gmail profile: %w", err)
		}

		fmt.Printf("Verifying archive for %s...\n\n", profile.EmailAddress)

		// Look up the Gmail source by the user-supplied identifier,
		// not the canonical profile address — the source is keyed
		// under the identifier from add-account. Filter to Gmail
		// specifically since the same identifier may exist for
		// other source types (mbox, imap).
		source, err := findGmailSource(s, email)
		if err != nil {
			return fmt.Errorf("get source: %w", err)
		}
		if source == nil {
			fmt.Printf("Gmail account %s not found in database.\n", email)
			fmt.Println("Run 'sync-full' first to populate the archive.")
			if dbCorrupt {
				return fmt.Errorf("database integrity check failed")
			}
			return nil
		}

		// Count local messages
		archiveCount, err := s.CountMessagesForSource(source.ID)
		if err != nil {
			return fmt.Errorf("count messages: %w", err)
		}

		withRaw, err := s.CountMessagesWithRaw(source.ID)
		if err != nil {
			return fmt.Errorf("count messages with raw: %w", err)
		}

		// Print summary
		gmailTotal := profile.MessagesTotal
		fmt.Printf("Gmail messages:      %10d\n", gmailTotal)
		fmt.Printf("Archived messages:   %10d\n", archiveCount)
		diff := gmailTotal - archiveCount
		if diff > 0 {
			fmt.Printf("Missing:             %10d\n", diff)
		} else if diff < 0 {
			fmt.Printf("Extra in archive:    %10d\n", -diff)
		} else {
			fmt.Printf("Difference:          %10d\n", diff)
		}
		fmt.Println()

		rawPct := float64(0)
		if archiveCount > 0 {
			rawPct = float64(withRaw) / float64(archiveCount) * 100
		}
		fmt.Printf("With raw MIME:       %10d (%.1f%%)\n", withRaw, rawPct)
		fmt.Println()

		// Sample verification
		if archiveCount > 0 && verifySampleSize > 0 {
			actualSampleSize := verifySampleSize
			if int64(actualSampleSize) > archiveCount {
				actualSampleSize = int(archiveCount)
			}

			sampleIDs, err := s.GetRandomMessageIDs(source.ID, actualSampleSize)
			if err != nil {
				return fmt.Errorf("get sample IDs: %w", err)
			}

			fmt.Printf("Sampling %d messages...\n", len(sampleIDs))

			verified := 0
			var errors []string

			for _, msgID := range sampleIDs {
				// Check context cancellation
				if ctx.Err() != nil {
					fmt.Println("\nVerification interrupted.")
					break
				}

				// Get raw MIME
				rawData, err := s.GetMessageRaw(msgID)
				if err != nil {
					if err == sql.ErrNoRows {
						errors = append(errors, fmt.Sprintf("msg %d: missing raw MIME", msgID))
					} else {
						errors = append(errors, fmt.Sprintf("msg %d: db error (%v)", msgID, err))
					}
					continue
				}

				// Verify it can be parsed as MIME
				_, err = mime.Parse(rawData)
				if err != nil {
					errors = append(errors, fmt.Sprintf("msg %d: corrupt MIME (%v)", msgID, err))
					continue
				}

				verified++
			}

			if len(errors) > 0 {
				fmt.Printf("Sample verified:     %10d of %d\n", verified, len(sampleIDs))
				fmt.Printf("Sample errors:       %10d\n", len(errors))
				for i, err := range errors {
					if i >= 5 {
						fmt.Printf("  ... and %d more\n", len(errors)-5)
						break
					}
					fmt.Printf("  - %s\n", err)
				}
			} else {
				fmt.Printf("Sample verified:     %10d (all OK)\n", verified)
			}
		}

		fmt.Println()
		fmt.Println("Verification complete.")

		if dbCorrupt {
			return fmt.Errorf("database integrity check failed")
		}

		return nil
	},
}

// runIntegrityCheck runs PRAGMA integrity_check on the database and returns
// any error strings. An empty slice means the database is healthy.
func runIntegrityCheck(s *store.Store) ([]string, error) {
	rows, err := s.DB().Query("PRAGMA integrity_check(100)")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var errors []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return nil, err
		}
		if result != "ok" {
			errors = append(errors, result)
		}
	}
	return errors, rows.Err()
}

// printIntegrityRecoveryHint prints repair guidance tailored to the kind of
// corruption reported. FTS5 shadow-table corruption is fixable with the
// lightweight `rebuild-fts` command; core B-tree corruption needs `.recover`,
// which requires free disk roughly equal to the database size.
func printIntegrityRecoveryHint(integrityErrors []string) {
	var ftsErrs, coreErrs int
	for _, e := range integrityErrors {
		if isFTSIntegrityError(e) {
			ftsErrs++
		} else {
			coreErrs++
		}
	}

	fmt.Println()
	fmt.Println("  Back up msgvault.db before attempting any repair.")
	fmt.Println()

	if ftsErrs > 0 {
		fmt.Println("  Search index (FTS5) corruption:")
		fmt.Println("    Run: msgvault rebuild-fts")
		fmt.Println("    Drops and recreates messages_fts from the core tables.")
		fmt.Println("    SQLite's 'rebuild' pragma reads from the corrupt shadow")
		fmt.Println("    tables and cannot clear this state.")
		fmt.Println()
	}

	if coreErrs > 0 {
		fmt.Println("  Core table corruption (e.g., Rowid out of order in messages")
		fmt.Println("  or message_bodies):")
		fmt.Println("    Run: sqlite3 msgvault.db '.recover' | sqlite3 recovered.db")
		fmt.Println("    (requires free disk roughly equal to the database size)")
		fmt.Println("    Alternative: sqlite3 msgvault.db .dump | sqlite3 new.db")
	}
}

// isFTSIntegrityError reports whether an integrity-check line describes
// corruption in the FTS5 search index rather than the core tables.
func isFTSIntegrityError(msg string) bool {
	return strings.Contains(msg, "messages_fts") ||
		strings.Contains(msg, "FTS5")
}

func init() {
	verifyCmd.Flags().IntVar(&verifySampleSize, "sample", 100, "Number of messages to sample for MIME verification")
	verifyCmd.Flags().BoolVar(&verifySkipDBCheck, "skip-db-check", false, "Skip SQLite integrity check")
	rootCmd.AddCommand(verifyCmd)
}
