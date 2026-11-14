package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

var (
	o365Headless             bool
	o365TenantID             string
	noDefaultIdentityAddO365 bool
	o365Graph                bool
	o365SignIn               string
	o365OAuthApp             string
)

func newAddO365Cmd() *cobra.Command {
	cmd := newAddO365LocalCmd()
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			email, err := preflightAddO365Authorize(cmd, args[0])
			if err != nil {
				return err
			}
			args[0] = email
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runAddO365Local(cmd, args)
	}
	return cmd
}

// preflightAddO365Authorize runs the Microsoft browser flow in this process
// before proxying, so the daemon subprocess never opens a browser or waits
// on human consent while holding the operation gate.
func preflightAddO365Authorize(cmd *cobra.Command, email string) (string, error) {
	mailWrite, err := o365MailWrite(cmd)
	if err != nil {
		return "", err
	}
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return "", errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	if IsRemoteMode(state) {
		return email, nil
	}
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return "", err
	}
	accounts, _, err := st.GetCLIAccounts(cmd.Context())
	_ = st.Close()
	if err != nil {
		return "", fmt.Errorf("look up existing source: %w", err)
	}
	account, undetermined, err := selectO365AccountForCommand(cmd, accounts, email, o365Graph)
	if err != nil {
		return "", err
	}
	explicit := cmd.Flags().Changed("oauth-app")
	if undetermined {
		return "", fmt.Errorf("daemon does not report the IMAP authentication method for %s; update the daemon or check the account's authentication configuration", email)
	}
	var stored sql.NullString
	exists := account != nil
	if exists {
		stored = sql.NullString{String: account.OAuthApp, Valid: account.OAuthApp != ""}
		if o365Graph {
			email = account.Identifier
		} else {
			parsed, err := imapclient.ParseIdentifier(account.Identifier)
			if err != nil {
				return "", err
			}
			email = parsed.Username
		}
	}
	binding := resolveAddAccountBinding(o365OAuthApp, explicit, stored, exists)
	app, dir, err := microsoftMailApp(cfg, binding.resolvedApp)
	if err != nil {
		return "", err
	}
	if app.ClientID == "" {
		return "", requireMicrosoftOAuthConfig(cfg)
	}
	if err := authorizeO365(cmd, email, mailWrite, app, dir); err != nil {
		return "", err
	}
	if err := cmd.Flags().Set("oauth-app", binding.resolvedApp); err != nil {
		return "", fmt.Errorf("set --oauth-app after authorization: %w", err)
	}
	if err := cmd.Flags().Set(oauthPreflightedFlag, "true"); err != nil {
		return "", fmt.Errorf("set --%s after authorization: %w", oauthPreflightedFlag, err)
	}
	return email, nil
}

func selectO365AccountForCommand(cmd *cobra.Command, accounts []daemonclient.CLIAccount, email string, graph bool) (*daemonclient.CLIAccount, bool, error) {
	if !cmd.Flags().Changed("source-id") {
		return selectO365Account(accounts, email, graph)
	}
	id, err := cmd.Flags().GetInt64("source-id")
	if err != nil {
		return nil, false, fmt.Errorf("read --source-id: %w", err)
	}
	if id <= 0 {
		return nil, false, errors.New("source ID must be positive")
	}
	for _, account := range accounts {
		if account.ID != id {
			continue
		}
		match, unknown := matchesO365Account(account, email, graph)
		if !match && !unknown && (graph || !isO365PasswordConflict(account, email)) {
			return nil, false, fmt.Errorf("source %d does not match Microsoft mail account %s", id, email)
		}
		return selectO365Account([]daemonclient.CLIAccount{account}, email, graph)
	}
	return nil, false, fmt.Errorf("source %d does not match Microsoft mail account %s", id, email)
}

func preferExactO365Accounts(accounts []daemonclient.CLIAccount, email string, graph bool) []daemonclient.CLIAccount {
	var exact bool
	for _, account := range accounts {
		match, _ := matchesO365Account(account, email, graph)
		if account.MergedIntoSourceID == 0 && match && o365AccountMailbox(account, graph) == email {
			exact = true
			break
		}
	}
	if !exact {
		return accounts
	}
	filtered := make([]daemonclient.CLIAccount, 0, len(accounts))
	for _, account := range accounts {
		match, _ := matchesO365Account(account, email, graph)
		if match && o365AccountMailbox(account, graph) != email {
			continue
		}
		filtered = append(filtered, account)
	}
	return filtered
}

func o365AccountMailbox(account daemonclient.CLIAccount, graph bool) string {
	if graph {
		return account.Identifier
	}
	cfg, _ := imapclient.ParseIdentifier(account.Identifier)
	return cfg.Username
}

func o365IMAPHostForSource(host, email string, source *store.Source, sources []*store.Source) string {
	if source == nil {
		return host
	}
	identifier := (&imapclient.Config{Host: host, Port: 993, TLS: true, Username: email}).Identifier()
	for _, other := range sources {
		if other.ID != source.ID && other.Identifier == identifier {
			cfg, err := imapclient.ParseIdentifier(source.Identifier)
			if err == nil {
				return cfg.Host
			}
		}
	}
	return host
}

func selectO365Account(accounts []daemonclient.CLIAccount, email string, graph bool) (*daemonclient.CLIAccount, bool, error) {
	var selected *daemonclient.CLIAccount
	unknown := false
	retired := false
	for _, account := range preferExactO365Accounts(accounts, email, graph) {
		if account.MergedIntoSourceID != 0 {
			match, undetermined := matchesO365Account(account, email, graph)
			retired = retired || match || undetermined || (!graph && isO365PasswordConflict(account, email))
			continue
		}
		if !graph && isO365PasswordConflict(account, email) {
			return nil, false, fmt.Errorf("an Outlook IMAP account for %s already uses password authentication; remove or change that account before adding Microsoft OAuth", email)
		}
		match, undetermined := matchesO365Account(account, email, graph)
		if match {
			if selected != nil {
				return nil, false, fmt.Errorf("multiple Microsoft mail sources match %s; find its ID with list-accounts --json and choose it with --source-id", email)
			}
			selected = &account
		}
		unknown = unknown || undetermined
	}
	if selected == nil && retired {
		return nil, false, fmt.Errorf("microsoft mail source for %s is retired: %w", email, store.ErrSourceRetired)
	}
	return selected, unknown, nil
}

func matchesO365Account(account daemonclient.CLIAccount, email string, graph bool) (bool, bool) {
	if graph {
		return account.Type == sourceTypeMSMail && strings.EqualFold(account.Identifier, email), false
	}
	if account.Type != sourceTypeIMAP {
		return false, false
	}
	parsed, err := imapclient.ParseIdentifier(account.Identifier)
	if err != nil || !parsed.TLS || parsed.Port != 993 || !strings.EqualFold(parsed.Username, email) || (!strings.EqualFold(parsed.Host, "outlook.office365.com") && !strings.EqualFold(parsed.Host, "outlook.office.com")) {
		return false, false
	}
	return account.AuthMethod == string(imapclient.AuthXOAuth2), account.AuthMethod != string(imapclient.AuthXOAuth2) && account.AuthMethod != string(imapclient.AuthPassword)
}

func isO365PasswordConflict(account daemonclient.CLIAccount, email string) bool {
	if account.Type != sourceTypeIMAP || account.AuthMethod != string(imapclient.AuthPassword) {
		return false
	}
	cfg, err := imapclient.ParseIdentifier(account.Identifier)
	return err == nil && cfg.TLS && cfg.Port == 993 && strings.EqualFold(cfg.Username, email) && (strings.EqualFold(cfg.Host, "outlook.office365.com") || strings.EqualFold(cfg.Host, "outlook.office.com"))
}

func newAddO365LocalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-o365 <email>",
		Short: "Add a Microsoft 365 account via OAuth",
		Long: `Add a Microsoft 365 / Outlook.com email account using OAuth2 authentication.

This opens a browser for Microsoft authorization (or, with --headless, prints a
device code to enter on any device), then configures IMAP access
to outlook.office365.com automatically using the XOAUTH2 SASL mechanism.

Requires a [microsoft] section in config.toml with your Azure AD app's client_id.
See the docs for Azure AD app registration setup.

With --graph, the account syncs through the Microsoft Graph mail API instead
of IMAP. Use it when IMAP is turned off for the mailbox. It needs the Mail.Read
permission on the app registration. The first delete-staged for the account
asks for Mail.ReadWrite, which the app registration must also list. A Graph
account can request it during sign-in with --mail-write for category editing.
The Graph account is separate: if the mailbox is also synced over IMAP, the
vault holds two copies, and 'msgvault dedup --collection' hides the extra ones.

Examples:
  msgvault add-o365 user@outlook.com
  msgvault add-o365 user@outlook.com --headless
  msgvault add-o365 user@company.com --tenant my-tenant-id
  msgvault add-o365 user@company.com --sign-in jdoe@company.onmicrosoft.com
  msgvault add-o365 user@company.com --graph`,
		Args: cobra.ExactArgs(1),
		RunE: runAddO365Local,
	}
	cmd.Flags().StringVar(&o365OAuthApp, "oauth-app", "", "Microsoft OAuth app name")
	cmd.Flags().StringVar(&o365TenantID, "tenant", "",
		"Azure AD tenant ID (default: \"common\" for multi-tenant)")
	cmd.Flags().BoolVar(&noDefaultIdentityAddO365, "no-default-identity", false, savedDefaultIdentityHelp)
	cmd.Flags().BoolVar(&o365Headless, "headless", false,
		"Sign in with a device code instead of a local browser")
	cmd.Flags().BoolVar(&o365Graph, "graph", false, "sync through the Microsoft Graph mail API instead of IMAP")
	cmd.Flags().StringVar(&o365SignIn, "sign-in", "",
		"Microsoft sign-in name and browser login hint when it differs from the mailbox address (IMAP only)")
	cmd.MarkFlagsMutuallyExclusive("graph", "sign-in")
	cmd.Flags().Bool("mail-write", false, "With --graph, authorize category editing and remote deletion")
	registerO365Flags(cmd)
	return cmd
}

func registerO365Flags(cmd *cobra.Command) {
	cmd.Flags().Int64("source-id", 0, "Existing source ID to reauthorize")
	registerOAuthPreflightedFlag(cmd)
}

func runAddO365Local(cmd *cobra.Command, args []string) error {
	mailWrite, err := o365MailWrite(cmd)
	if err != nil {
		return err
	}
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	logger := state.logger
	email := args[0]

	s, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	typ := sourceTypeIMAP
	if o365Graph {
		typ = sourceTypeMSMail
	}
	existing, err := s.ListSources(typ)
	if err != nil {
		return fmt.Errorf("look up existing source: %w", err)
	}
	accounts := make([]daemonclient.CLIAccount, 0, len(existing))
	for _, src := range existing {
		account := daemonclient.CLIAccount{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier, OAuthApp: sourceOAuthApp(src), MergedIntoSourceID: src.MergedIntoSourceID}
		if src.SyncConfig.Valid {
			if imapCfg, parseErr := imapclient.ConfigFromJSON(src.SyncConfig.String); parseErr == nil {
				account.AuthMethod = string(imapCfg.EffectiveAuthMethod())
			}
		}
		accounts = append(accounts, account)
	}
	account, unknown, err := selectO365AccountForCommand(cmd, accounts, email, o365Graph)
	if err != nil {
		return err
	}
	if unknown {
		return fmt.Errorf("cannot determine the IMAP authentication method for %s; update the daemon or check the account's authentication configuration", email)
	}
	var source *store.Source
	if account != nil {
		for _, src := range existing {
			if src.ID == account.ID {
				source = src
				break
			}
		}
	}
	stored := sql.NullString{}
	if source != nil {
		stored = source.OAuthApp
	}
	binding := resolveAddAccountBinding(o365OAuthApp, cmd.Flags().Changed("oauth-app"), stored, source != nil)
	if source != nil {
		if o365Graph {
			email = source.Identifier
		} else {
			parsed, err := imapclient.ParseIdentifier(source.Identifier)
			if err != nil {
				return fmt.Errorf("parse source identifier: %w", err)
			}
			email = parsed.Username
		}
	}
	app, dir, err := microsoftMailApp(cfg, binding.resolvedApp)
	if err != nil {
		return err
	}
	if app.ClientID == "" {
		return requireMicrosoftOAuthConfig(cfg)
	}
	if binding.bindingChanged {
		fmt.Println("Switching OAuth app...")
	}
	if o365Graph {
		return runAddO365GraphLocal(cmd, email, mailWrite, s, source, binding, app, dir)
	}
	msMgr := microsoft.NewManager(app.ClientID, o365AppTenant(app), app.EffectiveRedirectURI(), dir, logger)

	preflighted, err := oauthPreflighted(cmd)
	if err != nil {
		return err
	}
	if !preflighted {
		if err := authorizeO365(cmd, email, mailWrite, app, dir); err != nil {
			return err
		}
	}

	// Determine the correct IMAP host from the token that was just saved.
	// Personal accounts (hotmail.com, outlook.com, etc.) use outlook.office.com;
	// organizational accounts use outlook.office365.com.
	imapHost, err := msMgr.IMAPHost(email)
	if err != nil {
		return fmt.Errorf("determine IMAP host: %w", err)
	}

	imapCfg := &imapclient.Config{
		Host:       o365IMAPHostForSource(imapHost, email, source, existing),
		Port:       993,
		TLS:        true,
		Username:   email,
		AuthMethod: imapclient.AuthXOAuth2,
	}

	identifier := imapCfg.Identifier()

	cfgJSON, err := imapCfg.ToJSON()
	if err != nil {
		return fmt.Errorf("serialize config: %w", err)
	}
	source, err = s.RegisterMicrosoftMail(cmd.Context(), source, sourceTypeIMAP, identifier, sql.NullString{String: binding.resolvedApp, Valid: binding.resolvedApp != ""}, cfgJSON)
	if err != nil {
		return fmt.Errorf("store OAuth app: %w", err)
	}
	if err := updateSourceDisplayNameForRegistration(s, source.ID, email, state.logger); err != nil {
		return fmt.Errorf("set display name: %w", err)
	}

	// Auto-default-identity must run BEFORE the legacy migration
	// retry — see comment in account_identity.go.
	if err := setDefaultIdentityOptOut(cmd.Context(), cmd, s, source, noDefaultIdentityAddO365); err != nil {
		return err
	}
	if !noDefaultIdentityAddO365 {
		confirmDefaultIdentity(cmd.Context(), cmd.OutOrStdout(), s, source.ID, email, email, "account-identifier", state.logger)
	}
	if err := runPostSourceCreateMigrationsForInvocation(cmd.Context(), s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}

	fmt.Printf("\nMicrosoft 365 account added successfully!\n")
	fmt.Printf("  Email:      %s\n", email)
	fmt.Printf("  Identifier: %s\n", identifier)
	fmt.Println()
	fmt.Println("You can now run:")
	oauth.PrintHeadlessCommand(os.Stdout, "msgvault", "sync-full", "--", email)

	return nil
}

func o365MailWrite(cmd *cobra.Command) (bool, error) {
	write, err := cmd.Flags().GetBool("mail-write")
	if err != nil {
		return false, fmt.Errorf("read --mail-write flag: %w", err)
	}
	if write && !o365Graph {
		return false, errors.New("--mail-write requires --graph")
	}
	return write, nil
}

func o365AppTenant(app config.MicrosoftConfig) string {
	if o365TenantID != "" {
		return o365TenantID
	}
	return app.EffectiveTenantID()
}

// authorizeO365 runs the Microsoft sign-in for the account kind: Graph mail
// with --graph, IMAP otherwise.
func authorizeO365(cmd *cobra.Command, email string, mailWrite bool, app config.MicrosoftConfig, dir string) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	logger := state.logger
	tenant := o365AppTenant(app)
	redirect := app.EffectiveRedirectURI()
	fmt.Printf("Authorizing %s with Microsoft...\n", email)
	var err error
	if o365Graph {
		mgr := microsoft.NewGraphMailManager(app.ClientID, tenant, redirect, dir, logger)
		if mailWrite {
			mgr = microsoft.NewGraphMailWriteManager(app.ClientID, tenant, redirect, dir, logger)
		}
		if o365Headless {
			mgr.UseDeviceCode()
		}
		err = mgr.Authorize(cmd.Context(), email)
	} else {
		mgr := microsoft.NewManager(app.ClientID, tenant, redirect, dir, logger)
		if o365Headless {
			mgr.UseDeviceCode()
		}
		if o365SignIn != "" {
			mgr.UseSignInName(o365SignIn)
		}
		err = mgr.Authorize(cmd.Context(), email)
	}
	if err != nil {
		return fmt.Errorf("authorization failed: %w", err)
	}
	return nil
}

// runAddO365GraphLocal creates an msmail source, the Graph mail counterpart of
// the IMAP source that add-o365 makes by default.
func runAddO365GraphLocal(cmd *cobra.Command, email string, mailWrite bool, s *store.Store, source *store.Source, binding addAccountBinding, app config.MicrosoftConfig, dir string) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	preflighted, err := oauthPreflighted(cmd)
	if err != nil {
		return err
	}
	if !preflighted {
		if err := authorizeO365(cmd, email, mailWrite, app, dir); err != nil {
			return err
		}
	}

	source, err = s.RegisterMicrosoftMail(cmd.Context(), source, sourceTypeMSMail, email, sql.NullString{String: binding.resolvedApp, Valid: binding.resolvedApp != ""}, "")
	if err != nil {
		return fmt.Errorf("store OAuth app: %w", err)
	}
	if err := updateSourceDisplayNameForRegistration(s, source.ID, email, state.logger); err != nil {
		return fmt.Errorf("set display name: %w", err)
	}
	if err := setDefaultIdentityOptOut(cmd.Context(), cmd, s, source, noDefaultIdentityAddO365); err != nil {
		return err
	}
	if !noDefaultIdentityAddO365 {
		confirmDefaultIdentity(cmd.Context(), cmd.OutOrStdout(), s, source.ID, email, email, "account-identifier", state.logger)
	}
	if err := runPostSourceCreateMigrationsForInvocation(cmd.Context(), s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}

	fmt.Printf("\nMicrosoft 365 account added for Graph mail sync!\n")
	fmt.Printf("  Email: %s\n", email)
	fmt.Println()
	fmt.Println("You can now run:")
	oauth.PrintHeadlessCommand(os.Stdout, "msgvault", "sync", "--", email)
	return nil
}

func init() {
	rootCmd.AddCommand(newAddO365Cmd())
}
