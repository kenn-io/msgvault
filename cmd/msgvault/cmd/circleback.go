package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/circleback"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

var (
	syncCirclebackLimit int
	syncCirclebackAfter string
	syncCirclebackFull  bool
	syncCirclebackProbe bool
)

const circlebackConfigHint = `Add to your config.toml:

  [[circleback]]
  identifier = "you@example.com"   # label for this account
  account_email = "you@example.com" # primary identity for organizer attribution
  enabled = true
  # schedule = "30 */6 * * *"      # optional daemon schedule

Then run 'msgvault add-circleback <identifier>' to authorize via browser`

// resolveCirclebackSource picks the [[circleback]] entry for an optional CLI
// argument: an explicit identifier must match a configured entry; with no
// argument there must be exactly one entry.
func resolveCirclebackSource(args []string) (*config.CirclebackSource, error) {
	if len(cfg.Circleback) == 0 {
		return nil, errors.New("no [[circleback]] sources configured\n\n" + circlebackConfigHint)
	}
	if len(args) > 0 {
		src := cfg.GetCirclebackSource(args[0])
		if src == nil {
			var ids []string
			for _, s := range cfg.Circleback {
				ids = append(ids, s.Identifier)
			}
			return nil, fmt.Errorf("no [[circleback]] entry with identifier %q (configured: %s)", args[0], strings.Join(ids, ", "))
		}
		return src, nil
	}
	if len(cfg.Circleback) > 1 {
		return nil, errors.New("multiple [[circleback]] sources configured; pass an identifier")
	}
	src := cfg.Circleback[0]
	return &src, nil
}

func circlebackManager(src *config.CirclebackSource) *circleback.Manager {
	return circleback.NewManager(src.Endpoint, cfg.TokensDir(), logger)
}

func newAddCirclebackCmd() *cobra.Command {
	cmd := newAddCirclebackLocalCmd()
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			if err := preflightAddCirclebackAuthorize(cmd, args); err != nil {
				return err
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runAddCirclebackLocal(cmd, args)
	}
	return cmd
}

// preflightAddCirclebackAuthorize runs the browser OAuth flow in this
// process before proxying, so the daemon subprocess never opens a browser or
// waits on human consent while holding the operation gate (add-teams
// pattern).
func preflightAddCirclebackAuthorize(cmd *cobra.Command, args []string) error {
	if err := validateAddCirclebackOAuthRouting(); err != nil {
		return err
	}
	src, err := resolveCirclebackSource(args)
	if err != nil {
		return err
	}
	if _, err := src.EffectiveAccountEmail(); err != nil {
		return err
	}
	fmt.Printf("Authorizing %s with Circleback...\n", src.Identifier)
	if err := circlebackManager(src).Authorize(cmd.Context(), src.Identifier); err != nil {
		return fmt.Errorf("authorize Circleback: %w", err)
	}
	if err := cmd.Flags().Set(oauthPreflightedFlag, "true"); err != nil {
		return fmt.Errorf("set --%s after authorization: %w", oauthPreflightedFlag, err)
	}
	return nil
}

func validateAddCirclebackOAuthRouting() error {
	if !IsRemoteMode() {
		return nil
	}
	return errors.New("add-circleback cannot run through a configured remote: the localhost OAuth callback would run on the daemon host; run msgvault add-circleback on the daemon host, or use --local to authorize a local account")
}

func newAddCirclebackLocalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-circleback [identifier]",
		Short: "Authorize a Circleback account (browser OAuth)",
		Long: `Authorize a configured Circleback account using OAuth.

This opens a browser for Circleback authorization (their MCP server uses
OAuth with dynamic client registration), then stores the token for meeting
ingestion.

Examples:
  msgvault add-circleback
  msgvault add-circleback you@example.com`,
		Args: cobra.MaximumNArgs(1),
		RunE: runAddCirclebackLocal,
	}
	registerOAuthPreflightedFlag(cmd)
	return cmd
}

func runAddCirclebackLocal(cmd *cobra.Command, args []string) error {
	src, err := resolveCirclebackSource(args)
	if err != nil {
		return err
	}
	accountEmail, err := src.EffectiveAccountEmail()
	if err != nil {
		return err
	}

	preflighted, err := oauthPreflighted(cmd)
	if err != nil {
		return err
	}
	if !preflighted {
		fmt.Printf("Authorizing %s with Circleback...\n", src.Identifier)
		if err := circlebackManager(src).Authorize(cmd.Context(), src.Identifier); err != nil {
			return fmt.Errorf("authorize Circleback: %w", err)
		}
	}

	s, cleanup, err := openWritableStoreAndInitForIngest()
	if err != nil {
		return err
	}
	defer cleanup()

	if _, err := registerMeetingSource(
		cmd.OutOrStdout(), s, sourceTypeCircleback, src.Identifier, accountEmail,
	); err != nil {
		return err
	}
	if err := runPostSourceCreateMigrations(s); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nCircleback account authorized successfully!\n")
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Identifier: %s\n\n", src.Identifier)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "You can now run:")
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  msgvault sync-circleback %s\n", src.Identifier)
	return nil
}

var syncCirclebackCmd = &cobra.Command{
	Use:   "sync-circleback [identifier]",
	Short: "Sync Circleback meetings, notes, and transcripts",
	Long: `Sync meetings, notes, action items, and transcripts from Circleback.

Incremental by default: each run searches from 48 hours before the last
successful run's newest meeting, so late edits are picked up; re-fetched
meetings are upserted in place. With no identifier, every configured
[[circleback]] source is synced.

Use --full to re-fetch everything; --after bounds a full sync. --probe
prints the server's tool inventory and a sample search result instead of
syncing (for diagnosing schema drift). --limit caps newly searched meetings;
due transcript maintenance items are additional work outside that cap.
Limited runs do not save search traversal position, so repeated limited runs
may revisit the same meetings; an unlimited run is required to complete sync.

Examples:
  msgvault sync-circleback
  msgvault sync-circleback you@example.com --limit 5
  msgvault sync-circleback --full --after 2024-01-01
  msgvault sync-circleback --probe`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}

		var sources []config.CirclebackSource
		if len(args) > 0 || len(cfg.Circleback) == 1 {
			src, err := resolveCirclebackSource(args)
			if err != nil {
				return err
			}
			sources = []config.CirclebackSource{*src}
		} else {
			sources = cfg.Circleback
		}
		if len(sources) == 0 {
			return errors.New("no [[circleback]] sources configured\n\n" + circlebackConfigHint)
		}

		var after time.Time
		if syncCirclebackAfter != "" {
			t, err := time.Parse("2006-01-02", syncCirclebackAfter)
			if err != nil {
				return usageErr(cmd, fmt.Errorf("invalid --after %q (expected YYYY-MM-DD): %w", syncCirclebackAfter, err))
			}
			after = t.UTC()
		}

		if syncCirclebackProbe {
			src := sources[0]
			return probeCircleback(cmd, &src)
		}

		s, cleanup, err := openWritableStoreAndInitForIngest()
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()

		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigChan)
		go func() {
			select {
			case <-sigChan:
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "\nInterrupted. Stopping Circleback sync...")
				cancel()
			case <-ctx.Done():
			}
		}()

		pendingCacheWrites := &circleback.ImportSummary{}
		for i := range sources {
			src := sources[i]
			accountEmail, err := src.EffectiveAccountEmail()
			if err != nil {
				return finishCirclebackImport(ctx, src.Identifier, pendingCacheWrites, err, func() {
					rebuildCacheAfterWrite(dbPath)
				})
			}
			if ctx.Err() != nil {
				return finishCirclebackImport(ctx, src.Identifier, pendingCacheWrites, nil, func() {
					rebuildCacheAfterWrite(dbPath)
				})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Circleback for %s\n\n", src.Identifier)

			mgr := circlebackManager(&src)
			session, err := circleback.Connect(ctx, mgr.Endpoint(), mgr.Handler(src.Identifier))
			if err != nil {
				return finishCirclebackImport(ctx, src.Identifier, pendingCacheWrites, err, func() {
					rebuildCacheAfterWrite(dbPath)
				})
			}
			imp := circleback.NewImporter(s, session)
			sum, err := imp.Import(ctx, circleback.ImportOptions{
				Identifier:   src.Identifier,
				AccountEmail: accountEmail,
				Full:         syncCirclebackFull || !after.IsZero(),
				Limit:        syncCirclebackLimit,
				CreatedAfter: after,
				Progress:     func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) },
			})
			accumulateCirclebackWrites(pendingCacheWrites, sum)
			_ = session.Close()
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run sync-circleback to resume.")
			}
			if finishErr := finishCirclebackImport(ctx, src.Identifier, pendingCacheWrites, err, func() {
				rebuildCacheAfterWrite(dbPath)
			}); finishErr != nil {
				return finishErr
			}

			writeCirclebackSummary(cmd.OutOrStdout(), sum)
		}

		if ctx.Err() != nil {
			return finishCirclebackImport(ctx, sources[len(sources)-1].Identifier, pendingCacheWrites, nil, func() {
				rebuildCacheAfterWrite(dbPath)
			})
		}
		rebuildCacheAfterWrite(dbPath)
		return nil
	},
}

func finishCirclebackImport(
	ctx context.Context,
	identifier string,
	sum *circleback.ImportSummary,
	importErr error,
	refreshCache func(),
) error {
	cancelErr := ctx.Err()
	if cancelErr == nil && errors.Is(importErr, context.Canceled) {
		cancelErr = context.Canceled
	}
	if cancelErr == nil && importErr == nil {
		return nil
	}
	if sum != nil && sum.MeetingsAdded+sum.MeetingsUpdated > 0 && refreshCache != nil {
		refreshCache()
	}
	if cancelErr != nil {
		return fmt.Errorf("circleback sync %s canceled: %w", identifier, cancelErr)
	}
	return fmt.Errorf("circleback sync %s failed: %w", identifier, importErr)
}

func accumulateCirclebackWrites(total, current *circleback.ImportSummary) {
	if total == nil || current == nil {
		return
	}
	total.MeetingsAdded += current.MeetingsAdded
	total.MeetingsUpdated += current.MeetingsUpdated
}

func writeCirclebackSummary(out io.Writer, sum *circleback.ImportSummary) {
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Circleback sync complete!")
	_, _ = fmt.Fprintf(out, "  Duration:           %s\n", sum.Duration.Round(time.Second))
	_, _ = fmt.Fprintf(out, "  Meetings processed: %d\n", sum.MeetingsProcessed)
	_, _ = fmt.Fprintf(out, "  Meetings added:     %d\n", sum.MeetingsAdded)
	if sum.MaintenanceRetries > 0 {
		_, _ = fmt.Fprintf(out, "  Maintenance items: %d (outside --limit; includes terminal expiry)\n", sum.MaintenanceRetries)
	}
	if sum.Errors > 0 {
		_, _ = fmt.Fprintf(out, "  Errors:             %d\n", sum.Errors)
	}
}

// probeCircleback prints the MCP tool inventory and one raw SearchMeetings
// result so field-name drift can be diagnosed without touching the archive.
func probeCircleback(cmd *cobra.Command, src *config.CirclebackSource) error {
	mgr := circlebackManager(src)
	session, err := circleback.Connect(cmd.Context(), mgr.Endpoint(), mgr.Handler(src.Identifier))
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	return runCirclebackProbe(cmd.Context(), cmd.OutOrStdout(), session)
}

type circlebackProbeSession interface {
	ToolInventory(ctx context.Context) ([]circleback.ToolInfo, error)
	CallToolJSON(ctx context.Context, name string, args map[string]any) (json.RawMessage, error)
}

// runCirclebackProbe prints the provider contract before making one read-only
// first-page search call. Keeping the connected session behind this seam makes
// the probe behavior testable without OAuth or network access.
func runCirclebackProbe(ctx context.Context, out io.Writer, session circlebackProbeSession) error {
	tools, err := session.ToolInventory(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "Tools:")
	for _, tool := range tools {
		schema, err := json.MarshalIndent(tool.InputSchema, "", "  ")
		if err != nil {
			return fmt.Errorf("format input schema for Circleback tool %s: %w", tool.Name, err)
		}
		indentedSchema := "      " + strings.ReplaceAll(string(schema), "\n", "\n      ")
		_, _ = fmt.Fprintf(out, "  %s\n    Description: %s\n    Input schema:\n%s\n",
			tool.Name, tool.Description, indentedSchema)
	}

	raw, err := session.CallToolJSON(ctx, "SearchMeetings", map[string]any{
		"intent":    "Inspecting Circleback meetings for msgvault archival.",
		"pageIndex": 0,
	})
	if err != nil {
		return fmt.Errorf("sample SearchMeetings call: %w", err)
	}
	_, _ = fmt.Fprintln(out, "\nSample SearchMeetings result:")
	_, _ = fmt.Fprintln(out, string(raw))
	return nil
}

// runConfiguredCirclebackSync is the daemon-scheduler entry point for one
// [[circleback]] source.
func runConfiguredCirclebackSync(ctx context.Context, st *store.Store, src config.CirclebackSource) error {
	registered, err := st.ListSources(circleback.SourceType)
	if err != nil {
		return fmt.Errorf("list registered Circleback sources: %w", err)
	}
	found := false
	for _, candidate := range registered {
		if candidate.Identifier == src.Identifier {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("circleback source %q is not registered; run msgvault add-circleback %s first",
			src.Identifier, src.Identifier)
	}
	accountEmail, err := src.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	mgr := circleback.NewManager(src.Endpoint, cfg.TokensDir(), logger)
	session, err := circleback.Connect(ctx, mgr.Endpoint(), mgr.Handler(src.Identifier))
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	imp := circleback.NewImporter(st, session)
	sum, err := imp.Import(ctx, circleback.ImportOptions{
		Identifier:   src.Identifier,
		AccountEmail: accountEmail,
	})
	return finishScheduledCirclebackImport(ctx, src.Identifier, sum, err, rebuildCacheAfterScheduledSync)
}

func finishScheduledCirclebackImport(
	ctx context.Context,
	identifier string,
	sum *circleback.ImportSummary,
	importErr error,
	refreshCache func(context.Context, string),
) error {
	refreshCtx := context.WithoutCancel(ctx)
	refresh := func() {
		if refreshCache != nil {
			refreshCache(refreshCtx, "circleback:"+identifier)
		}
	}
	if err := finishCirclebackImport(ctx, identifier, sum, importErr, refresh); err != nil {
		return err
	}
	refresh()
	return nil
}

func init() {
	syncCirclebackCmd.Flags().IntVar(&syncCirclebackLimit, "limit", 0,
		"max newly searched meetings per partial run; maintenance items are additional; an unlimited run is required to complete sync (0 = unlimited)")
	syncCirclebackCmd.Flags().StringVar(&syncCirclebackAfter, "after", "", "full-sync only meetings after this date (YYYY-MM-DD; implies --full)")
	syncCirclebackCmd.Flags().BoolVar(&syncCirclebackFull, "full", false, "ignore the stored creation watermark and re-fetch every meeting (repairs existing rows in place)")
	syncCirclebackCmd.Flags().BoolVar(&syncCirclebackProbe, "probe", false, "print the MCP tool inventory and a sample result instead of syncing")
	rootCmd.AddCommand(newAddCirclebackCmd())
	rootCmd.AddCommand(syncCirclebackCmd)
}
