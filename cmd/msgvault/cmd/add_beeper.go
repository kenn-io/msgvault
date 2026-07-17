package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/clirun"
)

var (
	addBeeperTokenFile         string
	noDefaultIdentityAddBeeper bool
)

func newAddBeeperCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-beeper",
		Short: "Add Beeper Desktop as an archive source",
		Long: `Add Beeper Desktop as an archive source.

Beeper Desktop bridges many chat networks (WhatsApp, Signal, Telegram,
Instagram, LinkedIn, X, Facebook, ...) into one local app with a read-only
API on localhost. Each connected network account becomes its own msgvault
source, so networks stay separately filterable and searchable.

Requires Beeper Desktop to be running on this machine. Mint an access token
in Beeper Desktop (Settings > Developer) and provide it via --token-file,
the MSGVAULT_BEEPER_TOKEN environment variable, or the interactive prompt.

Use [beeper].accounts / exclude_accounts in config.toml to limit which
networks are archived (e.g. exclude "whatsapp" if you already archive it
with import-whatsapp).

Examples:
  msgvault add-beeper
  msgvault add-beeper --token-file ~/beeper-token.txt
  MSGVAULT_BEEPER_TOKEN="..." msgvault add-beeper`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				token, err := readAddBeeperToken(cmd)
				if err != nil {
					return err
				}
				// The Beeper Desktop API is loopback-only: validate locally
				// when the daemon shares this machine so problems fail fast;
				// with a remote daemon, validation happens daemon-side against
				// the Beeper Desktop running there.
				if IsRemoteMode() {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Remote daemon configured: it must run beside its own Beeper Desktop, which will validate the token.")
				} else {
					accounts, err := beeperClient(token).ListAccounts(cmd.Context())
					if err != nil {
						return err
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Connected to Beeper Desktop (%d accounts)\n", len(accounts))
				}
				return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{
					clirun.EnvBeeperToken: token,
				})
			}

			token := os.Getenv(clirun.EnvBeeperToken)
			if token == "" {
				return errors.New("missing Beeper token in daemon subprocess (set MSGVAULT_BEEPER_TOKEN)")
			}
			accounts, err := beeperClient(token).ListAccounts(cmd.Context())
			if err != nil {
				return err
			}
			if err := beeper.SaveToken(cfg.TokensDir(), token); err != nil {
				return fmt.Errorf("save beeper token: %w", err)
			}

			s, cleanup, err := openWritableStoreAndInitForIngest()
			if err != nil {
				return err
			}
			defer cleanup()

			added := 0
			for _, acct := range accounts {
				if !cfg.Beeper.AccountIncluded(acct.AccountID) {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Skipping %s (filtered by [beeper] config)\n", acct.AccountID)
					continue
				}
				source, err := s.GetOrCreateSource(sourceTypeBeeper, acct.AccountID)
				if err != nil {
					return fmt.Errorf("create source for %s: %w", acct.AccountID, err)
				}
				if err := s.UpdateSourceDisplayName(source.ID, beeperSourceDisplayName(acct)); err != nil {
					return fmt.Errorf("set display name for %s: %w", acct.AccountID, err)
				}
				if !noDefaultIdentityAddBeeper {
					confirmDefaultIdentity(cmd.OutOrStdout(), s, source.ID,
						acct.AccountID, beeperSelfIdentity(acct), "account-identifier")
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Added %s (%s)\n", acct.AccountID, beeperSourceDisplayName(acct))
				added++
			}
			if err := runPostSourceCreateMigrations(s); err != nil {
				return fmt.Errorf("post-source-create migrations: %w", err)
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nBeeper Desktop added: %d account(s).\n", added)
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nYou can now run:")
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "  msgvault sync-beeper")
			return nil
		},
	}
	cmd.Flags().StringVar(&addBeeperTokenFile, "token-file", "", "read the Beeper Desktop access token from this file")
	cmd.Flags().BoolVar(&noDefaultIdentityAddBeeper, "no-default-identity", false, noDefaultIdentityHelp)
	return cmd
}

// beeperClient builds a Beeper Desktop API client from the configured URL and
// rate limit with a static token.
func beeperClient(token string) *beeper.Client {
	// An empty URL selects the client's loopback default.
	return beeper.NewClient(cfg.Beeper.URL,
		func(context.Context) (string, error) { return token, nil },
		cfg.Beeper.RateLimitQPS)
}

// beeperSourceDisplayName renders a human-readable source name, preferring
// the network name ("Beeper WhatsApp") over the raw accountID.
func beeperSourceDisplayName(acct beeper.Account) string {
	if acct.Network != "" {
		return "Beeper " + acct.Network
	}
	return "Beeper " + acct.AccountID
}

// beeperSelfIdentity picks the best own-identity value for a Beeper account:
// phone, then email, then the Beeper user ID.
func beeperSelfIdentity(acct beeper.Account) string {
	switch {
	case acct.User.PhoneNumber != "":
		return acct.User.PhoneNumber
	case acct.User.Email != "":
		return acct.User.Email
	default:
		return acct.User.ID
	}
}

// readAddBeeperToken resolves the access token: env var, then --token-file,
// then interactive masked prompt / piped stdin.
func readAddBeeperToken(cmd *cobra.Command) (string, error) {
	if envToken := os.Getenv(clirun.EnvBeeperToken); envToken != "" {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Using token from %s environment variable\n", clirun.EnvBeeperToken); err != nil {
			return "", fmt.Errorf("write token source notice: %w", err)
		}
		return envToken, nil
	}
	if addBeeperTokenFile != "" {
		data, err := os.ReadFile(addBeeperTokenFile)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("token file %s is empty", addBeeperTokenFile)
		}
		return token, nil
	}

	prompt := "Beeper Desktop access token (Settings > Developer):"
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
		return "", errors.New("cannot read token: no terminal available for prompt (try --token-file, piping the token via stdin, or setting MSGVAULT_BEEPER_TOKEN)")
	case passwordPipe:
		return readPasswordFromPipe(os.Stdin)
	default:
		return "", errors.New("cannot determine token input method")
	}
}

func init() {
	rootCmd.AddCommand(newAddBeeperCmd())
}
