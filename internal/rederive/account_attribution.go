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

// Heal derives the account attribution of a source's pending email and
// calendar rows before a sync or import adds more mail. Rows archived by older
// versions start pending, and an identity change returns the rows it affects
// to pending. A source with nothing pending costs one indexed lookup, so every
// sync runs it. A failure is logged instead of returned: committed pages stay
// derived and the next sync resumes the rest.
func Heal(ctx context.Context, logger *slog.Logger, s *store.Store, src *store.Source) {
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
