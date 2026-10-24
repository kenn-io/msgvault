package rederive

import (
	"context"
	"log/slog"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// healProgressInterval spaces the progress logs of a long account-attribution
// pass, so a first sync after upgrading does not look stalled.
const healProgressInterval = 10 * time.Second

// Heal rebuilds stale derived text, then attributes pending email and calendar
// rows to their account before a sync or import adds more mail. Older rows
// start pending, and an identity change returns the rows it affects
// to pending. A source with nothing pending costs one indexed lookup, so every
// sync runs it. A failure is logged instead of returned: committed pages stay
// derived, and the next sync retries unfinished work.
func Heal(ctx context.Context, logger *slog.Logger, s *store.Store, src *store.Source) {
	lastTextReport := time.Now()
	textProgress := func(message string) {
		if time.Since(lastTextReport) < healProgressInterval {
			return
		}
		lastTextReport = time.Now()
		logger.Info("rebuilding archived message text", "source_id", src.ID, "progress", message)
	}
	textSum, ran, textErr := RunIfStale(ctx, s, src.SourceType, src.Identifier, src.ID, textProgress)
	if textErr != nil || (textSum != nil && textSum.Errors > 0) {
		if ctx.Err() == nil {
			logger.Warn("rebuild archived message text failed; retrying on the next sync", "source_id", src.ID, "summary", textSum, "error", textErr)
		}
	} else if ran && textSum != nil && textSum.MessagesScanned > 0 {
		logger.Info("checked archived message text", "source_id", src.ID, "messages", textSum.MessagesScanned,
			"bodies", textSum.BodiesRewritten, "undecodable", textSum.Undecodable, "duration", textSum.Duration)
	}

	start := time.Now()
	lastReport := start
	progress := func(sum store.AccountAttributionRepairSummary) {
		if time.Since(lastReport) < healProgressInterval {
			return
		}
		lastReport = time.Now()
		logger.Info("deriving account attribution of archived messages",
			"source_id", src.ID, "messages", sum.Scanned)
	}
	sum, err := s.RepairAccountAttributionContext(ctx, src.ID, progress)
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("derive account attribution of archived messages failed; retrying on the next sync",
				"source_id", src.ID, "messages", sum.Scanned, "error", err)
		}
		return
	}
	if sum.Scanned > 0 {
		logger.Info("derived account attribution of archived messages",
			"source_id", src.ID, "messages", sum.Scanned, "undecodable", sum.Undecodable,
			"duration", time.Since(start).Round(time.Millisecond))
	}
}
