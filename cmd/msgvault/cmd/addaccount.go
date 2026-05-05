package cmd

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/oauth"
	"github.com/wesm/msgvault/internal/store"
)

var (
	headless                    bool
	accountDisplayName          string
	forceReauth                 bool
	oauthAppName                string
	noDefaultIdentityAddAccount bool
)

var addAccountCmd = &cobra.Command{
	Use:   "add-account <email>",
	Short: "Add a Gmail account via OAuth",
	Long: `Add a Gmail account by completing the OAuth2 authorization flow.

By default, opens a browser for authorization. Use --headless to see instructions
for authorizing on headless servers (Google does not support Gmail in device flow).

If a token already exists, the command skips authorization. Use --force to delete
the existing token and start a fresh OAuth flow.

For Google Workspace orgs that require their own OAuth app, use --oauth-app to
specify a named app from config.toml.

Examples:
  msgvault add-account you@gmail.com
  msgvault add-account you@gmail.com --headless
  msgvault add-account you@gmail.com --force
  msgvault add-account you@acme.com --oauth-app acme
  msgvault add-account you@gmail.com --display-name "Work Account"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		email := args[0]

		if headless && forceReauth {
			return fmt.Errorf("--headless and --force cannot be used together: --force requires browser-based OAuth which is not available in headless mode")
		}

		// Resolve which client secrets to use
		resolvedApp := oauthAppName
		oauthAppExplicit := cmd.Flags().Changed("oauth-app")
		var clientSecretsPath string

		// Initialize database (in case it's new)
		dbPath := cfg.DatabaseDSN()
		s, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = s.Close() }()

		if err := s.InitSchema(); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
		if err := runStartupMigrationsForIngest(s); err != nil {
			return fmt.Errorf("startup migrations: %w", err)
		}

		// Look up existing source to detect binding changes
		existingSource, err := findGmailSource(s, email)
		if err != nil {
			return fmt.Errorf("look up existing source: %w", err)
		}

		// Inherit stored binding when --oauth-app is not specified.
		// This ensures re-running add-account on a named-app account
		// (e.g., after token loss) uses the correct credentials.
		if !oauthAppExplicit && existingSource != nil && existingSource.OAuthApp.Valid {
			resolvedApp = existingSource.OAuthApp.String
		}

		// Detect binding change: --oauth-app was explicitly set and
		// differs from the stored value (including clearing to default).
		bindingChanged := false
		if oauthAppExplicit && existingSource != nil {
			currentApp := ""
			if existingSource.OAuthApp.Valid {
				currentApp = existingSource.OAuthApp.String
			}
			if currentApp != oauthAppName {
				bindingChanged = true
			}
		}

		saKeyPath := cfg.OAuth.ServiceAccountKeyFor(resolvedApp)
		if headless {
			if saKeyPath != "" {
				return fmt.Errorf("service accounts do not use --headless; run add-account without --headless")
			}
			oauth.PrintHeadlessInstructions(email, cfg.TokensDir(), resolvedApp)
			return nil
		}

		// Check for service account configuration first
		if saKeyPath != "" {
			if forceReauth {
				return fmt.Errorf("service accounts do not use --force; tokens are minted on demand from the configured service account key")
			}
			saMgr, saErr := oauth.NewServiceAccountManager(saKeyPath, oauth.Scopes)
			if saErr != nil {
				return fmt.Errorf("service account: %w", saErr)
			}

			// Validate access by calling Gmail profile API
			ts, saErr := saMgr.TokenSource(cmd.Context(), email)
			if saErr != nil {
				return fmt.Errorf("service account token for %s: %w", email, saErr)
			}
			if saErr := oauth.ValidateTokenEmail(cmd.Context(), ts, email); saErr != nil {
				var mismatch *oauth.TokenMismatchError
				if errors.As(saErr, &mismatch) {
					existing, lookupErr := findGmailSource(s, email)
					if lookupErr != nil {
						return fmt.Errorf("service account validation failed: %w (also: %v)", saErr, lookupErr)
					}
					if existing == nil {
						return fmt.Errorf(
							"%w\nIf %s is the primary address, re-add with:\n"+
								"  msgvault add-account %s",
							saErr, mismatch.Actual, mismatch.Actual,
						)
					}
				}
				return fmt.Errorf("service account validation for %s: %w", email, saErr)
			}

			// Register source
			source, saErr := s.GetOrCreateSource("gmail", email)
			if saErr != nil {
				return fmt.Errorf("create source: %w", saErr)
			}
			// Persist the oauth_app binding (set or clear). Mirror the
			// standard OAuth branch: when --oauth-app was explicitly
			// changed and resolves to "", clear the stored binding so
			// later syncs don't keep resolving credentials through the
			// stale named-app pointer.
			if resolvedApp != "" {
				newApp := sql.NullString{String: resolvedApp, Valid: true}
				if saErr := s.UpdateSourceOAuthApp(source.ID, newApp); saErr != nil {
					return fmt.Errorf("update oauth app binding: %w", saErr)
				}
			} else if bindingChanged {
				if saErr := s.UpdateSourceOAuthApp(source.ID, sql.NullString{}); saErr != nil {
					return fmt.Errorf("clear oauth app binding: %w", saErr)
				}
			}
			if accountDisplayName != "" {
				if saErr := s.UpdateSourceDisplayName(source.ID, accountDisplayName); saErr != nil {
					return fmt.Errorf("set display name: %w", saErr)
				}
			}

			fmt.Printf("Account %s authorized via service account.\n", email)
			fmt.Println("Next step: msgvault sync-full", email)
			return nil
		}

		// Resolve client secrets path (standard OAuth flow)
		clientSecretsPath, err = cfg.OAuth.ClientSecretsFor(resolvedApp)
		if err != nil {
			if !cfg.OAuth.HasAnyConfig() {
				return errOAuthNotConfigured()
			}
			return err
		}

		// Create OAuth manager
		oauthMgr, err := oauth.NewManager(clientSecretsPath, cfg.TokensDir(), logger)
		if err != nil {
			return wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
		}

		// If --force, delete existing token so we re-authorize
		if forceReauth {
			if oauthMgr.HasToken(email) {
				fmt.Printf("Removing existing token for %s...\n", email)
				if err := oauthMgr.DeleteToken(email); err != nil {
					return fmt.Errorf("delete existing token: %w", err)
				}
			} else {
				fmt.Printf("No existing token found for %s, proceeding with authorization.\n", email)
			}
		}

		// If a valid token exists, check if we can reuse it.
		// Validate the token's client identity when any named app is
		// involved — whether from an explicit flag, a binding change,
		// or inherited from the DB. A mismatched token would fail on
		// next refresh.
		needsClientCheck := bindingChanged || oauthAppExplicit ||
			resolvedApp != ""
		tokenReusable := !forceReauth && oauthMgr.HasToken(email) &&
			(!needsClientCheck || oauthMgr.TokenMatchesClient(email))
		if tokenReusable {
			source, err := s.GetOrCreateSource("gmail", email)
			if err != nil {
				return fmt.Errorf("create source: %w", err)
			}
			// Update oauth_app binding if it changed or was newly specified
			if bindingChanged || (resolvedApp != "" && !source.OAuthApp.Valid) {
				newApp := sql.NullString{String: resolvedApp, Valid: resolvedApp != ""}
				if err := s.UpdateSourceOAuthApp(source.ID, newApp); err != nil {
					return fmt.Errorf("update oauth app binding: %w", err)
				}
			}
			if accountDisplayName != "" {
				if err := s.UpdateSourceDisplayName(source.ID, accountDisplayName); err != nil {
					return fmt.Errorf("set display name: %w", err)
				}
			}
			// Auto-default-identity must run BEFORE the legacy migration
			// retry (runPostSourceCreateMigrations). The migration's
			// set-semantics merge handles the case where the legacy
			// [identity] block contains the same address. Reverse order
			// would leave the source without its own account identifier
			// because confirmDefaultIdentity skips on any existing rows.
			if !noDefaultIdentityAddAccount {
				confirmDefaultIdentity(cmd.OutOrStdout(), s, source.ID, email, email, "account-identifier")
			}
			if err := runPostSourceCreateMigrations(s); err != nil {
				return fmt.Errorf("post-source-create migrations: %w", err)
			}
			if bindingChanged {
				fmt.Printf("Account %s: OAuth app binding updated to %q.\n", email, resolvedApp)
			} else {
				fmt.Printf("Account %s is already authorized.\n", email)
			}
			fmt.Println("Next step: msgvault sync-full", email)
			return nil
		}

		// Perform authorization
		if bindingChanged {
			fmt.Printf("Switching OAuth app for %s to %q. Authorizing...\n", email, oauthAppName)
		} else {
			fmt.Println("Starting browser authorization...")
		}

		if err := oauthMgr.Authorize(cmd.Context(), email); err != nil {
			var mismatch *oauth.TokenMismatchError
			if errors.As(err, &mismatch) {
				existing, lookupErr := findGmailSource(s, email)
				if lookupErr != nil {
					return fmt.Errorf("authorization failed: %w (also: %v)", err, lookupErr)
				}
				if existing == nil {
					return fmt.Errorf(
						"%w\nIf %s is the primary address, re-add with:\n"+
							"  msgvault add-account %s",
						err, mismatch.Actual, mismatch.Actual,
					)
				}
			}
			return fmt.Errorf("authorization failed: %w", err)
		}

		// Authorization succeeded — now persist the binding and source.
		source, err := s.GetOrCreateSource("gmail", email)
		if err != nil {
			return fmt.Errorf("create source: %w", err)
		}

		// Update oauth_app binding (set or clear)
		if resolvedApp != "" {
			newApp := sql.NullString{String: resolvedApp, Valid: true}
			if err := s.UpdateSourceOAuthApp(source.ID, newApp); err != nil {
				return fmt.Errorf("update oauth app binding: %w", err)
			}
		} else if bindingChanged {
			// Clearing the binding (switching back to default)
			if err := s.UpdateSourceOAuthApp(source.ID, sql.NullString{}); err != nil {
				return fmt.Errorf("clear oauth app binding: %w", err)
			}
		}

		if accountDisplayName != "" {
			if err := s.UpdateSourceDisplayName(source.ID, accountDisplayName); err != nil {
				return fmt.Errorf("set display name: %w", err)
			}
		}
		// Auto-default-identity must run BEFORE the legacy migration
		// retry — see comment on the token-reusable path above.
		if !noDefaultIdentityAddAccount {
			confirmDefaultIdentity(cmd.OutOrStdout(), s, source.ID, email, email, "account-identifier")
		}
		if err := runPostSourceCreateMigrations(s); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}

		fmt.Printf("\nAccount %s authorized successfully!\n", email)
		fmt.Println("You can now run: msgvault sync-full", email)

		return nil
	},
}

func findGmailSource(
	s *store.Store, email string,
) (*store.Source, error) {
	sources, err := s.GetSourcesByIdentifier(email)
	if err != nil {
		return nil, fmt.Errorf("look up sources for %s: %w", email, err)
	}
	for _, src := range sources {
		if src.SourceType == "gmail" {
			return src, nil
		}
	}
	return nil, nil
}

func init() {
	addAccountCmd.Flags().BoolVar(&headless, "headless", false, "Show instructions for headless server setup")
	addAccountCmd.Flags().BoolVar(&forceReauth, "force", false, "Delete existing token and re-authorize")
	addAccountCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "Display name for the account (e.g., \"Work\", \"Personal\")")
	addAccountCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "Named OAuth app from config (for Google Workspace orgs)")
	addAccountCmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, noDefaultIdentityHelp)
	rootCmd.AddCommand(addAccountCmd)
}
