package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/tui"
)

var forceLocalTUI bool
var deprecatedTUIForceSQL bool
var deprecatedTUISkipCacheBuild bool
var deprecatedTUINoSQLiteScanner bool

var _ peoplebrowser.Backend = (*daemonclient.PeopleBrowser)(nil)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Open the interactive terminal UI",
	Long: `Open an interactive terminal UI for browsing email, text messages,
meeting transcripts, and people.

Email mode provides aggregate views by:
  - Senders: Who sends you the most email
  - Recipients: Who you email most frequently
  - Domains: Which domains you interact with
  - Labels: Gmail label distribution
  - Time: Message volume over time

Press 'm' to cycle through Email, Texts, Meetings, and People. Texts is skipped
when its engine is unavailable. Meetings remains available before a source is
configured. People browses every observed contact and remains available when
Texts is absent.

Navigation:
  ↑/k, ↓/j    Move up/down
  PgUp/PgDn   Page up/down
  Enter       Drill down / view message
  Esc         Go back
  m           Cycle Email / Texts / Meetings / People
  ,           Open Settings
  g           Cycle aggregate view (Email and Texts)
  /           Search; Tab adds active-message-only Semantic mode when enabled
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

		// The shipped daemon engine provides Texts directly. People uses the
		// focused wrapper because Engine.Search has a different query contract.
		var textEngine query.TextEngine = backend.engine
		peopleBackend := tuiPeopleBackend(cmd.Context(), backend.client, backend.engine)

		notice := analyticsCacheNotice(cmd.Context(), backend.client)
		if notice != "" {
			fmt.Println(notice)
		}

		// Create and run TUI
		semanticSearch := tuiSemanticSearcher(cmd.Context(), backend.client, backend.engine)
		model := tui.New(backend.engine, tui.Options{
			DataDir:          cfg.Data.DataDir,
			Version:          Version,
			TextEngine:       textEngine,
			PeopleBackend:    peopleBackend,
			ManifestSaver:    backend.client,
			AttachmentReader: tuiAttachmentOpener{client: backend.client},
			SemanticSearch:   semanticSearch,
			AnalyticsNotice:  notice,
			SettingsBackend:  backend.settings,
		})
		p := tea.NewProgram(model)
		noticeCtx, stopNoticeRefresh := context.WithCancel(cmd.Context())
		defer stopNoticeRefresh()
		if notice != "" {
			go refreshAnalyticsCacheNotice(
				noticeCtx,
				backend.client,
				analyticsCacheNoticeRefreshInterval,
				p.Send,
			)
		}

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

func tuiSemanticSearcher(
	ctx context.Context,
	client *daemonclient.Client,
	engine query.Engine,
) query.SemanticMessageSearcher {
	if client == nil || engine == nil {
		return nil
	}
	compatible, err := client.SupportsAPISchemaVersion(ctx, semanticSearchMinAPISchemaVersion)
	if err != nil || !compatible {
		return nil
	}
	available, err := client.VectorSearchAvailableForMessageType(ctx, tuiSemanticMessageType)
	if err != nil || !available {
		return nil
	}
	searcher, _ := engine.(query.SemanticMessageSearcher)
	return searcher
}

func tuiPeopleBackend(
	ctx context.Context,
	client *daemonclient.Client,
	engine *daemonclient.Engine,
) *daemonclient.PeopleBrowser {
	if client == nil || engine == nil {
		return nil
	}
	compatible, err := client.SupportsAPISchemaVersion(ctx, peopleMinAPISchemaVersion)
	if err != nil || !compatible {
		return nil
	}
	return daemonclient.NewPeopleBrowser(engine)
}

const (
	semanticSearchMinAPISchemaVersion = "2.7.0"
	peopleMinAPISchemaVersion         = "2.10.0"
	tuiSemanticMessageType            = "email"
)

type tuiAttachmentOpener struct {
	client *daemonclient.Client
}

func (o tuiAttachmentOpener) OpenAttachment(ctx context.Context, contentHash string) (io.ReadCloser, error) {
	return o.client.OpenCLIAttachment(ctx, contentHash)
}

type tuiBackend struct {
	engine   *daemonclient.Engine
	client   *daemonclient.Client
	settings tui.SettingsBackend
	info     HTTPStoreInfo
	cleanup  func()
}

const (
	analyticsCacheNoticeRefreshInterval = time.Second
	analyticsCacheFallbackNotice        = "Aggregate views are using live SQL while the analytics cache initializes or because no usable cache is available; they may load slowly. If this continues, run 'msgvault build-cache', then restart the daemon."
)

// analyticsCacheNotice asks the daemon which analytics engine currently
// serves requests (GET /health, no cache scans or archive access). Deliberate
// live SQL (engine = "sql", PostgreSQL) reports a different mode and stays
// silent, as do daemons predating the field. Best-effort: errors return an
// empty notice rather than blocking launch.
func analyticsCacheNotice(ctx context.Context, client *daemonclient.Client) string {
	mode, err := currentAnalyticsMode(ctx, client)
	if err != nil || mode != api.AnalyticsModeSQLFallback {
		return ""
	}
	return analyticsCacheFallbackNotice
}

func currentAnalyticsMode(ctx context.Context, client *daemonclient.Client) (string, error) {
	if client == nil {
		return "", errors.New("daemon client unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	health, err := client.GetHealth(ctx)
	if err != nil {
		return "", err
	}
	return health.AnalyticsEngine, nil
}

// refreshAnalyticsCacheNotice clears the launch notice when a background
// cache initialization swaps the daemon to DuckDB. Health errors are
// transient and leave the current notice unchanged.
func refreshAnalyticsCacheNotice(
	ctx context.Context,
	client *daemonclient.Client,
	interval time.Duration,
	send func(tea.Msg),
) {
	if client == nil || send == nil {
		return
	}
	if interval <= 0 {
		interval = analyticsCacheNoticeRefreshInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mode, err := currentAnalyticsMode(ctx, client)
			if err != nil {
				continue
			}
			if mode == api.AnalyticsModeDuckDB {
				send(tui.AnalyticsNoticeMsg{})
				return
			}
		}
	}
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
		engine:   engine,
		client:   st,
		settings: newTUISettingsBackend(st),
		info:     info,
		cleanup:  func() { _ = engine.Close() },
	}, nil
}

func init() {
	rootCmd.AddCommand(tuiCmd)
	tuiCmd.Flags().BoolVar(&forceLocalTUI, localValue, false, "Use the local daemon instead of the configured remote server")
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
