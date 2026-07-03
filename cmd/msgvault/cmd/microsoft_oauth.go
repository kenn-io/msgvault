package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// oauthPreflightedFlag marks that the frontend CLI already completed the
// browser authorization before proxying, so the daemon subprocess must not
// start another one.
const oauthPreflightedFlag = "oauth-preflighted"

func registerOAuthPreflightedFlag(cmd *cobra.Command) {
	cmd.Flags().Bool(oauthPreflightedFlag, false,
		"Internal: OAuth authorization was already completed by the frontend CLI")
	if err := cmd.Flags().MarkHidden(oauthPreflightedFlag); err != nil {
		panic(err)
	}
}

func oauthPreflighted(cmd *cobra.Command) (bool, error) {
	preflighted, err := cmd.Flags().GetBool(oauthPreflightedFlag)
	if err != nil {
		return false, fmt.Errorf("read --%s flag: %w", oauthPreflightedFlag, err)
	}
	return preflighted, nil
}

func requireMicrosoftOAuthConfig() error {
	if cfg.Microsoft.ClientID == "" {
		return errors.New("microsoft OAuth not configured\n\n" +
			"Add to your config.toml:\n\n" +
			"  [microsoft]\n" +
			"  client_id = \"your-azure-app-client-id\"\n\n" +
			"See docs for Azure AD app registration setup")
	}
	return nil
}

// microsoftTenantID resolves the tenant, letting a per-command flag
// override the configured default.
func microsoftTenantID(flagTenant string) string {
	if flagTenant != "" {
		return flagTenant
	}
	return cfg.Microsoft.EffectiveTenantID()
}

// errBrowserAuthBehindDaemon explains why the daemon subprocess refuses to
// start a browser flow: the consent screen would open on the daemon's host
// while the operation gate blocks every other command on the human.
func errBrowserAuthBehindDaemon(command, email string) error {
	return fmt.Errorf(
		"account %s needs browser authorization, which cannot run behind the daemon; "+
			"run `msgvault %s %s` from a terminal on this machine",
		email, command, email,
	)
}
