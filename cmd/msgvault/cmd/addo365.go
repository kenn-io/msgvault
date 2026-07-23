package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/store"
)

var (
	o365TenantID             string
	noDefaultIdentityAddO365 bool
)

func newAddO365Cmd() *cobra.Command {
	cmd := newAddO365LocalCmd()
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			if err := preflightAddO365Authorize(cmd, args[0]); err != nil {
				return err
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runAddO365Local(cmd, args)
	}
	return cmd
}

// preflightAddO365Authorize runs the Microsoft browser flow in this process
// before proxying, so the daemon subprocess never opens a browser or waits
// on human consent while holding the operation gate.
func preflightAddO365Authorize(cmd *cobra.Command, email string) error {
	if IsRemoteMode() {
		// Tokens live on the remote host; authorization must happen there.
		return nil
	}
	if err := requireMicrosoftOAuthConfig(); err != nil {
		return err
	}
	msMgr := microsoft.NewManager(
		cfg.Microsoft.ClientID,
		microsoftTenantID(o365TenantID),
		cfg.Microsoft.EffectiveRedirectURI(),
		cfg.TokensDir(),
		logger,
	)
	fmt.Printf("Authorizing %s with Microsoft...\n", email)
	if err := msMgr.Authorize(cmd.Context(), email); err != nil {
		return fmt.Errorf("authorization failed: %w", err)
	}
	if err := cmd.Flags().Set(oauthPreflightedFlag, "true"); err != nil {
		return fmt.Errorf("set --%s after authorization: %w", oauthPreflightedFlag, err)
	}
	return nil
}

func newAddO365LocalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-o365 <email>",
		Short: "Add a Microsoft 365 account via OAuth",
		Long: `Add a Microsoft 365 / Outlook.com email account using OAuth2 authentication.

This opens a browser for Microsoft authorization, then configures IMAP access
to outlook.office365.com automatically using the XOAUTH2 SASL mechanism.

Requires a [microsoft] section in config.toml with your Azure AD app's client_id.
See the docs for Azure AD app registration setup.

Examples:
  msgvault add-o365 user@outlook.com
  msgvault add-o365 user@company.com --tenant my-tenant-id`,
		Args: cobra.ExactArgs(1),
		RunE: runAddO365Local,
	}
	cmd.Flags().StringVar(&o365TenantID, "tenant", "",
		"Azure AD tenant ID (default: \"common\" for multi-tenant)")
	cmd.Flags().BoolVar(&noDefaultIdentityAddO365, "no-default-identity", false, noDefaultIdentityHelp)
	registerOAuthPreflightedFlag(cmd)
	return cmd
}

func runAddO365Local(cmd *cobra.Command, args []string) error {
	email := args[0]

	if err := requireMicrosoftOAuthConfig(); err != nil {
		return err
	}

	msMgr := microsoft.NewManager(
		cfg.Microsoft.ClientID,
		microsoftTenantID(o365TenantID),
		cfg.Microsoft.EffectiveRedirectURI(),
		cfg.TokensDir(),
		logger,
	)

	preflighted, err := oauthPreflighted(cmd)
	if err != nil {
		return err
	}
	if !preflighted {
		fmt.Printf("Authorizing %s with Microsoft...\n", email)
		if err := msMgr.Authorize(cmd.Context(), email); err != nil {
			return fmt.Errorf("authorization failed: %w", err)
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
		Host:       imapHost,
		Port:       993,
		TLS:        true,
		Username:   email,
		AuthMethod: imapclient.AuthXOAuth2,
	}

	s, cleanup, err := openWritableStoreAndInitForIngest()
	if err != nil {
		return err
	}
	defer cleanup()

	identifier := imapCfg.Identifier()

	// If a Microsoft IMAP source with this email already exists (matched by
	// display name AND XOAUTH2 config), reuse it and update its identifier +
	// config in place. This handles re-authorization after a host change
	// (e.g. personal vs org scope correction changes the IMAP hostname).
	// We require the existing source to already be a Microsoft XOAUTH2 source
	// so that a non-Microsoft IMAP source sharing the same display name is
	// never silently repointed to Outlook XOAUTH2.
	var source *store.Source
	existing, err := s.GetSourcesByDisplayName(email)
	if err != nil {
		return fmt.Errorf("look up existing source: %w", err)
	}
	for _, src := range existing {
		if src.SourceType == sourceTypeIMAP && isMicrosoftIMAPSource(src, email) {
			source = src
			break
		}
	}

	if source != nil {
		if err := s.UpdateSourceIdentifier(source.ID, identifier); err != nil {
			return fmt.Errorf("update source identifier: %w", err)
		}
	} else {
		source, err = s.GetOrCreateSource(sourceTypeIMAP, identifier)
		if err != nil {
			return fmt.Errorf("create source: %w", err)
		}
	}
	cfgJSON, err := imapCfg.ToJSON()
	if err != nil {
		return fmt.Errorf("serialize config: %w", err)
	}
	if err := s.UpdateSourceSyncConfig(source.ID, cfgJSON); err != nil {
		return fmt.Errorf("store config: %w", err)
	}
	if err := s.UpdateSourceDisplayName(source.ID, email); err != nil {
		return fmt.Errorf("set display name: %w", err)
	}

	// Auto-default-identity must run BEFORE the legacy migration
	// retry — see comment in account_identity.go.
	if !noDefaultIdentityAddO365 {
		confirmDefaultIdentity(cmd.OutOrStdout(), s, source.ID, email, email, "account-identifier")
	}
	if err := runPostSourceCreateMigrations(s); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}

	fmt.Printf("\nMicrosoft 365 account added successfully!\n")
	fmt.Printf("  Email:      %s\n", email)
	fmt.Printf("  Identifier: %s\n", identifier)
	fmt.Println()
	fmt.Println("You can now run:")
	fmt.Printf("  msgvault sync-full %s\n", email)

	return nil
}

// isMicrosoftIMAPSource returns true only if src is an IMAP source already
// configured for Microsoft XOAUTH2 with the given username. This prevents
// a non-Microsoft IMAP source (e.g. a password-auth source) that happens to
// share the same display name from being silently repointed to Outlook XOAUTH2.
func isMicrosoftIMAPSource(src *store.Source, email string) bool {
	if !src.SyncConfig.Valid {
		return false
	}
	cfg, err := imapclient.ConfigFromJSON(src.SyncConfig.String)
	if err != nil {
		return false
	}
	return cfg.EffectiveAuthMethod() == imapclient.AuthXOAuth2 &&
		strings.EqualFold(cfg.Username, email)
}

func init() {
	rootCmd.AddCommand(newAddO365Cmd())
}
