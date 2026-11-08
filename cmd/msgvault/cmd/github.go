package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/github"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

func githubUserAgent() string {
	return "msgvault/" + Version + " (+https://msgvault.io)"
}

func newGitHubClient(token string) *github.Client {
	return github.NewClient(github.DefaultBaseURL, token, githubUserAgent())
}

func newAddGitHubCmd() *cobra.Command {
	var tokenFile string
	var noDefaultIdentity bool
	cmd := &cobra.Command{
		Use:   "add-github",
		Short: "Add a GitHub account as an archive source",
		Long: `Add a GitHub account as an archive source.

Archives the issues, pull requests and discussions you take part in, with
every comment and review, and the releases of repositories you watch for
releases. Syncs find changed threads through your GitHub notifications and
a search for threads that involve you, so threads you only commented on are
archived too.

Requires a classic personal access token: GitHub's notifications API doesn't
accept fine-grained tokens. Create one at
https://github.com/settings/tokens with the notifications scope, and the
repo scope to also archive private repositories.

Provide the token via --token-file, the MSGVAULT_GITHUB_TOKEN environment
variable, or the interactive prompt. Choose whose repositories are archived
with include_owners and exclude_owners under [github] in config.toml.

Examples:
  msgvault add-github
  msgvault add-github --token-file ~/github-token.txt
  MSGVAULT_GITHUB_TOKEN="ghp_..." msgvault add-github`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if !isDaemonCLISubprocess() {
				token, err := readAddGitHubToken(cmd, tokenFile)
				if err != nil {
					return err
				}
				return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{
					clirun.EnvGitHubToken: token,
				})
			}

			token := os.Getenv(clirun.EnvGitHubToken)
			if token == "" {
				return errors.New("missing GitHub token in daemon subprocess (set MSGVAULT_GITHUB_TOKEN)")
			}
			viewer, err := newGitHubClient(token).Viewer(cmd.Context())
			if err != nil {
				return err
			}
			if err := github.SaveToken(state.cfg.TokensDir(), github.Token{
				AccessToken: token, AccountID: viewer.ID, Login: viewer.Login,
			}); err != nil {
				return fmt.Errorf("save GitHub token: %w", err)
			}

			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
			if err != nil {
				return err
			}
			defer cleanup()

			identifier := strconv.FormatInt(viewer.ID, 10)
			source, err := s.GetOrCreateSourceContext(cmd.Context(), github.SourceType, identifier)
			if err != nil {
				return fmt.Errorf("create source for GitHub account %s: %w", viewer.Login, err)
			}
			if err := updateSourceDisplayNameForRegistration(s, source.ID, "GitHub "+viewer.Login, state.logger); err != nil {
				return fmt.Errorf("set display name for GitHub account %s: %w", viewer.Login, err)
			}
			if !noDefaultIdentity {
				// The account ID is the identifier of every post the account
				// writes, so the account's own posts count as sent by you.
				confirmDefaultIdentity(cmd.Context(), cmd.OutOrStdout(), s, source.ID,
					viewer.Login, identifier, "account-identifier", state.logger)
			}
			if err := runPostSourceCreateMigrationsForInvocation(cmd.Context(), s, state); err != nil {
				return fmt.Errorf("post-source-create migrations: %w", err)
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Added GitHub account %s\n", textutil.SanitizeTerminal(viewer.Login))
			_, _ = fmt.Fprintln(out, "\nYou can now run:")
			_, _ = fmt.Fprintln(out, "  msgvault sync-github")
			return nil
		},
	}
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "read the GitHub personal access token from this file")
	cmd.Flags().BoolVar(&noDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
	return cmd
}

// readAddGitHubToken resolves the token: env var, then --token-file, then
// a masked prompt or piped stdin.
func readAddGitHubToken(cmd *cobra.Command, tokenFile string) (string, error) {
	if envToken := os.Getenv(clirun.EnvGitHubToken); envToken != "" {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Using token from %s environment variable\n", clirun.EnvGitHubToken); err != nil {
			return "", fmt.Errorf("write token source notice: %w", err)
		}
		return envToken, nil
	}
	if tokenFile != "" {
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("token file %s is empty", tokenFile)
		}
		return token, nil
	}

	prompt := "GitHub personal access token (classic, with the notifications scope):"
	method, promptOut := choosePasswordStrategy(
		isatty.IsTerminal(os.Stdin.Fd()),
		isatty.IsCygwinTerminal(os.Stdin.Fd()),
		isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()),
		isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()),
	)
	switch method {
	case passwordInteractive:
		return readPasswordInteractive(prompt, promptOut, false)
	case passwordNoPrompt:
		return "", errors.New("cannot read token: no terminal available for prompt (try --token-file, piping the token via stdin, or setting MSGVAULT_GITHUB_TOKEN)")
	case passwordPipe:
		return readPasswordFromPipe(os.Stdin)
	default:
		return "", errors.New("cannot determine token input method")
	}
}

func newSyncGitHubCmd() *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:   "sync-github [account]",
		Short: "Archive changed GitHub issues, pull requests, discussions and releases",
		Long: `Sync the GitHub accounts added with add-github, or the one named by its login.

Each sync finds the threads that changed since the last one, through your
notifications and a search for threads that involve you, and archives every
post in them. The first sync also finds every older thread you took part in.
A sync leaves part of each hourly GitHub rate limit to your other tools; a
backfill that reaches it continues on the next sync.

--full forgets how far discovery came, so every thread is found and fetched
again; posts archived unchanged are not written twice.

Examples:
  msgvault sync-github
  msgvault sync-github octocat
  msgvault sync-github --full`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			account := ""
			if len(args) > 0 {
				account = args[0]
			}
			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
			if err != nil {
				return err
			}
			defer cleanup()
			sources, err := resolveGitHubSyncSources(cmd.Context(), s, state.cfg.TokensDir(), account)
			if err != nil {
				return err
			}
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving GitHub progress...")
			defer stop()

			var syncErrors []error
			for _, source := range sources {
				if ctx.Err() != nil {
					break
				}
				summary, err := syncGitHubSource(ctx, s, state.cfg, source, github.Options{
					Owners: githubOwnerFilter(state.cfg), Full: full,
				})
				err = recordSyncAttempt(ctx, github.SourceType, source.Identifier, err)
				printGitHubSummary(cmd.OutOrStdout(), source, summary)
				if err != nil && ctx.Err() == nil {
					syncErrors = append(syncErrors, fmt.Errorf("GitHub %s: %w", githubSourceName(source, summary), err))
				}
			}
			cacheErr := rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state)
			if ctx.Err() != nil {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted; run sync-github again to continue.")
				return errors.Join(fmt.Errorf("interrupted: %w", ctx.Err()), cacheErr)
			}
			return errors.Join(append(syncErrors, cacheErr)...)
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "find and fetch every thread again, not only the ones changed since the last sync")
	return addManualSyncCacheFlags(cmd)
}

func githubOwnerFilter(cfg *config.Config) github.OwnerFilter {
	return github.NewOwnerFilter(cfg.GitHub.IncludeOwners, cfg.GitHub.ExcludeOwners)
}

// syncGitHubSource syncs one GitHub account with its stored token.
func syncGitHubSource(ctx context.Context, s *store.Store, cfg *config.Config, source *store.Source, opts github.Options) (github.Summary, error) {
	accountID, err := strconv.ParseInt(source.Identifier, 10, 64)
	if err != nil {
		return github.Summary{SourceID: source.ID}, fmt.Errorf("malformed GitHub source identifier %q", source.Identifier)
	}
	token, err := github.LoadToken(cfg.TokensDir(), accountID)
	if err != nil {
		return github.Summary{SourceID: source.ID}, err
	}
	syncer := github.NewSyncer(s, newGitHubClient(token.AccessToken), loggerFromContext(ctx))
	return syncer.Sync(ctx, source.ID, opts)
}

// resolveGitHubSyncSources returns the GitHub sources to sync: the one
// whose account login or ID is account, or every active one.
func resolveGitHubSyncSources(ctx context.Context, s *store.Store, tokensDir, account string) ([]*store.Source, error) {
	sources, err := s.ListSourcesContext(ctx, github.SourceType)
	if err != nil {
		return nil, fmt.Errorf("list GitHub sources: %w", err)
	}
	active := activeSyncSources(sources)
	if account == "" {
		if len(active) == 0 {
			return nil, errors.New("no GitHub accounts added (run 'add-github' first)")
		}
		return active, nil
	}
	for _, source := range active {
		if source.Identifier == account {
			return []*store.Source{source}, nil
		}
		if id, err := strconv.ParseInt(source.Identifier, 10, 64); err == nil {
			if token, err := github.LoadToken(tokensDir, id); err == nil && strings.EqualFold(token.Login, account) {
				return []*store.Source{source}, nil
			}
		}
	}
	return nil, fmt.Errorf("GitHub account %q is not added (run 'add-github' first)", account)
}

// runConfiguredGitHubSync is the [github] scheduler job: a sync of every
// added account. One failing account doesn't stop the others.
func runConfiguredGitHubSync(ctx context.Context, s *store.Store) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	sources, err := resolveGitHubSyncSources(ctx, s, state.cfg.TokensDir(), "")
	if err != nil {
		return err
	}
	var errs []error
	for _, source := range sources {
		if ctx.Err() != nil {
			break
		}
		summary, err := syncGitHubSource(ctx, s, state.cfg, source, github.Options{Owners: githubOwnerFilter(state.cfg)})
		if err := recordSyncAttempt(ctx, github.SourceType, source.Identifier, err); err != nil {
			errs = append(errs, fmt.Errorf("GitHub %s: %w", githubSourceName(source, summary), err))
		}
	}
	// Even a failed run may have archived threads before it stopped.
	errs = append(errs, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), github.SourceType))
	return errors.Join(errs...)
}

func githubSourceName(source *store.Source, summary github.Summary) string {
	if summary.Login != "" {
		return summary.Login
	}
	if source.DisplayName.Valid && source.DisplayName.String != "" {
		return source.DisplayName.String
	}
	return source.Identifier
}

func printGitHubSummary(out io.Writer, source *store.Source, summary github.Summary) {
	name := textutil.SanitizeTerminal(githubSourceName(source, summary))
	_, _ = fmt.Fprintf(out, "GitHub %s: %d threads archived, %d new messages, %d updated",
		name, summary.Archived, summary.Added, summary.Updated)
	if summary.Excluded > 0 {
		_, _ = fmt.Fprintf(out, ", %d threads of excluded owners skipped", summary.Excluded)
	}
	if summary.Unavailable > 0 {
		_, _ = fmt.Fprintf(out, ", %d unavailable", summary.Unavailable)
	}
	if summary.Failed > 0 {
		_, _ = fmt.Fprintf(out, ", %d failed", summary.Failed)
	}
	_, _ = fmt.Fprintln(out)
	if summary.Remaining > 0 {
		if summary.RateLimit != nil && !summary.RateLimit.Reset.IsZero() {
			_, _ = fmt.Fprintf(out, "  %d threads wait for the GitHub rate limit, which resets at %s; the next sync continues.\n",
				summary.Remaining, summary.RateLimit.Reset.Local().Format(time.Kitchen))
		} else {
			_, _ = fmt.Fprintf(out, "  %d threads wait for the next sync.\n", summary.Remaining)
		}
	}
}

func init() {
	rootCmd.AddCommand(newAddGitHubCmd(), newSyncGitHubCmd())
}
