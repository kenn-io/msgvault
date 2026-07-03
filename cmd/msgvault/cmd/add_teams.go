package cmd

import (
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
			if err := preflightAddTeamsAuthorize(cmd, args[0]); err != nil {
				return err
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runAddTeamsLocal(cmd, args)
	}
	return cmd
}

// preflightAddTeamsAuthorize runs the Microsoft browser flow in this
// process before proxying, so the daemon subprocess never opens a browser
// or waits on human consent while holding the operation gate.
func preflightAddTeamsAuthorize(cmd *cobra.Command, email string) error {
	if IsRemoteMode() {
		// Tokens live on the remote host; authorization must happen there.
		return nil
	}
	if err := requireMicrosoftOAuthConfig(); err != nil {
		return err
	}
	mgr := microsoft.NewGraphManager(
		cfg.Microsoft.ClientID,
		microsoftTenantID(teamsTenantID),
		cfg.TokensDir(),
		logger,
	)
	fmt.Printf("Authorizing %s with Microsoft Teams...\n", email)
	if err := mgr.Authorize(cmd.Context(), email); err != nil {
		return fmt.Errorf("authorize Teams: %w", err)
	}
	if err := cmd.Flags().Set(oauthPreflightedFlag, "true"); err != nil {
		return fmt.Errorf("set --%s after authorization: %w", oauthPreflightedFlag, err)
	}
	return nil
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
	registerOAuthPreflightedFlag(cmd)
	return cmd
}

func runAddTeamsLocal(cmd *cobra.Command, args []string) error {
	email := args[0]

	if err := requireMicrosoftOAuthConfig(); err != nil {
		return err
	}

	preflighted, err := oauthPreflighted(cmd)
	if err != nil {
		return err
	}
	if !preflighted {
		if isDaemonCLISubprocess() {
			return errBrowserAuthBehindDaemon("add-teams", email)
		}
		mgr := microsoft.NewGraphManager(
			cfg.Microsoft.ClientID,
			microsoftTenantID(teamsTenantID),
			cfg.TokensDir(),
			logger,
		)
		fmt.Printf("Authorizing %s with Microsoft Teams...\n", email)
		if err := mgr.Authorize(cmd.Context(), email); err != nil {
			return fmt.Errorf("authorize Teams: %w", err)
		}
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
