package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

var (
	headless                    bool
	accountDisplayName          string
	forceReauth                 bool
	oauthAppName                string
	noDefaultIdentityAddAccount bool
)

// addAccountUse is the usage string for the add-account command.
const addAccountUse = "add-account <email>"

// errGmailSourceNotFound is returned by findGmailSource when no Gmail
// source is registered for the given identifier. Wrapped via fmt.Errorf
// so callers can use errors.Is to tell "no such account" apart from real
// lookup errors.
var errGmailSourceNotFound = errors.New("gmail source not found")

var addAccountCmd = &cobra.Command{
	Use:   addAccountUse,
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
	RunE: runAddAccountLocal,
}

func newAddAccountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   addAccountUse,
		Short: addAccountCmd.Short,
		Long:  addAccountCmd.Long,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if headless && forceReauth {
				return usageErr(cmd, errors.New("--headless and --force cannot be used together: --force requires browser-based OAuth which is not available in headless mode"))
			}
			if !isDaemonCLISubprocess() {
				return runAddAccountHTTP(cmd, args)
			}
			return runAddAccountLocal(cmd, args)
		},
	}
	registerAddAccountFlags(cmd)
	return cmd
}

// addAccountBinding is the resolved OAuth app for an add-account run.
type addAccountBinding struct {
	resolvedApp    string
	explicit       bool
	bindingChanged bool
}

// resolveAddAccountBinding inherits the stored oauth_app binding when the
// flag is absent (so re-adding a named-app account after token loss uses
// the correct credentials) and detects explicit binding changes, including
// clearing back to the default app.
func resolveAddAccountBinding(flagApp string, flagExplicit bool, storedApp sql.NullString, sourceExists bool) addAccountBinding {
	binding := addAccountBinding{resolvedApp: flagApp, explicit: flagExplicit}
	if !flagExplicit && sourceExists && storedApp.Valid {
		binding.resolvedApp = storedApp.String
	}
	if flagExplicit && sourceExists {
		currentApp := ""
		if storedApp.Valid {
			currentApp = storedApp.String
		}
		if currentApp != flagApp {
			binding.bindingChanged = true
		}
	}
	return binding
}

// newAddAccountOAuthManager builds the Gmail OAuth manager for email,
// preserving any already-granted scopes so Google's replacement consent
// does not drop Calendar/Drive scopes from the shared token file.
func newAddAccountOAuthManager(clientSecretsPath, email string) (*oauth.Manager, error) {
	scopeProbe, err := oauth.NewManager(clientSecretsPath, cfg.TokensDir(), logger)
	if err != nil {
		return nil, wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
	}
	oauthScopes := addAccountOAuthScopesForToken(
		scopeProbe.HasScopeMetadata(email),
		scopeProbe.GrantedScopes(email),
	)
	mgr, err := oauth.NewManagerWithScopes(clientSecretsPath, cfg.TokensDir(), logger, oauthScopes)
	if err != nil {
		return nil, wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
	}
	return mgr, nil
}

// addAccountTokenReusable reports whether the stored token can be reused
// without a fresh authorization. The token's client identity is validated
// whenever any named app is involved — from an explicit flag, a binding
// change, or a stored binding — because a mismatched token would fail on
// its next refresh.
func addAccountTokenReusable(mgr *oauth.Manager, email string, binding addAccountBinding) bool {
	needsClientCheck := binding.bindingChanged || binding.explicit ||
		binding.resolvedApp != ""
	return mgr.HasToken(email) &&
		(!needsClientCheck || mgr.TokenMatchesClient(email)) &&
		addAccountTokenHasGmailScopes(mgr, email)
}

// addAccountAuthorizeError decorates an authorization failure with the
// re-add hint when the consent screen authenticated a different address
// than the one being added.
func addAccountAuthorizeError(err error, sourceExists bool) error {
	var mismatch *oauth.TokenMismatchError
	if errors.As(err, &mismatch) && !sourceExists {
		return fmt.Errorf(
			"%w\nIf %s is the primary address, re-add with:\n"+
				"  msgvault add-account %s",
			err, mismatch.Actual, mismatch.Actual,
		)
	}
	return fmt.Errorf("authorization failed: %w", err)
}

// runAddAccountHTTP completes any needed browser authorization in this
// process — which owns the user's display and browser — before proxying to
// the daemon. The subprocess then finds a fresh reusable token, so it never
// opens a browser (or waits on a human) while holding the operation gate.
func runAddAccountHTTP(cmd *cobra.Command, args []string) error {
	if !headless {
		if err := preflightAddAccountAuthorize(cmd, args[0]); err != nil {
			return err
		}
	}
	return runDaemonCLICommandHTTPFromCobra(cmd, args)
}

func preflightAddAccountAuthorize(cmd *cobra.Command, email string) error {
	if IsRemoteMode() {
		// Tokens live on the remote host; authorization must happen there.
		return nil
	}
	storedApp, sourceExists, err := lookupGmailAccountBinding(cmd.Context(), email)
	if err != nil {
		return err
	}
	binding := resolveAddAccountBinding(oauthAppName, cmd.Flags().Changed("oauth-app"), storedApp, sourceExists)
	if cfg.OAuth.ServiceAccountKeyFor(binding.resolvedApp) != "" {
		// Service accounts mint tokens on demand; no browser involved.
		return nil
	}
	clientSecretsPath, err := cfg.OAuth.ClientSecretsFor(binding.resolvedApp)
	if err != nil {
		// Let the subprocess report the configuration error.
		return nil //nolint:nilerr // deliberate: config errors surface daemon-side
	}
	mgr, err := newAddAccountOAuthManager(clientSecretsPath, email)
	if err != nil {
		return err
	}
	if forceReauth {
		if mgr.HasToken(email) {
			fmt.Printf("Removing existing token for %s...\n", email)
			if err := mgr.DeleteToken(email); err != nil {
				return fmt.Errorf("delete existing token: %w", err)
			}
		} else {
			fmt.Printf("No existing token found for %s, proceeding with authorization.\n", email)
		}
	}
	if addAccountTokenReusable(mgr, email, binding) {
		return nil
	}

	if binding.bindingChanged {
		fmt.Printf("Switching OAuth app for %s to %q. Authorizing...\n", email, oauthAppName)
	} else {
		fmt.Println("Starting browser authorization...")
	}
	if err := mgr.Authorize(cmd.Context(), email); err != nil {
		return addAccountAuthorizeError(err, sourceExists)
	}
	// The subprocess must not force-delete the token minted above.
	if forceReauth {
		if err := cmd.Flags().Set("force", "false"); err != nil {
			return fmt.Errorf("clear --force after authorization: %w", err)
		}
	}
	return nil
}

// lookupGmailAccountBinding fetches the stored oauth_app binding for email
// through the daemon's read API, mirroring findGmailSource for callers
// without direct database access.
func lookupGmailAccountBinding(ctx context.Context, email string) (sql.NullString, bool, error) {
	st, _, err := OpenHTTPStore(ctx)
	if err != nil {
		return sql.NullString{}, false, err
	}
	defer func() { _ = st.Close() }()
	accounts, err := st.GetCLIAccounts(ctx)
	if err != nil {
		return sql.NullString{}, false, fmt.Errorf("look up existing source: %w", err)
	}
	for _, account := range accounts {
		if account.Type == sourceTypeGmail && account.Email == email {
			return sql.NullString{String: account.OAuthApp, Valid: account.OAuthApp != ""}, true, nil
		}
	}
	return sql.NullString{}, false, nil
}

func runAddAccountLocal(cmd *cobra.Command, args []string) error {
	email := args[0]

	if headless && forceReauth {
		return usageErr(cmd, errors.New("--headless and --force cannot be used together: --force requires browser-based OAuth which is not available in headless mode"))
	}

	oauthAppExplicit := cmd.Flags().Changed("oauth-app")
	var clientSecretsPath string

	// Initialize database (in case it's new)
	s, cleanup, err := openWritableStoreAndInitForIngest()
	if err != nil {
		return err
	}
	defer cleanup()

	// Look up existing source to detect binding changes
	existingSource, err := findGmailSource(s, email)
	if err != nil && !errors.Is(err, errGmailSourceNotFound) {
		return fmt.Errorf("look up existing source: %w", err)
	}

	storedApp := sql.NullString{}
	if existingSource != nil {
		storedApp = existingSource.OAuthApp
	}
	binding := resolveAddAccountBinding(oauthAppName, oauthAppExplicit, storedApp, existingSource != nil)
	resolvedApp := binding.resolvedApp
	bindingChanged := binding.bindingChanged

	saKeyPath := cfg.OAuth.ServiceAccountKeyFor(resolvedApp)
	if headless {
		if saKeyPath != "" {
			return usageErr(cmd, errors.New("service accounts do not use --headless; run add-account without --headless"))
		}
		oauth.PrintHeadlessInstructions(email, cfg.TokensDir(), resolvedApp)
		return nil
	}

	// Check for service account configuration first
	if saKeyPath != "" {
		if forceReauth {
			return usageErr(cmd, errors.New("service accounts do not use --force; tokens are minted on demand from the configured service account key"))
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
				if lookupErr != nil && !errors.Is(lookupErr, errGmailSourceNotFound) {
					return fmt.Errorf("service account validation failed: %w (also: %w)", saErr, lookupErr)
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
		source, saErr := s.GetOrCreateSource(sourceTypeGmail, email)
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

	// Create OAuth manager. If a scoped token already exists, preserve those
	// grants when reauthorizing for Gmail; Google replacement consent would
	// otherwise drop Calendar/Drive scopes from the shared token file.
	oauthMgr, err := newAddAccountOAuthManager(clientSecretsPath, email)
	if err != nil {
		return err
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

	tokenReusable := !forceReauth && addAccountTokenReusable(oauthMgr, email, binding)
	if tokenReusable {
		source, err := s.GetOrCreateSource(sourceTypeGmail, email)
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

	// Perform authorization. Under the daemon this cannot work: the
	// frontend CLI preflights browser authorization before proxying, and
	// a browser opened here would appear on the daemon's host while the
	// operation gate blocks every other command on the human consent.
	if isDaemonCLISubprocess() {
		return fmt.Errorf(
			"account %s needs browser authorization, which cannot run behind the daemon; "+
				"run `msgvault add-account %s` from a terminal on this machine, "+
				"or use --headless for token-copy instructions",
			email, email,
		)
	}
	if bindingChanged {
		fmt.Printf("Switching OAuth app for %s to %q. Authorizing...\n", email, oauthAppName)
	} else {
		fmt.Println("Starting browser authorization...")
	}

	if err := oauthMgr.Authorize(cmd.Context(), email); err != nil {
		return addAccountAuthorizeError(err, existingSource != nil)
	}

	// Authorization succeeded — now persist the binding and source.
	source, err := s.GetOrCreateSource(sourceTypeGmail, email)
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
}

func addAccountOAuthScopesForToken(hasScopeMetadata bool, existingScopes []string) []string {
	if !hasScopeMetadata {
		return append([]string(nil), oauth.Scopes...)
	}
	scopes := append([]string(nil), existingScopes...)
	for _, scope := range oauth.Scopes {
		scopes = appendScopeIfMissing(scopes, scope)
	}
	return scopes
}

func addAccountTokenHasGmailScopes(mgr *oauth.Manager, email string) bool {
	if !mgr.HasScopeMetadata(email) {
		return true
	}
	for _, scope := range oauth.ScopesDeletion {
		if mgr.HasScope(email, scope) {
			return true
		}
	}
	for _, scope := range oauth.Scopes {
		if !mgr.HasScope(email, scope) {
			return false
		}
	}
	return true
}

func findGmailSource(
	s *store.Store, email string,
) (*store.Source, error) {
	sources, err := s.GetSourcesByIdentifier(email)
	if err != nil {
		return nil, fmt.Errorf("look up sources for %s: %w", email, err)
	}
	for _, src := range sources {
		if src.SourceType == sourceTypeGmail {
			return src, nil
		}
	}
	return nil, fmt.Errorf("identifier %q: %w", email, errGmailSourceNotFound)
}

func registerAddAccountFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&headless, "headless", false, "Show instructions for headless server setup")
	cmd.Flags().BoolVar(&forceReauth, "force", false, "Delete existing token and re-authorize")
	cmd.Flags().StringVar(&accountDisplayName, "display-name", "", "Display name for the account (e.g., \"Work\", \"Personal\")")
	cmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "Named OAuth app from config (for Google Workspace orgs)")
	cmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, noDefaultIdentityHelp)
}

func init() {
	registerAddAccountFlags(addAccountCmd)
	rootCmd.AddCommand(newAddAccountCmd())
}
