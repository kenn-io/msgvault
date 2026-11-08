package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/forge"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// forgeSource is one code forge as the commands present it. Each forge gets
// add-<name> and sync-<name> commands, a [<name>] config block and a
// scheduler job, all from this one implementation.
type forgeSource struct {
	// name is the forge's source type, such as "github".
	name string
	// label names the forge to people, such as "GitHub".
	label string
	// defaultURL is the web URL of the instance used without --url; empty
	// means --url is required.
	defaultURL string
	// fixedURL means the forge has one instance, so add takes no --url.
	fixedURL bool
	// tokenEnv passes the token to the daemon's subprocess.
	tokenEnv string
	// tokenPrompt asks for the token interactively.
	tokenPrompt string
	// addHelp is add's long help.
	addHelp string
	// config returns the forge's config block.
	config func(*config.Config) config.ForgeConfig
	// open returns the forge for a token on the instance at base.
	open func(base *url.URL, token string) (forge.Forge, error)
}

func forgeUserAgent() string {
	return "msgvault/" + Version + " (+https://msgvault.io)"
}

// forgeSources are the forges msgvault archives, by name.
var forgeSources = map[string]*forgeSource{}

func registerForge(source *forgeSource) {
	forgeSources[source.name] = source
	rootCmd.AddCommand(source.addCommand(), source.syncCommand())
}

// forgeFor returns the forge registered for a source type.
func forgeFor(sourceType string) (*forgeSource, bool) {
	source, ok := forgeSources[sourceType]
	return source, ok
}

func (fs *forgeSource) addCommand() *cobra.Command {
	var instanceURL, tokenFile string
	var noDefaultIdentity bool
	cmd := &cobra.Command{
		Use:   "add-" + fs.name,
		Short: "Add a " + fs.label + " account as an archive source",
		Long:  fs.addHelp,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if !isDaemonCLISubprocess() {
				token, err := fs.readToken(cmd, tokenFile)
				if err != nil {
					return err
				}
				return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{fs.tokenEnv: token})
			}
			token := os.Getenv(fs.tokenEnv)
			if token == "" {
				return fmt.Errorf("missing %s token in daemon subprocess (set %s)", fs.label, fs.tokenEnv)
			}
			if instanceURL == "" {
				instanceURL = fs.defaultURL
			}
			if instanceURL == "" {
				return usageErr(cmd, errors.New("--url is required: the web address of the "+fs.label+" server"))
			}
			instance, base, err := forge.ParseInstance(instanceURL)
			if err != nil {
				return err
			}
			adapter, err := fs.open(base, token)
			if err != nil {
				return err
			}
			account, err := adapter.Account(cmd.Context())
			if err != nil {
				return err
			}
			if err := forge.SaveToken(state.cfg.TokensDir(), fs.name, forge.Token{
				AccessToken: token, BaseURL: base.String(), Instance: instance,
				AccountID: account.ID, Login: account.Login,
			}); err != nil {
				return fmt.Errorf("save %s token: %w", fs.label, err)
			}

			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
			if err != nil {
				return err
			}
			defer cleanup()
			identifier := forge.SourceIdentifier(instance, account.ID)
			source, err := s.GetOrCreateSourceContext(cmd.Context(), fs.name, identifier)
			if err != nil {
				return fmt.Errorf("create source for %s account %s: %w", fs.label, account.Login, err)
			}
			if err := updateSourceDisplayNameForRegistration(s, source.ID, fs.displayName(instance, account.Login), state.logger); err != nil {
				return fmt.Errorf("set display name for %s account %s: %w", fs.label, account.Login, err)
			}
			if !noDefaultIdentity {
				// The account's key identifies every post it writes, so its
				// own posts count as sent by you.
				confirmDefaultIdentity(cmd.Context(), cmd.OutOrStdout(), s, source.ID,
					account.Login, forge.AccountKey(instance, account.ID), "account-identifier", state.logger)
			}
			if err := runPostSourceCreateMigrationsForInvocation(cmd.Context(), s, state); err != nil {
				return fmt.Errorf("post-source-create migrations: %w", err)
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Added %s account %s\n", fs.label, textutil.SanitizeTerminal(account.Login))
			_, _ = fmt.Fprintln(out, "\nYou can now run:")
			_, _ = fmt.Fprintf(out, "  msgvault sync-%s\n", fs.name)
			return nil
		},
	}
	if !fs.fixedURL {
		usage := "web address of the " + fs.label + " server"
		if fs.defaultURL != "" {
			usage += " (default " + fs.defaultURL + ")"
		}
		cmd.Flags().StringVar(&instanceURL, "url", "", usage)
	}
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "read the "+fs.label+" token from this file")
	cmd.Flags().BoolVar(&noDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
	return cmd
}

// displayName names an account's source: the forge and login, and the
// instance when it isn't the forge's default.
func (fs *forgeSource) displayName(instance forge.Instance, login string) string {
	name := fs.label + " " + login
	if fs.defaultURL != "" {
		if defaultInstance, _, err := forge.ParseInstance(fs.defaultURL); err == nil && defaultInstance == instance {
			return name
		}
	}
	if fs.fixedURL {
		return name
	}
	return name + " (" + string(instance) + ")"
}

// readToken resolves the token: env var, then --token-file, then a masked
// prompt or piped stdin.
func (fs *forgeSource) readToken(cmd *cobra.Command, tokenFile string) (string, error) {
	if envToken := os.Getenv(fs.tokenEnv); envToken != "" {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Using token from %s environment variable\n", fs.tokenEnv); err != nil {
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
	method, promptOut := choosePasswordStrategy(
		isatty.IsTerminal(os.Stdin.Fd()),
		isatty.IsCygwinTerminal(os.Stdin.Fd()),
		isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()),
		isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()),
	)
	switch method {
	case passwordInteractive:
		return readPasswordInteractive(fs.tokenPrompt, promptOut, false)
	case passwordNoPrompt:
		return "", fmt.Errorf("cannot read token: no terminal available for prompt (try --token-file, piping the token via stdin, or setting %s)", fs.tokenEnv)
	case passwordPipe:
		return readPasswordFromPipe(os.Stdin)
	default:
		return "", errors.New("cannot determine token input method")
	}
}

func (fs *forgeSource) syncCommand() *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:   "sync-" + fs.name + " [account]",
		Short: "Archive changed " + fs.label + " threads",
		Long: `Sync the ` + fs.label + ` accounts added with add-` + fs.name + `, or the one named by its login.

Each sync finds the threads that changed since the last one and archives
every post in them; the first sync also finds the older threads the account
took part in. A sync that reaches a rate limit pauses with its place saved,
and the next sync continues it. Unchanged posts are not written twice.`,
		Example: "  msgvault sync-" + fs.name + "\n  msgvault sync-" + fs.name + " octocat\n  msgvault sync-" + fs.name + " --full",
		Args:    cobra.MaximumNArgs(1),
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
			sources, err := fs.sources(cmd.Context(), s, state.cfg.TokensDir(), account)
			if err != nil {
				return err
			}
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving "+fs.label+" progress...")
			defer stop()
			var syncErrors []error
			for _, source := range sources {
				if ctx.Err() != nil {
					break
				}
				summary, err := fs.sync(ctx, s, state.cfg, source, full)
				err = recordSyncAttempt(ctx, fs.name, source.Identifier, err)
				fs.printSummary(cmd.OutOrStdout(), source, summary)
				if err != nil && ctx.Err() == nil {
					syncErrors = append(syncErrors, fmt.Errorf("%s: %w", fs.accountName(source, summary), err))
				}
			}
			cacheErr := rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state)
			if ctx.Err() != nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nInterrupted; run sync-%s again to continue.\n", fs.name)
				return errors.Join(fmt.Errorf("interrupted: %w", ctx.Err()), cacheErr)
			}
			return errors.Join(append(syncErrors, cacheErr)...)
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "find and fetch every thread again, not only the ones changed since the last sync")
	return addManualSyncCacheFlags(cmd)
}

// sync syncs one account with its stored token.
func (fs *forgeSource) sync(ctx context.Context, s *store.Store, cfg *config.Config, source *store.Source, full bool) (forge.Summary, error) {
	token, err := forge.LoadToken(cfg.TokensDir(), fs.name, source.Identifier)
	if err != nil {
		return forge.Summary{SourceID: source.ID}, err
	}
	base, err := url.Parse(token.BaseURL)
	if err != nil {
		return forge.Summary{SourceID: source.ID}, fmt.Errorf("stored %s URL: %w", fs.label, err)
	}
	blocks := fs.config(cfg)
	adapter, err := fs.open(base, token.AccessToken)
	if err != nil {
		return forge.Summary{SourceID: source.ID}, err
	}
	syncer := forge.NewSyncer(s, adapter, loggerFromContext(ctx))
	summary, err := syncer.Sync(ctx, source.ID, forge.Options{
		Owners: forge.NewOwnerFilter(blocks.IncludeOwners, blocks.ExcludeOwners),
		Full:   full,
	})
	if summary.Login != "" && summary.Login != token.Login {
		if renameErr := fs.rememberLogin(ctx, s, cfg.TokensDir(), source, token, summary.Login); renameErr != nil {
			loggerFromContext(ctx).Warn("could not record a renamed forge account's login",
				"forge", fs.name, "source_id", source.ID, "error", renameErr)
		}
	}
	return summary, err
}

// rememberLogin keeps an account's current login after the account was
// renamed, so sync-<forge> <login> finds it by the name it has now, and
// renames its source when the source still carries the old login's name.
func (fs *forgeSource) rememberLogin(ctx context.Context, s *store.Store, tokensDir string, source *store.Source,
	token forge.Token, login string,
) error {
	previous := token.Login
	token.Login = login
	if err := forge.SaveToken(tokensDir, fs.name, token); err != nil {
		return fmt.Errorf("save %s token: %w", fs.label, err)
	}
	if source.DisplayName.Valid && source.DisplayName.String == fs.displayName(token.Instance, previous) {
		if err := s.UpdateSourceDisplayNameContext(ctx, source.ID, fs.displayName(token.Instance, login)); err != nil {
			return fmt.Errorf("rename %s source: %w", fs.label, err)
		}
	}
	return nil
}

// sources returns the accounts to sync: the one whose source identifier
// is account, else the one whose login is account, or every active one. A
// login that names accounts on more than one server is refused, with the
// identifiers that tell them apart.
func (fs *forgeSource) sources(ctx context.Context, s *store.Store, tokensDir, account string) ([]*store.Source, error) {
	all, err := s.ListSourcesContext(ctx, fs.name)
	if err != nil {
		return nil, fmt.Errorf("list %s sources: %w", fs.label, err)
	}
	active := activeSyncSources(all)
	if account == "" {
		if len(active) == 0 {
			return nil, fmt.Errorf("no %s accounts added (run 'add-%s' first)", fs.label, fs.name)
		}
		return active, nil
	}
	var byLogin []*store.Source
	for _, source := range active {
		if source.Identifier == account {
			return []*store.Source{source}, nil
		}
		if token, err := forge.LoadToken(tokensDir, fs.name, source.Identifier); err == nil && strings.EqualFold(token.Login, account) {
			byLogin = append(byLogin, source)
		}
	}
	switch len(byLogin) {
	case 0:
		return nil, fmt.Errorf("%s account %q is not added (run 'add-%s' first)", fs.label, account, fs.name)
	case 1:
		return byLogin, nil
	}
	identifiers := make([]string, len(byLogin))
	for i, source := range byLogin {
		identifiers[i] = source.Identifier
	}
	return nil, fmt.Errorf("%s login %q names accounts on more than one server; name one by its identifier: %s",
		fs.label, account, strings.Join(identifiers, ", "))
}

// runScheduled is the [<forge>] scheduler job: a sync of every added
// account. One failing account doesn't stop the others; a run that yields
// to other scheduled work resumes with the next account, so one long
// backfill can't starve the rest.
func (fs *forgeSource) runScheduled(ctx context.Context, s *store.Store) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	sources, err := fs.sources(ctx, s, state.cfg.TokensDir(), "")
	if err != nil {
		return err
	}
	var errs []error
	ordered := forgeRotation.order(fs.name, sources)
	resumeAt := ""
	for i, source := range ordered {
		if ctx.Err() != nil || jobctx.PreemptionRequested(ctx) {
			resumeAt = source.Identifier
			break
		}
		summary, err := fs.sync(ctx, s, state.cfg, source, false)
		if err := recordSyncAttempt(ctx, fs.name, source.Identifier, err); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", fs.accountName(source, summary), err))
		}
		if summary.Paused || jobctx.PreemptionRequested(ctx) {
			resumeAt = ordered[(i+1)%len(ordered)].Identifier
			break
		}
	}
	forgeRotation.resumeAt(fs.name, resumeAt)
	// Even a failed or paused run may have archived threads.
	errs = append(errs, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), fs.name))
	return errors.Join(errs...)
}

// forgeRotation remembers, per forge, which account a scheduled run that
// stopped early continues with.
var forgeRotation = &accountRotation{next: map[string]string{}}

type accountRotation struct {
	mu   sync.Mutex
	next map[string]string
}

func (r *accountRotation) order(forgeName string, sources []*store.Source) []*store.Source {
	r.mu.Lock()
	next := r.next[forgeName]
	r.mu.Unlock()
	start := slices.IndexFunc(sources, func(source *store.Source) bool { return source.Identifier == next })
	if start <= 0 {
		return slices.Clone(sources)
	}
	return append(slices.Clone(sources[start:]), sources[:start]...)
}

func (r *accountRotation) resumeAt(forgeName, identifier string) {
	r.mu.Lock()
	r.next[forgeName] = identifier
	r.mu.Unlock()
}

// accountName names an account in sync output, such as "GitHub octocat":
// by the login the forge reported, else by its source's name, which already
// starts with the forge's.
func (fs *forgeSource) accountName(source *store.Source, summary forge.Summary) string {
	switch {
	case summary.Login != "":
		return fs.label + " " + summary.Login
	case source.DisplayName.Valid && source.DisplayName.String != "":
		return source.DisplayName.String
	default:
		return fs.label + " " + source.Identifier
	}
}

func (fs *forgeSource) printSummary(out io.Writer, source *store.Source, summary forge.Summary) {
	name := textutil.SanitizeTerminal(fs.accountName(source, summary))
	_, _ = fmt.Fprintf(out, "%s: %d threads archived, %d new messages, %d updated",
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
	if summary.Missed > 0 {
		_, _ = fmt.Fprintf(out, ", %d found but not identified (the next sync looks again)", summary.Missed)
	}
	_, _ = fmt.Fprintln(out)
	if summary.Remaining == 0 {
		return
	}
	if summary.RateLimit != nil && !summary.RateLimit.Reset.IsZero() {
		_, _ = fmt.Fprintf(out, "  %d threads wait for the %s rate limit, which resets at %s; the next sync continues.\n",
			summary.Remaining, fs.label, summary.RateLimit.Reset.Local().Format(time.Kitchen))
		return
	}
	_, _ = fmt.Fprintf(out, "  %d threads wait for the next sync.\n", summary.Remaining)
}
