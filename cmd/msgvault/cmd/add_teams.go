package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/microsoft"
)

var (
	teamsTenantID             string
	noDefaultIdentityAddTeams bool
)

func newAddTeamsCmd() *cobra.Command {
	cmd := newAddTeamsLocalCmd()
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runAddTeamsLocal(cmd, args)
	}
	return cmd
}

func newAddTeamsLocalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-teams <email>",
		Short: "Authorize Microsoft Teams (delegated Graph) for an account",
		Long: `Authorize a Microsoft Teams account using OAuth2 (delegated Graph API).

This opens a browser for Microsoft authorization, then stores the token for
Teams message ingestion.

Requires a [microsoft] section in config.toml with your Azure AD app's client_id.
See the docs for Azure AD app registration setup.

Examples:
  msgvault add-teams user@company.com
  msgvault add-teams user@company.com --tenant my-tenant-id`,
		Args: cobra.ExactArgs(1),
		RunE: runAddTeamsLocal,
	}
	cmd.Flags().StringVar(&teamsTenantID, "tenant", "",
		"Azure AD tenant ID (default: \"common\" for multi-tenant)")
	cmd.Flags().BoolVar(&noDefaultIdentityAddTeams, "no-default-identity", false, noDefaultIdentityHelp)
	return cmd
}

func runAddTeamsLocal(cmd *cobra.Command, args []string) error {
	email := args[0]

	if cfg.Microsoft.ClientID == "" {
		return errors.New("microsoft OAuth not configured\n\n" +
			"Add to your config.toml:\n\n" +
			"  [microsoft]\n" +
			"  client_id = \"your-azure-app-client-id\"\n\n" +
			"See docs for Azure AD app registration setup")
	}

	tenantID := cfg.Microsoft.EffectiveTenantID()
	if teamsTenantID != "" {
		tenantID = teamsTenantID
	}

	mgr := microsoft.NewGraphManager(
		cfg.Microsoft.ClientID,
		tenantID,
		cfg.TokensDir(),
		logger,
	)

	fmt.Printf("Authorizing %s with Microsoft Teams...\n", email)
	if err := mgr.Authorize(cmd.Context(), email); err != nil {
		return fmt.Errorf("authorize Teams: %w", err)
	}

	s, cleanup, err := openWritableStoreAndInitForIngest()
	if err != nil {
		return err
	}
	defer cleanup()

	source, err := s.GetOrCreateSource(sourceTypeTeams, email)
	if err != nil {
		return fmt.Errorf("create source: %w", err)
	}
	if err := s.UpdateSourceDisplayName(source.ID, email); err != nil {
		return fmt.Errorf("set display name: %w", err)
	}

	if !noDefaultIdentityAddTeams {
		confirmDefaultIdentity(cmd.OutOrStdout(), s, source.ID, email, email, "account-identifier")
	}
	if err := runPostSourceCreateMigrations(s); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}

	fmt.Printf("\nMicrosoft Teams account authorized successfully!\n")
	fmt.Printf("  Email: %s\n", email)
	fmt.Println()
	fmt.Println("You can now run:")
	fmt.Printf("  msgvault sync-teams %s\n", email)

	return nil
}

func init() {
	rootCmd.AddCommand(newAddTeamsCmd())
}
