package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/slack"
)

var (
	addSlackTokenFile         string
	noDefaultIdentityAddSlack bool
)

func newAddSlackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-slack",
		Short: "Add a Slack workspace as an archive source",
		Long: `Add a Slack workspace as an archive source.

Archives your own view of the workspace — public/private channels you are a
member of, group DMs, and 1:1 DMs — via the Slack Web API.

Requires a user token (xoxp-...) from an internal Slack app you create:

  1. https://api.slack.com/apps > Create New App > From scratch, in your
     workspace.
  2. OAuth & Permissions > User Token Scopes, add:
       channels:history groups:history im:history mpim:history
       channels:read groups:read im:read mpim:read
       users:read users:read.email files:read reactions:read team:read
  3. Install to Workspace, then copy the "User OAuth Token".

Internal apps you create yourself are not subject to Slack's non-Marketplace
rate limits, so backfills run at full speed.

Provide the token via --token-file, the MSGVAULT_SLACK_TOKEN environment
variable, or the interactive prompt.

Examples:
  msgvault add-slack
  msgvault add-slack --token-file ~/slack-token.txt
  MSGVAULT_SLACK_TOKEN="xoxp-..." msgvault add-slack`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				token, err := readAddSlackToken(cmd)
				if err != nil {
					return err
				}
				return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{
					clirun.EnvSlackToken: token,
				})
			}

			token := os.Getenv(clirun.EnvSlackToken)
			if token == "" {
				return errors.New("missing Slack token in daemon subprocess (set MSGVAULT_SLACK_TOKEN)")
			}
			auth, err := slack.NewClient("", token).AuthTest(cmd.Context())
			if err != nil {
				return err
			}
			teamDomain := strings.TrimSuffix(strings.TrimPrefix(auth.URL, "https://"), ".slack.com/")
			if err := slack.SaveToken(cfg.TokensDir(), auth.TeamID, teamDomain, auth.UserID, token); err != nil {
				return fmt.Errorf("save slack token: %w", err)
			}

			s, cleanup, err := openWritableStoreAndInitForIngest()
			if err != nil {
				return err
			}
			defer cleanup()

			identifier := auth.TeamID + ":" + auth.UserID
			source, err := s.GetOrCreateSource(sourceTypeSlack, identifier)
			if err != nil {
				return fmt.Errorf("create source for %s: %w", identifier, err)
			}
			displayName := "Slack " + auth.Team
			if err := s.UpdateSourceDisplayName(source.ID, displayName); err != nil {
				return fmt.Errorf("set display name for %s: %w", identifier, err)
			}
			if !noDefaultIdentityAddSlack {
				confirmDefaultIdentity(cmd.OutOrStdout(), s, source.ID,
					identifier, auth.UserID, "account-identifier")
			}
			if err := runPostSourceCreateMigrations(s); err != nil {
				return fmt.Errorf("post-source-create migrations: %w", err)
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Added Slack workspace %s (%s) as %s\n", auth.Team, auth.TeamID, identifier)
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nYou can now run:")
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  msgvault sync-slack %s\n", auth.TeamID)
			return nil
		},
	}
	cmd.Flags().StringVar(&addSlackTokenFile, "token-file", "", "read the Slack user token from this file")
	cmd.Flags().BoolVar(&noDefaultIdentityAddSlack, "no-default-identity", false, noDefaultIdentityHelp)
	return cmd
}

// readAddSlackToken resolves the user token: env var, then --token-file,
// then interactive masked prompt / piped stdin.
func readAddSlackToken(cmd *cobra.Command) (string, error) {
	if envToken := os.Getenv(clirun.EnvSlackToken); envToken != "" {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Using token from %s environment variable\n", clirun.EnvSlackToken); err != nil {
			return "", fmt.Errorf("write token source notice: %w", err)
		}
		return envToken, nil
	}
	if addSlackTokenFile != "" {
		data, err := os.ReadFile(addSlackTokenFile)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("token file %s is empty", addSlackTokenFile)
		}
		return token, nil
	}

	prompt := "Slack user token (xoxp-..., from your app's OAuth & Permissions page):"
	method, promptOut := choosePasswordStrategy(
		isatty.IsTerminal(os.Stdin.Fd()),
		isatty.IsCygwinTerminal(os.Stdin.Fd()),
		isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()),
		isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()),
	)
	switch method {
	case passwordInteractive:
		return readPasswordInteractive(prompt, promptOut)
	case passwordNoPrompt:
		return "", errors.New("cannot read token: no terminal available for prompt (try --token-file, piping the token via stdin, or setting MSGVAULT_SLACK_TOKEN)")
	case passwordPipe:
		return readPasswordFromPipe(os.Stdin)
	default:
		return "", errors.New("cannot determine token input method")
	}
}

func init() {
	rootCmd.AddCommand(newAddSlackCmd())
}
