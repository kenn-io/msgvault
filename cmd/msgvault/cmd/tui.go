package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/tui"
)

var forceLocalTUI bool
var deprecatedTUIForceSQL bool
var deprecatedTUISkipCacheBuild bool
var deprecatedTUINoSQLiteScanner bool

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Open the interactive terminal UI",
	Long: `Open an interactive terminal UI for browsing email, text messages,
and meeting transcripts.

Email mode provides aggregate views by:
  - Senders: Who sends you the most email
  - Recipients: Who you email most frequently
  - Domains: Which domains you interact with
  - Labels: Gmail label distribution
  - Time: Message volume over time

Press 'm' to cycle through Email, Texts, and Meetings. Texts is skipped when
its engine is unavailable. Meetings shows a read-only list of Granola and
Circleback transcripts and remains available before a source is configured.

Navigation:
  ↑/k, ↓/j    Move up/down
  PgUp/PgDn   Page up/down
  Enter       Drill down / view message
  Esc         Go back
  m           Cycle Email / Texts / Meetings
  g           Cycle aggregate view (Email and Texts)
  /           Search; find within an open meeting transcript
  A           Filter by account, or meeting source in Meetings mode
  s           Cycle sort field
  r           Reverse sort direction
  t           Toggle time granularity (Time view only)

Email selection:
  Space       Toggle selection
  x           Clear selection
  d           Stage selected messages for deletion

Meeting browsing is read-only; selection and deletion keys are disabled.
Press '?' in any mode for its complete key reference, or 'q' to quit.

Performance:
  The TUI talks to the msgvault HTTP API. Local runs use the daemon, which owns
  database access and server-side cache selection. Run 'msgvault build-cache'
  on the daemon host to prebuild analytics cache files for large archives.

HTTP Mode:
  When [remote].url is configured, the TUI connects to that remote server.
  Otherwise it starts or reuses the local daemon. Use --local to force the local
  daemon when a remote is configured.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		backend, err := openTUIBackend(cmd.Context())
		if err != nil {
			return err
		}
		defer backend.cleanup()
		if backend.info.Kind == HTTPStoreConfiguredRemote {
			fmt.Printf("Connected to remote: %s\n", cfg.Remote.URL)
		}

		// Check if engine supports text queries
		var textEngine query.TextEngine
		if te, ok := backend.engine.(query.TextEngine); ok {
			textEngine = te
		}

		notice := analyticsCacheNotice(cmd.Context(), backend.client)
		if notice != "" {
			fmt.Println(notice)
		}

		// Create and run TUI
		model := tui.New(backend.engine, tui.Options{
			DataDir:          cfg.Data.DataDir,
			Version:          Version,
			TextEngine:       textEngine,
			ManifestSaver:    backend.client,
			AttachmentReader: tuiAttachmentOpener{client: backend.client},
			AnalyticsNotice:  notice,
		})
		p := tea.NewProgram(model)

		// Swap the slog default to a file-only logger for the
		// duration of the TUI. Bubble Tea owns the terminal in
		// alt-screen mode; any stderr write from slog corrupts
		// the render. The daily log file still receives
		// everything, so 'msgvault logs -f' in another pane
		// continues to work for diagnostics.
		prevLogger := slog.Default()
		if logResult != nil {
			slog.SetDefault(logResult.FileOnlyLogger())
		}
		defer slog.SetDefault(prevLogger)

		if _, err := p.Run(); err != nil {
			return fmt.Errorf("run tui: %w", err)
		}

		return nil
	},
}

type tuiAttachmentOpener struct {
	client *daemonclient.Client
}

func (o tuiAttachmentOpener) OpenAttachment(ctx context.Context, contentHash string) (io.ReadCloser, error) {
	return o.client.OpenCLIAttachment(ctx, contentHash)
}

type tuiBackend struct {
	engine  query.Engine
	client  *daemonclient.Client
	info    HTTPStoreInfo
	cleanup func()
}

// analyticsCacheNotice asks the daemon which analytics engine it actually
// selected at startup (GET /health, no cache scans or archive access) and
// warns when aggregate views run live SQL only because no usable cache
// existed then. Deliberate live SQL (engine = "sql", PostgreSQL) reports a
// different mode and stays silent, as do daemons predating the field. The
// mode is fixed for the daemon's lifetime, so the notice stays accurate —
// and keeps firing — after a cache build until the daemon restarts.
// Best-effort: errors return an empty notice rather than blocking launch.
func analyticsCacheNotice(ctx context.Context, client *daemonclient.Client) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	health, err := client.GetHealth(ctx)
	if err != nil || health.AnalyticsEngine != api.AnalyticsModeSQLFallback {
		return ""
	}
	return "Aggregate views are using live SQL because the daemon started without a usable analytics cache; they may load slowly. Run 'msgvault build-cache', then restart the daemon."
}

func openTUIBackend(ctx context.Context) (*tuiBackend, error) {
	if forceLocalTUI {
		previousUseLocal := useLocal
		useLocal = true
		defer func() { useLocal = previousUseLocal }()
	}

	st, info, err := OpenHTTPStore(ctx)
	if err != nil {
		return nil, err
	}
	engine := daemonclient.NewEngineAdapter(st)
	return &tuiBackend{
		engine:  engine,
		client:  st,
		info:    info,
		cleanup: func() { _ = engine.Close() },
	}, nil
}

func init() {
	rootCmd.AddCommand(tuiCmd)
	tuiCmd.Flags().BoolVar(&forceLocalTUI, "local", false, "Use the local daemon instead of the configured remote server")
	tuiCmd.Flags().BoolVar(&deprecatedTUIForceSQL, "force-sql", false, "Deprecated in 0.17.0: set [analytics].engine = \"sql\" in config.toml")
	tuiCmd.Flags().BoolVar(&deprecatedTUISkipCacheBuild, "no-cache-build", false, "Deprecated in 0.17.0: set [analytics].auto_build_cache = false in config.toml")
	tuiCmd.Flags().BoolVar(&deprecatedTUINoSQLiteScanner, "no-sqlite-scanner", false, "Deprecated in 0.17.0: cache engine selection is daemon-managed")
	_ = tuiCmd.Flags().MarkDeprecated("force-sql", "deprecated in 0.17.0; set [analytics].engine = \"sql\" in config.toml")
	_ = tuiCmd.Flags().MarkDeprecated("no-cache-build", "deprecated in 0.17.0; set [analytics].auto_build_cache = false in config.toml")
	_ = tuiCmd.Flags().MarkDeprecated("no-sqlite-scanner", "deprecated in 0.17.0; cache engine selection is daemon-managed; use [analytics].engine = \"sql\" for live SQL")
	_ = tuiCmd.Flags().MarkHidden("force-sql")
	_ = tuiCmd.Flags().MarkHidden("no-cache-build")
	_ = tuiCmd.Flags().MarkHidden("no-sqlite-scanner")
}
