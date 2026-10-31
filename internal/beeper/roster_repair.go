package beeper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// Optional repairs follow message work, at most sixteen per run. Failed rooms
// back off for a day so they cannot starve later rooms, pin discovery, or fail
// a sync whose message work already completed.
const maxRosterRepairsPerRun = 16

// rosterRepairBackoff is how long a failed quiet-room repair waits before the
// next attempt. A full sync or new activity in the room refreshes it sooner.
const rosterRepairBackoff = 24 * time.Hour

func (imp *Importer) repairQuietRosters(ctx context.Context, syncID, sourceID int64, opts ImportOptions, state *SyncState, chats []chatVisit, sum *ImportSummary) (bool, error) {
	ids, err := imp.store.ListUnresolvedConversationIDs(ctx, sourceID)
	if err != nil {
		return false, err
	}
	seen := make(map[string]bool, len(chats))
	for _, chat := range chats {
		seen[chat.ID] = true // Normal sync already attempted these rosters.
	}
	now := time.Now().UTC()
	attempts := 0
	for _, chatID := range ids {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if opts.stopRequested() {
			return true, nil // Completed message work remains complete.
		}
		if attempts == maxRosterRepairsPerRun {
			return false, nil
		}
		cs := state.Chats[chatID]
		if seen[chatID] || cs == nil || !cs.Done || cs.Gone || parseWatermark(cs.RosterRetryAfter).After(now) {
			continue
		}
		attempts++
		stop, err := imp.repairQuietRoster(ctx, syncID, sourceID, chatID, opts, cs, now, sum)
		if err != nil || stop {
			return stop, err
		}
	}
	return false, nil
}

// repairQuietRoster refreshes one quiet room's membership. It returns stop when
// optional work should end for this run, and an error only when the import
// context is canceled. Every other failure defers the room.
func (imp *Importer) repairQuietRoster(ctx context.Context, syncID, sourceID int64, chatID string, opts ImportOptions, cs *ChatState, now time.Time, sum *ImportSummary) (bool, error) {
	deferRoom := func(reason string, status string, cause error) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		cs.RosterRetryAfter = formatWatermark(now.Add(rosterRepairBackoff))
		if status == store.SyncRunItemStatusError {
			sum.RosterRepairErrors++
			slog.Warn("quiet Beeper roster repair failed", "chat", chatID, "error", cause)
		}
		imp.recordItem(syncID, chatID, "membership", status, reason, cause)
		return opts.stopRequested(), nil
	}
	failed := func(cause error) (bool, error) {
		if errors.Is(opts.budgetError(ctx, cause), errBeeperBudgetExpired) {
			return deferRoom("beeper_roster_repair_deferred", store.SyncRunItemStatusSkipped, cause)
		}
		return deferRoom("beeper_roster_repair_failed", store.SyncRunItemStatusError, cause)
	}

	// Like chat discovery's probe, clear the route's membership proof before the
	// request and record a fetch failure only if the request actually fails.
	if err := imp.store.InvalidateSourceConversationMessagingRouteEvidenceContext(ctx, sourceID, chatID); err != nil {
		return failed(fmt.Errorf("invalidate route before roster repair: %w", err))
	}
	// The probe timeout bounds only the provider request. Roster writes for a
	// large room can outlast it, so they share the run's budget instead.
	requestCtx, cancelRequest := opts.requestContext(ctx)
	defer cancelRequest()
	probeCtx, cancelProbe := context.WithTimeout(requestCtx, routeProbeTimeout)
	detail, fetchErr := imp.client.GetChat(probeCtx, chatID)
	timedOut := errors.Is(probeCtx.Err(), context.DeadlineExceeded)
	cancelProbe()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if errors.Is(fetchErr, ErrNotFound) {
		// A chat Beeper no longer has keeps its invalidated route proof without a
		// fetch-failure marker, and later runs stop probing it.
		cs.Gone = true
		return deferRoom("beeper_chat_gone", store.SyncRunItemStatusSkipped, fetchErr)
	}
	if fetchErr == nil && (detail.ID != chatID || detail.AccountID != opts.AccountID) {
		fetchErr = errors.New("roster chat does not match the requested account and conversation")
	}
	if fetchErr != nil {
		if err := imp.store.InvalidateSourceConversationMessagingRouteEvidenceWithFailureContext(ctx, sourceID, chatID, "membership_fetch_failed"); err != nil {
			return failed(fmt.Errorf("record roster fetch failure: %w", err))
		}
		stop, err := deferRoom("beeper_roster_repair_deferred", store.SyncRunItemStatusSkipped, fetchErr)
		// Spend at most one timeout on optional work.
		return stop || timedOut || opts.budgetExpired(), err
	}

	// Metadata-only failures are recorded separately from missing messages.
	// Reuse the normal roster writer and its exact count. The pending marker
	// keeps the room unresolved until every step below has finished.
	if err := imp.store.SetSourceConversationRosterRepairPendingContext(ctx, sourceID, chatID, true); err != nil {
		return failed(fmt.Errorf("mark roster repair pending: %w", err))
	}
	conversationID, _, membership, err := imp.ensureConversation(requestCtx, syncID, sourceID, detail, opts, &ImportSummary{})
	if err != nil {
		return failed(fmt.Errorf("repair archived roster: %w", err))
	}
	// Quiet rooms skip syncChat's deferred stats refresh, so update the
	// denormalized conversation counts after their roster is repaired.
	statsCtx, cancelStats := opts.conversationStatsContext(ctx)
	err = imp.store.RecomputeConversationStatsForConversationContext(statsCtx, conversationID)
	cancelStats()
	if err != nil {
		return failed(fmt.Errorf("recompute conversation stats after roster repair: %w", err))
	}
	pendingCtx := ctx
	if opts.stopRequested() {
		var cancelPending context.CancelFunc
		pendingCtx, cancelPending = opts.finalizeContext(ctx)
		defer cancelPending()
	}
	if err := imp.store.SetSourceConversationRosterRepairPendingContext(pendingCtx, sourceID, chatID, false); err != nil {
		return failed(fmt.Errorf("clear roster repair pending marker: %w", err))
	}
	if !membership.known {
		stop, err := deferRoom("beeper_roster_repair_deferred", store.SyncRunItemStatusSkipped, nil)
		return stop || opts.budgetExpired(), err
	}
	cs.RosterRetryAfter = ""
	return opts.stopRequested(), nil
}
