package cmd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/remote"
	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/tui"
)

var forceSQL bool
var skipCacheBuild bool
var noSQLiteScanner bool
var forceLocalTUI bool

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Open the interactive terminal UI",
	Long: `Open an interactive terminal UI for browsing your email archive.

The TUI provides aggregate views of your messages by:
  - Senders: Who sends you the most email
  - Recipients: Who you email most frequently
  - Domains: Which domains you interact with
  - Labels: Gmail label distribution
  - Time: Message volume over time

Navigation:
  ↑/k, ↓/j    Move up/down
  PgUp/PgDn   Page up/down
  Enter       Drill down / view message
  Esc         Go back
  Tab         Switch view (aggregates only)
  s           Cycle sort field
  r           Reverse sort direction
  t           Toggle time granularity (Time view only)

Selection & Deletion:
  Space       Toggle selection
  A           Select all visible
  x           Clear selection
  D           Stage selected for deletion
  q           Quit

Performance:
  For large archives (100k+ messages), the TUI uses Parquet files for fast
  aggregation queries. Run 'msgvault build-cache' to generate them.
  Use --force-sql to bypass Parquet and query SQLite directly (slow).

Remote Mode:
  When [remote].url is configured, the TUI connects to a remote msgvault server.
  Use --local to force local database. Deletion and export are disabled in remote mode.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		var engine query.Engine
		var isRemote bool

		// Check for remote mode (unless --local flag is set)
		if cfg.Remote.URL != "" && !forceLocalTUI {
			// Remote mode - connect to remote msgvault server
			remoteCfg := remote.Config{
				URL:           cfg.Remote.URL,
				APIKey:        cfg.Remote.APIKey,
				AllowInsecure: cfg.Remote.AllowInsecure,
			}
			remoteEngine, err := remote.NewEngine(remoteCfg)
			if err != nil {
				return fmt.Errorf("connect to remote: %w", err)
			}
			defer func() { _ = remoteEngine.Close() }()
			engine = remoteEngine
			isRemote = true
			fmt.Printf("Connected to remote: %s\n", cfg.Remote.URL)
		} else {
			// Local mode - use local database
			dbPath := cfg.DatabaseDSN()
			s, err := store.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer func() { _ = s.Close() }()

			// Ensure schema is up to date
			if err := s.InitSchema(); err != nil {
				return fmt.Errorf("init schema: %w", err)
			}
			if err := runStartupMigrations(s); err != nil {
				return fmt.Errorf("startup migrations: %w", err)
			}

			// Build FTS index in background — TUI uses DuckDB/Parquet for
			// aggregates and only needs FTS for deep search (Tab to switch).
			if s.NeedsFTSBackfill() {
				go func() {
					_, _ = s.BackfillFTS(nil)
				}()
			}

			analyticsDir := cfg.AnalyticsDir()

			// Check if cache needs to be built/updated (unless forcing SQL or skipping)
			if !forceSQL && !skipCacheBuild {
				staleness := cacheNeedsBuild(dbPath, analyticsDir)
				if staleness.NeedsBuild {
					fmt.Printf("Building analytics cache (%s)...\n", staleness.Reason)
					result, err := buildCache(dbPath, analyticsDir, staleness.FullRebuild)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Warning: Failed to build cache: %v\n", err)
						fmt.Fprintf(os.Stderr, "Falling back to SQLite (may be slow for large archives)\n")
					} else if !result.Skipped {
						fmt.Printf("Cached %d messages for fast queries.\n", result.ExportedCount)
					}
				}
			}

			// Determine query engine to use
			if !forceSQL && query.HasCompleteParquetData(analyticsDir) {
				// Use DuckDB for fast Parquet queries
				var duckOpts query.DuckDBOptions
				if noSQLiteScanner {
					duckOpts.DisableSQLiteScanner = true
				}
				duckEngine, err := query.NewDuckDBEngine(analyticsDir, dbPath, s.DB(), duckOpts)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: Failed to open Parquet engine: %v\n", err)
					fmt.Fprintf(os.Stderr, "Falling back to SQLite (may be slow for large archives)\n")
					engine = query.NewSQLiteEngine(s.DB())
				} else {
					engine = duckEngine
					defer func() { _ = duckEngine.Close() }()
				}
			} else {
				// Use SQLite directly
				if !forceSQL {
					fmt.Fprintf(os.Stderr, "Note: No cache data available, using SQLite (slow for large archives)\n")
					fmt.Fprintf(os.Stderr, "Run 'msgvault build-cache' to enable fast queries.\n")
				}
				engine = query.NewSQLiteEngine(s.DB())
			}
		}

		// Check if engine supports text queries
		var textEngine query.TextEngine
		if te, ok := engine.(query.TextEngine); ok {
			textEngine = te
		}

		// Create and run TUI
		model := tui.New(engine, tui.Options{
			DataDir:    cfg.Data.DataDir,
			Version:    Version,
			IsRemote:   isRemote,
			TextEngine: textEngine,
		})
		p := tea.NewProgram(model, tea.WithAltScreen())

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

// cacheStaleness describes why the analytics cache needs a rebuild.
type cacheStaleness struct {
	NeedsBuild  bool
	HasNew      bool // new messages since last build
	HasDeleted  bool // deletions since last build
	HasUpdated  bool // existing messages mutated since last build
	FullRebuild bool // must rewrite all shards (not incremental)
	Reason      string
}

// cacheNeedsBuild checks if the analytics cache needs to be built or
// updated. Collects all staleness signals before returning so that
// e.g. a mixed add+delete sync correctly reports both.
func cacheNeedsBuild(dbPath, analyticsDir string) cacheStaleness {
	messagesDir := filepath.Join(analyticsDir, "messages")
	stateFile := filepath.Join(analyticsDir, "_last_sync.json")

	hasParquetData := query.HasParquetData(analyticsDir)

	// Load last sync state
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if !hasParquetData {
			return cacheStaleness{
				NeedsBuild: true, FullRebuild: true,
				Reason: "no cache exists",
			}
		}
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "no sync state found",
		}
	}

	var state syncState
	if err := json.Unmarshal(data, &state); err != nil {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "invalid sync state",
		}
	}

	db, err := store.Open(dbPath)
	if err != nil {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "cannot verify cache status",
		}
	}
	defer func() { _ = db.Close() }()

	var maxID int64
	err = db.DB().QueryRow(`
		SELECT COALESCE(MAX(id), 0) FROM messages
		WHERE deleted_from_source_at IS NULL AND sent_at IS NOT NULL
	`).Scan(&maxID)
	if err != nil {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "cannot verify cache status",
		}
	}

	if maxID == 0 && state.LastMessageID == 0 {
		return cacheStaleness{}
	}

	if !hasParquetData {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "no cache exists",
		}
	}

	// Collect staleness signals without short-circuiting so a mixed
	// add+delete sync correctly triggers a full rebuild.
	var reasons []string
	result := cacheStaleness{}

	if maxID > state.LastMessageID {
		newCount := maxID - state.LastMessageID
		result.HasNew = true
		reasons = append(reasons,
			fmt.Sprintf("%d new messages", newCount))
	}

	syncAtStr := state.LastSyncAt.UTC().Format("2006-01-02 15:04:05")
	var deletedSinceBuild int64
	err = db.DB().QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE deleted_from_source_at IS NOT NULL
		  AND deleted_from_source_at >= ?
	`, syncAtStr).Scan(&deletedSinceBuild)
	if err != nil {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "cannot verify deletion state",
		}
	}
	if deletedSinceBuild > 0 {
		result.HasDeleted = true
		result.FullRebuild = true
		reasons = append(reasons,
			fmt.Sprintf("%d deletions", deletedSinceBuild))
	}

	// Dedup-hidden rows (deleted_at) are excluded from the messages
	// Parquet export, so a dedup run after the last cache build leaves
	// stale duplicate rows in the cache. Detect that by counting hides
	// since LastSyncAt and force a full rebuild if any are present.
	// The deleted_from_source_at IS NULL clause keeps the count
	// disjoint from the deletedSinceBuild count above so a row that is
	// both source-deleted and dedup-hidden after LastSyncAt is reported
	// once (as a deletion), not double-counted in the reason string.
	var hiddenSinceBuild int64
	err = db.DB().QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE deleted_at IS NOT NULL
		  AND deleted_at >= ?
		  AND deleted_from_source_at IS NULL
	`, syncAtStr).Scan(&hiddenSinceBuild)
	if err != nil {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "cannot verify dedup state",
		}
	}
	if hiddenSinceBuild > 0 {
		result.HasDeleted = true
		result.FullRebuild = true
		reasons = append(reasons,
			fmt.Sprintf("%d dedup-hidden", hiddenSinceBuild))
	}

	var hasSyncRunsTable int
	err = db.DB().QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'sync_runs'
	`).Scan(&hasSyncRunsTable)
	if err != nil {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "cannot verify sync history",
		}
	}
	if hasSyncRunsTable > 0 {
		var updatedSinceBuild int64
		err = db.DB().QueryRow(`
			SELECT COALESCE(SUM(messages_updated), 0) FROM sync_runs
			WHERE status = 'completed'
			  AND completed_at IS NOT NULL
			  AND id > ?
		`, state.LastCompletedSyncRunID).Scan(&updatedSinceBuild)
		if err != nil {
			return cacheStaleness{
				NeedsBuild: true, FullRebuild: true,
				Reason: "cannot verify sync history",
			}
		}
		if updatedSinceBuild > 0 {
			result.HasUpdated = true
			result.FullRebuild = true
			reasons = append(reasons,
				fmt.Sprintf("%d updated messages", updatedSinceBuild))
		}
	}

	// Check if parquet files actually exist (directory might be empty)
	files, _ := filepath.Glob(
		filepath.Join(messagesDir, "*", "*.parquet"))
	if len(files) == 0 {
		result.FullRebuild = true
		reasons = append(reasons, "cache directory empty")
	}

	if missingRequiredParquet(analyticsDir) {
		result.FullRebuild = true
		reasons = append(reasons, "cache missing required tables")
	}

	if len(reasons) > 0 {
		result.NeedsBuild = true
		result.Reason = strings.Join(reasons, "; ")
	}

	return result
}

func init() {
	rootCmd.AddCommand(tuiCmd)
	tuiCmd.Flags().BoolVar(&forceSQL, "force-sql", false, "Force SQLite queries instead of Parquet (slow for large archives)")
	tuiCmd.Flags().BoolVar(&skipCacheBuild, "no-cache-build", false, "Skip automatic cache build/update")
	tuiCmd.Flags().BoolVar(&noSQLiteScanner, "no-sqlite-scanner", false, "Disable DuckDB sqlite_scanner extension (use direct SQLite fallback)")
	tuiCmd.Flags().BoolVar(&forceLocalTUI, "local", false, "Force local database (override remote config)")
	_ = tuiCmd.Flags().MarkHidden("no-sqlite-scanner")
}
