package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

// addManualSyncCacheFlags is called by each archive-writing sync command.
// Cobra forwards changed local flags through the generic daemon CLI proxy.
func addManualSyncCacheFlags(cmd *cobra.Command) *cobra.Command {
	cmd.Flags().Bool("build-cache", false, "refresh analytics cache after this sync")
	cmd.Flags().Bool("no-build-cache", false, "skip analytics cache refresh after this sync")
	cmd.MarkFlagsMutuallyExclusive("build-cache", "no-build-cache")
	return cmd
}

func manualSyncCacheFlags(cmd *cobra.Command) (force, skip bool, err error) {
	if cmd == nil || cmd.Flags().Lookup("build-cache") == nil {
		return false, false, nil
	}
	force, err = cmd.Flags().GetBool("build-cache")
	if err != nil {
		return false, false, fmt.Errorf("read --build-cache: %w", err)
	}
	skip, err = cmd.Flags().GetBool("no-build-cache")
	if err != nil {
		return false, false, fmt.Errorf("read --no-build-cache: %w", err)
	}
	if force && skip {
		return false, false, errors.New("--build-cache and --no-build-cache are mutually exclusive")
	}
	return force, skip, nil
}

func manualSyncCLICommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "sync-slack", "sync-teams", "sync-beeper", "sync-discord", "sync-circleback",
		"sync-notion-meetings", "sync-granola", "sync-calendar", "sync-synctech-sms":
		if args[0] == "sync-circleback" || args[0] == "sync-notion-meetings" {
			for _, arg := range args[1:] {
				if arg == "--probe" || arg == "--probe=true" {
					return false
				}
			}
		}
		return true
	default:
		return false
	}
}

func manualSyncCacheFlagValues(args []string) (force, skip bool) {
	for _, arg := range args {
		switch arg {
		case "--build-cache", "--build-cache=true":
			force = true
		case "--no-build-cache", "--no-build-cache=true":
			skip = true
		case "--build-cache=false":
			force = false
		case "--no-build-cache=false":
			skip = false
		}
	}
	return force, skip
}

// queueCacheRefreshAfterManualSync runs in the daemon, after its CLI child
// returns. It may inspect cache metadata, but it never waits for a builder.
func (a *storeAPIAdapter) queueCacheRefreshAfterManualSync(force, skip bool) error {
	if a.cacheJobs == nil || skip || (!force && !cfg.Analytics.AutoBuildCache) {
		return nil
	}
	dbPath := cfg.DatabaseDSN()
	if store.IsPostgresURL(dbPath) {
		return nil
	}
	staleness, err := cacheNeedsBuildForQuery(context.Background(), dbPath, cfg.AnalyticsDir())
	if err != nil {
		return fmt.Errorf("inspect analytics cache after sync: %w", err)
	}
	if !staleness.NeedsBuild {
		return nil
	}
	mode := buildCacheModeAuto
	if !force && staleness.HasUsablePublication {
		if remaining, deferBuild := scheduledCacheBuildDelay(staleness, cfg.Analytics.MinRebuildInterval, time.Now()); deferBuild {
			logger.Info("skipping cache rebuild after manual sync: minimum interval not elapsed",
				"remaining", remaining.String(), "published_at", staleness.PublishedAt)
			return nil
		}
		mode = buildCacheModeScheduledAuto
	}
	job, err := a.cacheJobs.acceptAfterWrite(mode)
	if err != nil {
		return fmt.Errorf("queue analytics cache build after sync: %w", err)
	}
	logger.Info("queued analytics cache build after manual sync", "job_id", job.JobID)
	return nil
}
