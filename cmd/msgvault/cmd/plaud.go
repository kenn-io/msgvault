package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/plaud"
	"go.kenn.io/msgvault/internal/store"
)

const plaudConfigHint = `Add to your config.toml:

  [[plaud]]
  identifier = "work"
  account_email = "you@example.com"
  enabled = true
  # schedule = "30 */6 * * *"

Then run 'msgvault add-plaud work' on the daemon host to authorize via browser`

const plaudCheckOwnerOnlyFlag = "check-owner-only"

func resolvePlaudSource(args []string, cfg *config.Config) (*config.PlaudSource, error) {
	if cfg == nil {
		return nil, errors.New("configuration is unavailable")
	}
	if len(cfg.Plaud) == 0 {
		return nil, errors.New("no [[plaud]] sources configured\n\n" + plaudConfigHint)
	}
	if len(args) > 0 {
		src := cfg.GetPlaudSource(args[0])
		if src == nil {
			return nil, fmt.Errorf("no [[plaud]] entry with identifier %q", args[0])
		}
		return src, nil
	}
	if len(cfg.Plaud) > 1 {
		return nil, errors.New("multiple [[plaud]] sources configured; pass an identifier")
	}
	src := cfg.Plaud[0]
	return &src, nil
}

func plaudManager(src *config.PlaudSource, state *invocation) *plaud.Manager {
	return plaud.NewManager(src.Endpoint, state.cfg.TokensDir(), state.logger)
}

func newAddPlaudCmd() *cobra.Command {
	cmd := newAddPlaudLocalCmd()
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			checkOnly, err := cmd.Flags().GetBool(plaudCheckOwnerOnlyFlag)
			if err != nil {
				return fmt.Errorf("read Plaud owner-check flag: %w", err)
			}
			if !checkOnly {
				if err := preflightAddPlaudAuthorize(cmd, args); err != nil {
					return err
				}
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runAddPlaudLocal(cmd, args)
	}
	return cmd
}

func newAddPlaudLocalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-plaud [identifier]",
		Short: "Authorize and register a Plaud cloud account",
		Long:  "Authorize a configured Plaud account using browser OAuth on the daemon host. The live account email must match account_email before registration.",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runAddPlaudLocal,
	}
	registerOAuthPreflightedFlag(cmd)
	cmd.Flags().Bool(plaudCheckOwnerOnlyFlag, false, "Internal: Check the archive owner before browser authorization")
	if err := cmd.Flags().MarkHidden(plaudCheckOwnerOnlyFlag); err != nil {
		panic(err)
	}
	return cmd
}

func preflightAddPlaudAuthorize(cmd *cobra.Command, args []string) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	if IsRemoteMode(state) {
		return errors.New("add-plaud cannot run through a configured remote: the localhost OAuth callback runs on the daemon host; run msgvault add-plaud there, or SSH with localhost:8091 forwarded and run msgvault --local add-plaud on the daemon host")
	}
	src, err := resolvePlaudSource(args, state.cfg)
	if err != nil {
		return err
	}
	email, err := src.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	runArgs, err := daemonCLIArgsFromCobra(cmd, args)
	if err != nil {
		return err
	}
	// The daemon owns the archive. Check its binding before replacing tokens.
	runArgs = append(runArgs, "--"+plaudCheckOwnerOnlyFlag)
	if err := runDaemonCLICommandHTTPWithEnv(cmd, runArgs, nil, false, false); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Authorizing %s with Plaud...\n", src.Identifier)
	if _, err = authorizePlaudAccount(cmd.Context(), plaudManager(src, state), src.Identifier, email); err != nil {
		return fmt.Errorf("authorize Plaud: %w", err)
	}
	if err := cmd.Flags().Set(oauthPreflightedFlag, "true"); err != nil {
		return fmt.Errorf("record Plaud authorization preflight: %w", err)
	}
	return nil
}

func runAddPlaudLocal(cmd *cobra.Command, args []string) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	src, err := resolvePlaudSource(args, state.cfg)
	if err != nil {
		return err
	}
	email, err := src.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	if err := validatePlaudOwnerBeforeAuthorization(state, src.Identifier, email); err != nil {
		return err
	}
	checkOnly, err := cmd.Flags().GetBool(plaudCheckOwnerOnlyFlag)
	if err != nil {
		return fmt.Errorf("read Plaud owner-check flag: %w", err)
	}
	if checkOnly {
		return nil
	}
	done, err := oauthPreflighted(cmd)
	if err != nil {
		return err
	}
	mgr := plaudManager(src, state)
	var live string
	if !done {
		live, err = authorizePlaudAccount(cmd.Context(), mgr, src.Identifier, email)
		if err != nil {
			return fmt.Errorf("authorize Plaud: %w", err)
		}
	} else {
		session, err := plaud.Connect(cmd.Context(), mgr.Endpoint(), mgr.Handler(src.Identifier))
		if err != nil {
			return err
		}
		defer func() { _ = session.Close() }()
		live, err = session.CurrentUser(cmd.Context())
		if err != nil {
			return fmt.Errorf("confirm Plaud account: %w", err)
		}
		// Validate live identity before creating a source.
		if err = validatePlaudLiveEmail(email, live); err != nil {
			return err
		}
	}
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err = registerPlaudAccount(st, src.Identifier, email, live); err != nil {
		return err
	}
	if err = runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Plaud account %s authorized. Confirmed account email: %s. Run msgvault sync-plaud %s\n", src.Identifier, email, src.Identifier)
	return nil
}

func authorizePlaudAccount(ctx context.Context, mgr *plaud.Manager, identifier, email string) (string, error) {
	var live string
	err := mgr.AuthorizeWithValidation(ctx, identifier, func(ctx context.Context, handler auth.OAuthHandler) error {
		session, err := plaud.Connect(ctx, mgr.Endpoint(), handler)
		if err != nil {
			return fmt.Errorf("connect to Plaud: %w", err)
		}
		defer func() { _ = session.Close() }()
		live, err = session.CurrentUser(ctx)
		if err != nil {
			return fmt.Errorf("confirm Plaud account: %w", err)
		}
		return validatePlaudLiveEmail(email, live)
	})
	if err != nil {
		return "", err
	}
	return live, nil
}

func validatePlaudLiveEmail(configured, live string) error {
	expected, err := (config.PlaudSource{AccountEmail: configured}).EffectiveAccountEmail()
	if err != nil {
		return err
	}
	actual, err := (config.PlaudSource{AccountEmail: live}).EffectiveAccountEmail()
	if err != nil {
		return errors.New("plaud returned an invalid account email")
	}
	if expected != actual {
		return errors.New("plaud live account does not match configured account_email; authorize the matching account")
	}
	return nil
}

func registerPlaudAccount(st *store.Store, identifier, email, live string) (*store.Source, error) {
	if err := validatePlaudLiveEmail(email, live); err != nil {
		return nil, err
	}
	return plaud.RegisterSource(st, identifier, email)
}

func validatePlaudOwnerBeforeAuthorization(state *invocation, identifier, email string) error {
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return fmt.Errorf("open archive before Plaud authorization: %w", err)
	}
	defer cleanup()

	registered, err := st.GetSourceByTypeAndIdentifier(sourceTypePlaud, identifier)
	if errors.Is(err, store.ErrSourceNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find existing Plaud source before authorization: %w", err)
	}
	if registered.SyncConfig.Valid {
		if err := plaud.ValidateOwner(registered, email); err != nil {
			return fmt.Errorf("validate existing Plaud source owner: %w", err)
		}
	}
	return nil
}

func newSyncPlaudCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sync-plaud [identifier]", Short: "Sync Plaud recordings, transcripts, and notes", Long: `Archive Plaud cloud recordings as searchable meetings. Every run checks full
transcripts, speaker labels and all note tabs for edits. With no identifier,
all configured accounts are synced. --limit starts with newest recordings and
rotates through least recently checked records. --after applies locally and implies
--full. --probe prints tool schemas and first-page counts without personal content.
Cloud Sync and Plaud transcription must already be enabled; audio is not downloaded.`, Args: cobra.MaximumNArgs(1), RunE: runSyncPlaud}
	cmd.Flags().Int("limit", 0, "max records hydrated per run; newest recordings first, then rotate through least recently checked (0 = unlimited)")
	cmd.Flags().String("after", "", "recordings after YYYY-MM-DD (implies --full)")
	cmd.Flags().Bool("full", false, "force archive repair while retaining stable recording IDs")
	cmd.Flags().Bool("probe", false, "print tool schemas and counts without meeting content")
	return addManualSyncCacheFlags(cmd)
}
func plaudImportOptions(cmd *cobra.Command) (plaud.ImportOptions, error) {
	limit, err := cmd.Flags().GetInt("limit")
	if err != nil {
		return plaud.ImportOptions{}, fmt.Errorf("read Plaud limit flag: %w", err)
	}
	if limit < 0 {
		return plaud.ImportOptions{}, usageErr(cmd, errors.New("--limit must be nonnegative"))
	}
	full, err := cmd.Flags().GetBool("full")
	if err != nil {
		return plaud.ImportOptions{}, fmt.Errorf("read Plaud full flag: %w", err)
	}
	after, err := cmd.Flags().GetString("after")
	if err != nil {
		return plaud.ImportOptions{}, fmt.Errorf("read Plaud after flag: %w", err)
	}
	opts := plaud.ImportOptions{Limit: limit, Full: full}
	if after != "" {
		date, err := time.Parse("2006-01-02", after)
		if err != nil {
			return opts, usageErr(cmd, fmt.Errorf("invalid --after %q (expected YYYY-MM-DD): %w", after, err))
		}
		opts.CreatedAfter = &date
		opts.Full = true
	}
	return opts, nil
}
func runSyncPlaud(cmd *cobra.Command, args []string) error {
	opts, err := plaudImportOptions(cmd)
	if err != nil {
		return err
	}
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	if !isDaemonCLISubprocess() {
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
	}
	var sources []config.PlaudSource
	if len(args) > 0 || len(state.cfg.Plaud) == 1 {
		src, err := resolvePlaudSource(args, state.cfg)
		if err != nil {
			return err
		}
		sources = []config.PlaudSource{*src}
	} else {
		sources = state.cfg.Plaud
	}
	if len(sources) == 0 {
		return errors.New("no [[plaud]] sources configured\n\n" + plaudConfigHint)
	}
	probe, err := cmd.Flags().GetBool("probe")
	if err != nil {
		return fmt.Errorf("read Plaud probe flag: %w", err)
	}
	if probe {
		src, err := resolvePlaudSource(args, state.cfg)
		if err != nil {
			return err
		}
		return probePlaud(cmd, src)
	}
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	total := &plaud.ImportSummary{}
	refresh := func() error { return rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state) }
	for _, src := range sources {
		if err = registeredPlaudSource(st, src); err != nil {
			return finishPlaudImport(ctx, src.Identifier, total, err, refresh)
		}
		if err = ctx.Err(); err != nil {
			return finishPlaudImport(ctx, src.Identifier, total, err, refresh)
		}
		email, emailErr := src.EffectiveAccountEmail()
		if emailErr != nil {
			return finishPlaudImport(ctx, src.Identifier, total, emailErr, refresh)
		}
		mgr := plaudManager(&src, state)
		session, connectErr := plaud.Connect(ctx, mgr.Endpoint(), mgr.Handler(src.Identifier))
		if connectErr != nil {
			return finishPlaudImport(ctx, src.Identifier, total, connectErr, refresh)
		}
		runOpts := opts
		runOpts.Identifier = src.Identifier
		runOpts.AccountEmail = email
		runOpts.Progress = func(current, count int, title string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %d/%d %s\n", current, count, title)
		}
		sum, importErr := plaud.NewImporter(st, session).Import(ctx, runOpts)
		_ = session.Close()
		if sum != nil {
			total.MeetingsAdded += sum.MeetingsAdded
			total.MeetingsUpdated += sum.MeetingsUpdated
		}
		if err = finishPlaudImport(ctx, src.Identifier, total, importErr, refresh); err != nil {
			return err
		}
		if sum != nil {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Plaud %s: %d processed, %d added, %d updated (%s)\n", src.Identifier, sum.MeetingsProcessed, sum.MeetingsAdded, sum.MeetingsUpdated, sum.Duration.Round(time.Second))
		}
	}
	if err = ctx.Err(); err != nil {
		return finishPlaudImport(ctx, sources[len(sources)-1].Identifier, total, err, refresh)
	}
	return refresh()
}
func registeredPlaudSource(st *store.Store, src config.PlaudSource) error {
	registered, err := st.GetSourceByTypeAndIdentifier(sourceTypePlaud, src.Identifier)
	if errors.Is(err, store.ErrSourceNotFound) {
		return fmt.Errorf("plaud source %q is not registered; run msgvault add-plaud %s first", src.Identifier, src.Identifier)
	}
	if err != nil {
		return fmt.Errorf("find registered Plaud source: %w", err)
	}
	email, err := src.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	return plaud.ValidateOwner(registered, email)
}
func finishPlaudImport(ctx context.Context, identifier string, sum *plaud.ImportSummary, importErr error, refresh func() error) error {
	operationErr := importErr
	if ctx.Err() != nil {
		operationErr = errors.Join(operationErr, ctx.Err())
	}
	if operationErr == nil {
		return nil
	}
	operationErr = fmt.Errorf("plaud sync %s failed: %w", identifier, operationErr)
	if sum != nil && sum.MeetingsAdded+sum.MeetingsUpdated > 0 && refresh != nil {
		return errors.Join(operationErr, refresh())
	}
	return operationErr
}
func runConfiguredPlaudSync(ctx context.Context, st *store.Store, src config.PlaudSource) error {
	if err := registeredPlaudSource(st, src); err != nil {
		return err
	}
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	email, err := src.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	mgr := plaudManager(&src, state)
	session, err := plaud.Connect(ctx, mgr.Endpoint(), mgr.Handler(src.Identifier))
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	sum, err := plaud.NewImporter(st, session).Import(ctx, plaud.ImportOptions{Identifier: src.Identifier, AccountEmail: email})
	return finishScheduledPlaudImport(ctx, src.Identifier, sum, err, rebuildCacheAfterScheduledSync)
}
func finishScheduledPlaudImport(ctx context.Context, identifier string, sum *plaud.ImportSummary, importErr error, refresh func(context.Context, string) error) error {
	detached := context.WithoutCancel(ctx)
	finish := func() error {
		if refresh == nil {
			return nil
		}
		return refresh(detached, "plaud:"+identifier)
	}
	if err := finishPlaudImport(ctx, identifier, sum, importErr, finish); err != nil {
		return err
	}
	return finish()
}

type plaudProbeSession interface {
	ToolInventory(ctx context.Context) ([]plaud.ToolInfo, error)
	ListFiles(ctx context.Context, page, pageSize int) (plaud.FilePage, error)
}

func probePlaud(cmd *cobra.Command, src *config.PlaudSource) error {
	mgr := plaudManager(src, invocationFromCommand(cmd))
	session, err := plaud.Connect(cmd.Context(), mgr.Endpoint(), mgr.Handler(src.Identifier))
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	return runPlaudProbe(cmd.Context(), cmd.OutOrStdout(), session)
}

func runPlaudProbe(ctx context.Context, out io.Writer, session plaudProbeSession) error {
	tools, err := session.ToolInventory(ctx)
	if err != nil {
		return fmt.Errorf("plaud tool inventory failed: %w", err)
	}
	_, _ = fmt.Fprintln(out, "Tools:")
	for _, tool := range tools {
		schema, err := json.Marshal(tool.InputSchema, json.Deterministic(true))
		if err != nil {
			return fmt.Errorf("format Plaud tool schema failed: %w", err)
		}
		_, _ = fmt.Fprintf(out, "  %s\n    Input schema: %s\n", tool.Name, strings.TrimSpace(string(schema)))
	}
	page, err := session.ListFiles(ctx, 1, 100)
	if err != nil {
		return fmt.Errorf("plaud first-page count failed; verify provider contract: %w", err)
	}
	_, _ = fmt.Fprintf(out, "First-page recordings: %d\n", len(page.Files))
	return nil
}

func init() { rootCmd.AddCommand(newAddPlaudCmd(), newSyncPlaudCmd()) }
