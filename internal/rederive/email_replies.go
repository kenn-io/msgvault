package rederive

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// emailRepliesVersion names the reply-link ledger state. Bump it to backfill
// In-Reply-To and relink every source again.
const emailRepliesVersion = "email-replies-v1"

// One ledger key per source extends the versioned name with its phase.
// Fixed-width hex IDs keep the keys colon free, so a source merge clears them
// and the next call backfills the merged messages. The phases sort in the
// order they run, so a stray duplicate key resolves to the earlier state.
//
//	#h<after>            header backfill resumes after the ID
//	#r<after>-<through>  full linking resumes after the ID, then marks through
//	#w<through>          messages through the ID are linked
const (
	emailRepliesHeaders = "#h"
	emailRepliesResolve = "#r"
	emailRepliesLinked  = "#w"
)

// LinkEmailReplies keeps a source's email reply links current. The first call
// for a source records In-Reply-To from the archived MIME headers of messages
// that lack it and links the whole source; later calls link only around the
// messages archived since the previous call. Sync and import call it before
// and after adding mail. A failure is logged instead of returned: committed
// pages stay linked, and the next call resumes from the last checkpoint.
func LinkEmailReplies(ctx context.Context, logger *slog.Logger, s *store.Store, src *store.Source) {
	if src == nil {
		return
	}
	start := time.Now()
	lastReport := start
	var scanned, recorded int
	progress := func(page store.EmailReplyBackfillPage) {
		scanned += page.Scanned
		recorded += page.Recorded
		if time.Since(lastReport) < healProgressInterval {
			return
		}
		lastReport = time.Now()
		logger.Info("reading reply headers of archived email",
			"source_id", src.ID, "messages", scanned, "replies", recorded)
	}
	backfilled, err := linkEmailReplies(ctx, s, src, progress)
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("link email replies failed; retrying on the next sync",
				"source_id", src.ID, "error", err)
		}
		return
	}
	if backfilled && scanned > 0 {
		logger.Info("linked archived email replies", "source_id", src.ID,
			"messages", scanned, "replies", recorded,
			"duration", time.Since(start).Round(time.Millisecond))
	}
}

// linkEmailReplies advances the source's ledger state and reports whether
// this call finished a backfill.
func linkEmailReplies(
	ctx context.Context, s *store.Store, src *store.Source, progress func(store.EmailReplyBackfillPage),
) (bool, error) {
	base := LedgerKey(src.SourceType, src.Identifier, emailRepliesVersion)
	key, err := s.NextMigrationKeyContext(ctx, base+"#", "")
	if err != nil {
		return false, err
	}
	phase, after, through, ok := parseEmailRepliesState(strings.TrimPrefix(key, base))
	if key != "" && !ok {
		if err := s.ForgetMigrationsContext(ctx, base); err != nil {
			return false, err
		}
		return false, fmt.Errorf("malformed email reply state %q", key)
	}
	advance := func(next string) error {
		next = base + next
		if err := s.ReplaceMigrationContext(ctx, key, next); err != nil {
			return err
		}
		key = next
		return nil
	}

	if phase == emailRepliesLinked {
		latest, err := s.LatestSourceMessageIDContext(ctx, src.ID)
		if err != nil || latest <= after {
			return false, err
		}
		if err := s.LinkEmailRepliesSinceContext(ctx, src.ID, after, latest); err != nil {
			return false, err
		}
		return false, advance(fmt.Sprintf("%s%016x", emailRepliesLinked, latest))
	}

	if phase != emailRepliesResolve {
		for {
			page, err := s.BackfillEmailReplyHeadersContext(ctx, src.ID, after)
			if err != nil {
				return false, err
			}
			if page.LastID == 0 {
				break
			}
			after = page.LastID
			progress(page)
			if err := advance(fmt.Sprintf("%s%016x", emailRepliesHeaders, after)); err != nil {
				return false, err
			}
		}
		// Messages archived after this point are linked by the next call.
		if through, err = s.LatestSourceMessageIDContext(ctx, src.ID); err != nil {
			return false, err
		}
		after = 0
		if err := advance(fmt.Sprintf("%s%016x-%016x", emailRepliesResolve, after, through)); err != nil {
			return false, err
		}
	}
	err = s.ResolveEmailReplyParentsContext(ctx, src.ID, after, func(next int64) error {
		return advance(fmt.Sprintf("%s%016x-%016x", emailRepliesResolve, next, through))
	})
	if err != nil {
		return false, err
	}
	return true, advance(fmt.Sprintf("%s%016x", emailRepliesLinked, through))
}

// parseEmailRepliesState reads a ledger suffix. An empty suffix is the
// initial state; ok is false for anything this version did not write.
func parseEmailRepliesState(suffix string) (phase string, after, through int64, ok bool) {
	if suffix == "" {
		return "", 0, 0, true
	}
	if len(suffix) < 2 {
		return "", 0, 0, false
	}
	phase, rest := suffix[:2], suffix[2:]
	switch phase {
	case emailRepliesHeaders, emailRepliesLinked:
		id, ok := parseEmailRepliesID(rest)
		return phase, id, id, ok
	case emailRepliesResolve:
		first, last, found := strings.Cut(rest, "-")
		after, afterOK := parseEmailRepliesID(first)
		through, throughOK := parseEmailRepliesID(last)
		return phase, after, through, found && afterOK && throughOK
	}
	return "", 0, 0, false
}

// parseEmailRepliesID accepts only the fixed-width hex IDs the state is
// written with. Zero is valid: a source may hold no messages yet.
func parseEmailRepliesID(hex string) (int64, bool) {
	if hex == "0000000000000000" {
		return 0, true
	}
	return parseEmailRetryID(hex)
}
