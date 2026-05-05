package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	imapclient "github.com/wesm/msgvault/internal/imap"
	"github.com/wesm/msgvault/internal/microsoft"
	"github.com/wesm/msgvault/internal/store"
)

var (
	o365TenantID             string
	noDefaultIdentityAddO365 bool
)

var addO365Cmd = &cobra.Command{
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
	RunE: func(cmd *cobra.Command, args []string) error {
		email := args[0]

		if cfg.Microsoft.ClientID == "" {
			return fmt.Errorf("microsoft OAuth not configured\n\n" +
				"Add to your config.toml:\n\n" +
				"  [microsoft]\n" +
				"  client_id = \"your-azure-app-client-id\"\n\n" +
				"See docs for Azure AD app registration setup")
		}

		tenantID := cfg.Microsoft.EffectiveTenantID()
		if o365TenantID != "" {
			tenantID = o365TenantID
		}

		msMgr := microsoft.NewManager(
			cfg.Microsoft.ClientID,
			tenantID,
			cfg.TokensDir(),
			logger,
		)

		fmt.Printf("Authorizing %s with Microsoft...\n", email)
		if err := msMgr.Authorize(cmd.Context(), email); err != nil {
			return fmt.Errorf("authorization failed: %w", err)
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
			if src.SourceType == "imap" && isMicrosoftIMAPSource(src, email) {
				source = src
				break
			}
		}

		if source != nil {
			if err := s.UpdateSourceIdentifier(source.ID, identifier); err != nil {
				return fmt.Errorf("update source identifier: %w", err)
			}
		} else {
			source, err = s.GetOrCreateSource("imap", identifier)
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
	},
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
	addO365Cmd.Flags().StringVar(&o365TenantID, "tenant", "",
		"Azure AD tenant ID (default: \"common\" for multi-tenant)")
	addO365Cmd.Flags().BoolVar(&noDefaultIdentityAddO365, "no-default-identity", false, noDefaultIdentityHelp)
	rootCmd.AddCommand(addO365Cmd)
}
