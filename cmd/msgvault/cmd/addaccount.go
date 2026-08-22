package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

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
	readonlyGrant               bool
)

// addAccountUse is the usage string for the add-account command.
const addAccountUse = "add-account <email>"

// addAccountGrantDecidedFlag records that the grant decision was already made
// by the frontend CLI before it proxied to the daemon. The daemon appends this
// flag only after authenticating the frontend's proof with its private runtime
// token. The decision gates authorization, so re-running it in the subprocess
// after the frontend has already authorized would refuse a token this run just
// minted.
const addAccountGrantDecidedFlag = "grant-decided"

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

By default msgvault requests Gmail read and modify access. Use --readonly to
request read access only.

Access already granted cannot be narrowed. Re-authorizing does not revoke it,
and revoking applies to the whole grant, so --readonly refuses an account that
already holds Gmail write access. To make such an account read-only, revoke
msgvault at https://myaccount.google.com/permissions, delete its token file,
then add it again with --readonly.

For Google Workspace orgs that require their own OAuth app, use --oauth-app to
specify a named app from config.toml.

Examples:
  msgvault add-account you@gmail.com
  msgvault add-account you@gmail.com --headless
 msgvault add-account you@gmail.com --force
  msgvault add-account you@gmail.com --readonly
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

// addAccountGrantDecided reports whether the daemon authenticated a completed
// frontend grant decision before starting this subprocess.
//
// The flag is absent when a caller builds its own command without registering
// the internal flags, which several tests and embedders do. Absent means
// undecided — the conservative default, since it keeps the gate rather than
// skipping it.
func addAccountGrantDecided(cmd *cobra.Command) bool {
	// Only honored inside the daemon's CLI subprocess. Direct CLI use rejects
	// it, the generic CLI-run API rejects it, and the daemon appends it only
	// after validating its private runtime token.
	if !isDaemonCLISubprocess() {
		return false
	}
	flag := cmd.Flags().Lookup(addAccountGrantDecidedFlag)
	return flag != nil && flag.Value.String() == "true"
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
//
// Callers MUST build the manager before --force deletes the token. The scope
// list is derived from the grant on disk, so probing after deletion would find
// nothing to preserve and quietly discard the account's Calendar/Drive access
// along with the Gmail scopes --force meant to reset.
func newAddAccountOAuthManager(clientSecretsPath, email string) (*oauth.Manager, error) {
	scopeProbe, err := oauth.NewManager(clientSecretsPath, cfg.TokensDir(), logger)
	if err != nil {
		return nil, wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
	}
	oauthScopes := addAccountOAuthScopesForToken(
		scopeProbe.HasScopeMetadata(email),
		scopeProbe.GrantedScopes(email),
		readonlyGrant,
	)
	mgr, err := oauth.NewManagerWithScopes(clientSecretsPath, cfg.TokensDir(), logger, oauthScopes)
	if err != nil {
		return nil, wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
	}
	return mgr, nil
}

// addAccountTokenReusable reports whether the stored token can be reused
// without a fresh authorization. A known client mismatch is never reusable.
// Named-app and authenticated-preflight paths also reject unknown client
// provenance, while the ordinary implicit-default path retains its legacy
// tolerance for tokens without client metadata.
//
// grantDecided is true only in the daemon subprocess after an authenticated
// frontend preflight minted the token. That token can be registered even when
// the provider returned wider scopes than requested; the frontend already
// warned about the grant, and reauthorizing cannot narrow it.
func addAccountTokenReusable(
	mgr *oauth.Manager,
	email string,
	binding addAccountBinding,
	grantDecided bool,
) bool {
	if !mgr.HasToken(email) || mgr.TokenIssuedByDifferentClient(email) {
		return false
	}
	needsClientCheck := grantDecided || binding.bindingChanged || binding.explicit ||
		binding.resolvedApp != ""
	if needsClientCheck && !mgr.TokenMatchesClient(email) {
		return false
	}
	return grantDecided || addAccountTokenHasGmailScopes(mgr, email, readonlyGrant)
}

// addAccountAuthorizeError decorates an authorization failure with the
// re-add hint when the consent screen authenticated a different address
// than the one being added.
//
// The hint repeats the flags that determine the grant. Printing a bare
// add-account to someone who ran --readonly would have them request write
// access on the retry, silently undoing the narrowing they asked for.
func addAccountAuthorizeError(err error, sourceExists bool) error {
	var mismatch *oauth.TokenMismatchError
	if errors.As(err, &mismatch) && !sourceExists {
		return fmt.Errorf(
			"%w\nIf %s is the primary address, re-add with:\n"+
				"  msgvault add-account %s%s",
			err, mismatch.Actual, mismatch.Actual, addAccountGrantFlagSuffix(),
		)
	}
	return fmt.Errorf("authorization failed: %w", err)
}

// readonlyGrantWarning checks what actually came back after a --readonly
// authorization. Requesting read-only does not by itself guarantee a read-only
// grant: the authorization server decides what to issue, and a response can
// carry scopes that were not requested. Rather than trust the request, read the
// recorded grant and say so if any write scope survived.
//
// It warns rather than failing, for two reasons. The token is already written
// and is the account's only credential, so deleting it would trade a too-wide
// grant for no access at all. And aborting here would stop short of registering
// the source, leaving a token on disk that no other command can see — while the
// obvious retry is refused, because the grant now carries the very write access
// the refusal exists to catch. Warning keeps the account working and puts the
// remedy in the operator's hands.
func readonlyGrantWarning(mgr *oauth.Manager, email, resolvedApp string) string {
	granted := oauth.GrantedGmailWriteScopes(mgr.GrantedScopes(email))
	if len(granted) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"Warning: authorization for %s returned Gmail write access (%s) despite --readonly.\n"+
			"The grant is wider than requested. Removing it takes the same steps as any\n"+
			"other narrowing — revoking alone will not do it, because the refusal reads the\n"+
			"scopes recorded in the token file:\n%s",
		email, strings.Join(granted, ", "),
		narrowingSteps(email, mgr.TokenPath(email), resolvedApp),
	)
}

// warnOnWiderThanRequestedGrant prints the readonly grant warning, if any.
func warnOnWiderThanRequestedGrant(out io.Writer, mgr *oauth.Manager, email, resolvedApp string) {
	if !readonlyGrant {
		return
	}
	if warning := readonlyGrantWarning(mgr, email, resolvedApp); warning != "" {
		_, _ = fmt.Fprintln(out, warning)
	}
}

// addAccountGrantFlagSuffix renders the grant-affecting flags of the current
// run for inclusion in remediation commands.
func addAccountGrantFlagSuffix() string {
	if readonlyGrant {
		return " --readonly"
	}
	return ""
}

// runAddAccountHTTP completes any needed browser authorization in this
// process — which owns the user's display and browser — before proxying to
// the daemon. The subprocess then finds a fresh reusable token, so it never
// opens a browser (or waits on a human) while holding the operation gate.
func runAddAccountHTTP(cmd *cobra.Command, args []string) error {
	if cmd.Flags().Changed(addAccountGrantDecidedFlag) {
		return usageErr(cmd, fmt.Errorf("--%s is internal and cannot be supplied", addAccountGrantDecidedFlag))
	}
	grantDecided := false
	if !headless {
		var err error
		grantDecided, err = preflightAddAccountAuthorize(cmd, args[0])
		if err != nil {
			return err
		}
	}
	return runDaemonCLICommandHTTPFromCobraWithGrantDecision(cmd, args, grantDecided)
}

func preflightAddAccountAuthorize(cmd *cobra.Command, email string) (bool, error) {
	if IsRemoteMode() {
		// Tokens live on the remote host; authorization must happen there.
		return false, nil
	}
	storedApp, sourceExists, err := lookupGmailAccountBinding(cmd.Context(), email)
	if err != nil {
		return false, err
	}
	binding := resolveAddAccountBinding(oauthAppName, cmd.Flags().Changed("oauth-app"), storedApp, sourceExists)
	if cfg.OAuth.ServiceAccountKeyFor(binding.resolvedApp) != "" {
		// Service accounts mint tokens on demand; no browser involved.
		return false, nil
	}
	clientSecretsPath, err := cfg.OAuth.ClientSecretsFor(binding.resolvedApp)
	if err != nil {
		// Let the subprocess report the configuration error.
		return false, nil //nolint:nilerr // deliberate: config errors surface daemon-side
	}
	mgr, err := newAddAccountOAuthManager(clientSecretsPath, email)
	if err != nil {
		return false, err
	}
	if err := applyAddAccountGrantDecision(cmd.OutOrStdout(), mgr, email, binding.resolvedApp); err != nil {
		return false, err
	}
	if forceReauth {
		if mgr.HasToken(email) {
			fmt.Printf("Removing existing token for %s...\n", email)
			if err := mgr.DeleteToken(email); err != nil {
				return false, fmt.Errorf("delete existing token: %w", err)
			}
		} else {
			fmt.Printf("No existing token found for %s, proceeding with authorization.\n", email)
		}
	}
	if addAccountTokenReusable(mgr, email, binding, false) {
		return false, nil
	}

	if binding.bindingChanged {
		fmt.Printf("Switching OAuth app for %s to %q. Authorizing...\n", email, oauthAppName)
	} else {
		fmt.Println("Starting browser authorization...")
	}
	if err := mgr.Authorize(cmd.Context(), email); err != nil {
		return false, addAccountAuthorizeError(err, sourceExists)
	}
	warnOnWiderThanRequestedGrant(cmd.OutOrStdout(), mgr, email, binding.resolvedApp)
	// The grant decision is a pre-authorization gate, and it has now been made
	// for this run. The daemon request carries a runtime-token-authenticated
	// proof so its subprocess registers the account instead of
	// re-litigating the grant: if the authorization above came back wider than
	// requested, the subprocess would otherwise refuse the very token this
	// process just minted, leaving it on disk with no source row and the
	// obvious retry blocked by the same refusal.
	// The subprocess must not force-delete the token minted above.
	if forceReauth {
		if err := cmd.Flags().Set("force", "false"); err != nil {
			return false, fmt.Errorf("clear --force after authorization: %w", err)
		}
	}
	return true, nil
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
		// The grant decision applies here too. --headless only prints
		// instructions, but those instructions are what the operator then runs
		// on the browser machine: telling someone to widen a narrowed account,
		// or handing them a --readonly recipe that will be refused when they
		// get there, is the same failure as doing it directly.
		if err := applyHeadlessGrantDecision(cmd, email, resolvedApp); err != nil {
			return err
		}
		oauth.PrintHeadlessInstructions(email, cfg.TokensDir(), resolvedApp, readonlyGrant)
		return nil
	}

	// Check for service account configuration first
	if saKeyPath != "" {
		if forceReauth {
			return usageErr(cmd, errors.New("service accounts do not use --force; tokens are minted on demand from the configured service account key"))
		}
		if readonlyGrant {
			return usageErr(cmd, errors.New("service accounts do not use --readonly; scopes come from the domain-wide delegation grant configured in the Workspace admin console"))
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

	// Refuse or warn about the grant change while the existing token is still
	// on disk — --force below removes the evidence this decision needs.
	// Skipped when the frontend CLI already decided and authorized; see
	// addAccountGrantDecidedFlag.
	grantDecided := addAccountGrantDecided(cmd)
	if !grantDecided {
		if err := applyAddAccountGrantDecision(cmd.OutOrStdout(), oauthMgr, email, resolvedApp); err != nil {
			return err
		}
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

	tokenReusable := !forceReauth && addAccountTokenReusable(oauthMgr, email, binding, grantDecided)
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

	// Perform authorization. Local frontends preflight the browser flow
	// before proxying, so a subprocess normally finds a fresh token above;
	// this path still runs for remote daemons, where the browser opens on
	// the daemon's host exactly as it did before daemon routing.
	if bindingChanged {
		fmt.Printf("Switching OAuth app for %s to %q. Authorizing...\n", email, oauthAppName)
	} else {
		fmt.Println("Starting browser authorization...")
	}

	if err := oauthMgr.Authorize(cmd.Context(), email); err != nil {
		return addAccountAuthorizeError(err, existingSource != nil)
	}
	warnOnWiderThanRequestedGrant(cmd.OutOrStdout(), oauthMgr, email, resolvedApp)

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

// addAccountOAuthScopesForToken builds the scope list to request. Existing
// grants are carried forward because Google's replacement consent would
// otherwise drop Calendar/Drive from the shared token file.
//
// Under readonly the Gmail write scopes are subtracted from those carried-over
// grants and only gmail.readonly is required, which is what makes narrowing
// actually narrow. Everything non-Gmail passes through untouched.
func addAccountOAuthScopesForToken(hasScopeMetadata bool, existingScopes []string, readonly bool) []string {
	required := oauth.Scopes
	if readonly {
		required = oauth.ScopesGmailReadonly
	}
	if !hasScopeMetadata {
		return append([]string(nil), required...)
	}
	scopes := append([]string(nil), existingScopes...)
	if readonly {
		scopes = oauth.WithoutGmailWriteScopes(scopes)
	}
	for _, scope := range required {
		scopes = appendScopeIfMissing(scopes, scope)
	}
	return scopes
}

// addAccountTokenHasGmailScopes reports whether the stored token already
// covers what this run needs, so the token can be reused without a fresh
// consent. Under readonly, gmail.readonly on its own is enough — re-authorizing
// an account that is already narrowed would be a pointless browser round trip.
func addAccountTokenHasGmailScopes(mgr *oauth.Manager, email string, readonly bool) bool {
	if !mgr.HasScopeMetadata(email) {
		// A token predating scope recording satisfies a default run, whose
		// scope set it almost certainly already matches. It cannot satisfy a
		// readonly run: reusing an unverifiable grant would treat "unknown" as
		// "already narrow". decideAddAccountGrant refuses this case before it
		// reaches here, so this is a guard rather than the primary check.
		return !readonly
	}
	if readonly {
		// Read access is necessary but not sufficient: gmail.readonly is part
		// of the default scope set, so an ordinary write-capable token has it
		// too. Reusing one would report success over a grant --readonly exists
		// to avoid. decideAddAccountGrant refuses that case first, but this
		// must not depend on the gate upstream staying in place.
		return mgr.HasScope(email, oauth.ScopeGmailReadonly) &&
			!oauth.HasGmailWriteScope(mgr.GrantedScopes(email))
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

// addAccountGrantDecision is the verdict on an account's current grant versus
// what this run is about to request.
type addAccountGrantDecision struct {
	// Err refuses the run outright.
	Err error
	// Warning is printed before authorization proceeds.
	Warning string
}

// decideAddAccountGrant compares the account's current grant against the
// requested one. It is pure so both the local command and the daemon preflight
// reach the same verdict from the same inputs.
//
// Refusal (readonly over an existing write grant) is unconditional. Neither
// --force nor --headless is consulted, because neither changes the outcome:
// re-authorizing does not revoke the previous grant, and revoking it would
// invalidate the replacement, so there is no flag combination that narrows
// access already granted.
//
// The widening warning fires only when there is an existing narrower Gmail
// grant to widen. A brand-new account is the common case and a warning there
// would be noise; an account holding only Calendar/Drive is likewise getting
// its first Gmail grant, not losing a narrowed one.
//
// A token predating scope metadata is refused under readonly for the same
// reason a known write grant is: msgvault cannot tell what it holds, and such
// tokens were minted when read plus modify was the only scope set, so treating
// "unknown" as "narrow enough" would report success over a still-write-capable
// grant. Only readonly is affected; every other path keeps its existing
// tolerance for legacy tokens.
func decideAddAccountGrant(
	email string,
	hasToken bool,
	hasScopeMetadata bool,
	grantedScopes []string,
	readonly bool,
	freshClient bool,
	tokenPath string,
	resolvedApp string,
) addAccountGrantDecision {
	if !hasToken {
		return addAccountGrantDecision{}
	}
	if readonly {
		// Consenting to a genuinely different OAuth client means a grant that
		// does not exist yet, so nothing is carried forward and this is not
		// narrowing in place. The caller must confirm the client really is
		// different — two app names can resolve to the same client_secret.json,
		// and treating that as fresh would wave the existing write grant
		// through untouched.
		//
		// --force deliberately does NOT waive the refusal. It deletes the local
		// token and obtains a narrower one, but the grant it replaces stays
		// live at Google: a refresh token issued beforehand keeps working with
		// its original write scopes. Allowing --force here would hand back a
		// read-only token while write access remained in existence, which is
		// the appearance of narrowing rather than narrowing.
		if freshClient {
			return addAccountGrantDecision{}
		}
		if !hasScopeMetadata {
			return addAccountGrantDecision{Err: fmt.Errorf(
				"%s has a token that predates scope recording, so its Gmail access cannot be verified\n"+
					"Tokens this old were issued with read and modify access, which --readonly "+
					"cannot take away on its own.\n%s",
				email, narrowingRemedy(email, tokenPath, resolvedApp),
			)}
		}
		if granted := oauth.GrantedGmailWriteScopes(grantedScopes); len(granted) > 0 {
			return addAccountGrantDecision{Err: fmt.Errorf(
				"%s already has Gmail write access (%s)\n"+
					"Re-authorizing preserves scopes the account already holds, so --readonly "+
					"alone cannot take write access away.\n"+
					"Non-Gmail grants such as Calendar are preserved either way.\n%s",
				email, strings.Join(granted, ", "), narrowingRemedy(email, tokenPath, resolvedApp),
			)}
		}
		return addAccountGrantDecision{}
	}
	if oauth.IsNarrowedGmailGrant(grantedScopes) {
		return addAccountGrantDecision{Warning: fmt.Sprintf(
			"Warning: %s currently has read-only Gmail access (%s).\n"+
				"Continuing will request write access (%s).\n"+
				"Re-run with --readonly to keep the narrower grant.",
			email,
			strings.Join(gmailScopesIn(grantedScopes), ", "),
			strings.Join(oauth.Scopes, ", "),
		)}
	}
	return addAccountGrantDecision{}
}

// refuseReadonlyUnderAliasSpelling fails a read-only run when a token is
// stored under a Gmail alias spelling of email. Proceeding would mint or
// reuse a read-only token for this spelling while the other spelling's
// credential kept whatever access it has — reported as a successful
// read-only setup.
//
// With no token under this spelling the remedy is simply to use the stored
// one, where the normal grant decision applies. When both spellings hold
// tokens, that redirect would bounce off the mirror-image refusal, so the
// remedy is the same revoke-and-re-add procedure as any other narrowing —
// revocation at Google clears the account's grant for the client, so both
// credentials die together — with every duplicate file removed before the
// single re-add.
func refuseReadonlyUnderAliasSpelling(mgr *oauth.Manager, email, resolvedApp string) error {
	equivalents := mgr.FindEquivalentTokenEmails(email)
	equivalents = slices.DeleteFunc(equivalents, mgr.TokenIssuedByDifferentClient)
	if len(equivalents) == 0 {
		return nil
	}
	flags := addAccountReadonlyRemediationFlagSuffix(resolvedApp)
	if !mgr.HasToken(email) || mgr.TokenIssuedByDifferentClient(email) {
		return fmt.Errorf(
			"%s refers to the same Google account as %s, which has a stored token\n"+
				"A read-only setup under a second spelling would leave that token's "+
				"access in place, unnarrowed.\n"+
				"Use the stored spelling instead:\n"+
				"  msgvault add-account %s%s",
			email, equivalents[0], equivalents[0], flags)
	}
	removals := []string{"  2. rm " + oauth.ShellQuote(mgr.TokenPath(email))}
	for _, equivalent := range equivalents {
		removals = append(removals, "     rm "+oauth.ShellQuote(mgr.TokenPath(equivalent)))
	}
	return fmt.Errorf(
		"%s and %s hold stored tokens for the same Google account\n"+
			"A read-only decision cannot be made while both exist.\n"+
			"To make this account read-only, remove its access and grant it again:\n"+
			"  1. Revoke msgvault at https://myaccount.google.com/permissions\n"+
			"%s\n"+
			"  3. msgvault add-account %s%s\n"+
			"Revoking clears every other Google scope for this account, so re-run the\n"+
			"commands that granted them (msgvault add-calendar, add-synctech-sms-drive).\n"+
			"%s",
		email, strings.Join(equivalents, ", "), strings.Join(removals, "\n"), email, flags,
		"Archived mail is not affected.")
}

func addAccountReadonlyRemediationFlagSuffix(resolvedApp string) string {
	var suffix string
	if resolvedApp != "" {
		suffix = " --oauth-app " + oauth.ShellQuote(resolvedApp)
	}
	return suffix + " --readonly"
}

// narrowingRemedy renders the way out of a refusal.
//
// Access already granted cannot be narrowed in place, and no flag changes that.
// Re-authorizing does not revoke the previous grant — a refresh token issued
// beforehand keeps working with its original scopes — and revoking is
// all-or-nothing for the client, so revoking the old credential would destroy
// the replacement too. The only sequence that produces a genuinely read-only
// account is to remove the grant at Google and create it again.
//
// The procedure is identical on a headless host, so there is no variant: the
// authorization flow prints a URL and waits on its loopback callback, which can
// be completed from any browser.
//
// tokenPath is passed in rather than derived from the global config, so this
// and decideAddAccountGrant stay pure and testable.
func narrowingRemedy(email, tokenPath, resolvedApp string) string {
	return "Access already granted cannot be narrowed: re-authorizing does not revoke it,\n" +
		"and revoking applies to the whole grant.\n" +
		narrowingSteps(email, tokenPath, resolvedApp)
}

// narrowingSteps is the procedure itself, shared by the refusal and by the
// wider-than-requested warning so the two cannot drift. Step 2 is not optional
// and revoking alone will not do: the refusal reads the scopes recorded in the
// token file, which revoking at Google does not touch.
//
// The path is shell-quoted because the tokens directory comes from
// MSGVAULT_HOME, --home, or [data].data_dir, any of which may contain a space —
// and an unquoted rm would then take two operands and silently do nothing.
func narrowingSteps(email, tokenPath, resolvedApp string) string {
	flags := addAccountReadonlyRemediationFlagSuffix(resolvedApp)
	return fmt.Sprintf(
		"To make this account read-only, remove its access and grant it again:\n"+
			"  1. Revoke msgvault at https://myaccount.google.com/permissions\n"+
			"  2. rm %s\n"+
			"  3. msgvault add-account %s%s\n"+
			"Revoking clears every other Google scope for this account, so re-run the\n"+
			"commands that granted them (msgvault add-calendar, add-synctech-sms-drive).\n"+
			"Archived mail is not affected.",
		oauth.ShellQuote(tokenPath), email, flags,
	)
}

// gmailScopesIn returns the Gmail scopes present in scopes, so the widening
// warning can name the current grant precisely instead of describing it.
func gmailScopesIn(scopes []string) []string {
	var gmail []string
	for _, scope := range scopes {
		if oauth.HasAnyGmailScope([]string{scope}) {
			gmail = append(gmail, scope)
		}
	}
	return gmail
}

// applyHeadlessGrantDecision runs the grant decision for a --headless run,
// which prints instructions rather than authorizing and so never reaches the
// usual decision point.
//
// It is best-effort by design: --headless still prints for a new account when
// OAuth credentials are not configured. A read-only run with an exact or
// equivalent stored token fails closed, because its client cannot be verified.
func applyHeadlessGrantDecision(cmd *cobra.Command, email, resolvedApp string) error {
	clientSecretsPath, err := cfg.OAuth.ClientSecretsFor(resolvedApp)
	if err != nil {
		if !readonlyGrant {
			return nil // --headless still prints without configured credentials
		}
		if oauth.StoredTokenOrEquivalentExists(cfg.TokensDir(), email) {
			return fmt.Errorf(
				"%s has a stored token, but its Gmail access cannot be verified without OAuth client credentials: %w",
				email, err,
			)
		}
		return nil // --headless works without configured credentials for a new account
	}
	mgr, err := oauth.NewManager(clientSecretsPath, cfg.TokensDir(), logger)
	if err != nil {
		// Unreadable or malformed credentials are a real problem, not the
		// "no credentials configured" case above, and silently skipping the
		// gate would print instructions that widen a narrowed account. Say so
		// and print nothing rather than guess.
		return wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
	}
	return applyAddAccountGrantDecision(cmd.OutOrStdout(), mgr, email, resolvedApp)
}

// applyAddAccountGrantDecision reports the verdict, returning an error when
// the run must stop. It must run before any authorization and, critically,
// before --force deletes the token: once the token is gone there is no grant
// left to compare against.
func applyAddAccountGrantDecision(out io.Writer, mgr *oauth.Manager, email, resolvedApp string) error {
	// Authorization accepts Gmail alias spellings (dots, plus-addresses,
	// case, googlemail.com) as the same account, so a read-only decision
	// must not treat an exact-match miss as a fresh account while an
	// equivalent spelling holds a credential. Refused for --readonly only;
	// default runs keep their existing behavior.
	if readonlyGrant {
		if err := refuseReadonlyUnderAliasSpelling(mgr, email, resolvedApp); err != nil {
			return err
		}
	}
	// The token's own client_id is the whole question: a grant belonging to a
	// different client is not one --readonly could narrow. Deliberately not
	// gated on bindingChanged, which requires an existing source row and so
	// missed both a copied token with no row yet and an inherited binding.
	// TokenIssuedByDifferentClient already answers conservatively, returning
	// false when provenance is unknown.
	freshClient := mgr.TokenIssuedByDifferentClient(email)
	decision := decideAddAccountGrant(
		email,
		mgr.HasToken(email),
		mgr.HasScopeMetadata(email),
		mgr.GrantedScopes(email),
		readonlyGrant,
		freshClient,
		mgr.TokenPath(email),
		resolvedApp,
	)
	if decision.Err != nil {
		return decision.Err
	}
	if decision.Warning != "" {
		if _, err := fmt.Fprintln(out, decision.Warning); err != nil {
			return fmt.Errorf("write grant warning: %w", err)
		}
	}
	return nil
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
	cmd.Flags().BoolVar(&readonlyGrant, "readonly", false, "Request Gmail read-only access instead of read+write (refused if the account already holds write access)")
	cmd.Flags().Bool(addAccountGrantDecidedFlag, false, "Internal: the grant decision was already applied by the frontend CLI")
	if err := cmd.Flags().MarkHidden(addAccountGrantDecidedFlag); err != nil {
		panic(fmt.Sprintf("mark --%s hidden: %v", addAccountGrantDecidedFlag, err))
	}
}

func init() {
	registerAddAccountFlags(addAccountCmd)
	rootCmd.AddCommand(newAddAccountCmd())
}
