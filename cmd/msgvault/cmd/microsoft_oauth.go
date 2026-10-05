package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/teams"
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

func requireMicrosoftOAuthConfig(cfg *config.Config) error {
	if cfg == nil || cfg.Microsoft.ClientID == "" {
		return errors.New("microsoft OAuth not configured\n\n" +
			"Add to your config.toml:\n\n" +
			"  [microsoft]\n" +
			"  client_id = \"your-azure-app-client-id\"\n\n" +
			"See docs for Azure AD app registration setup")
	}
	return nil
}

// newTeamsClient builds a Graph client for email's persisted Teams token.
func newTeamsClient(ctx context.Context, cfg *config.Config, logger *slog.Logger, email string) (*teams.Client, error) {
	mgr := microsoft.NewGraphManager(cfg.Microsoft.ClientID, cfg.Microsoft.EffectiveTenantID(),
		cfg.Microsoft.EffectiveRedirectURI(), cfg.TokensDir(), logger)
	tokenFn, err := mgr.TokenSource(ctx, email)
	if err != nil {
		return nil, err
	}
	qps := float64(cfg.Sync.RateLimitQPS)
	if qps <= 0 {
		qps = 5
	}
	return teams.NewClient("https://graph.microsoft.com/v1.0", tokenFn, qps), nil
}

// microsoftTenantID resolves the tenant, letting a per-command flag
// override the configured default.
func microsoftTenantID(flagTenant string, cfg *config.Config) string {
	if flagTenant != "" {
		return flagTenant
	}
	if cfg == nil {
		return ""
	}
	return cfg.Microsoft.EffectiveTenantID()
}
