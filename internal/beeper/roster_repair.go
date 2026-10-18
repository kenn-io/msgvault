package beeper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// Optional repairs follow message work, at most sixteen per run. Failed rooms
// back off for a day so they cannot starve later rooms or pin discovery.
const maxRosterRepairsPerRun = 16

func (imp *Importer) repairQuietRosters(ctx context.Context, syncID, sourceID int64, opts ImportOptions, state *SyncState, chats []chatVisit) (bool, error) {
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
		if err := imp.store.InvalidateSourceConversationMessagingRouteEvidenceWithFailureContext(ctx, sourceID, chatID, "membership_fetch_failed"); err != nil {
			return false, fmt.Errorf("invalidate route before roster repair: %w", err)
		}
		requestCtx, cancelRequest := opts.requestContext(ctx)
		probeCtx, cancelProbe := context.WithTimeout(requestCtx, routeProbeTimeout)
		detail, fetchErr := imp.client.GetChat(probeCtx, chatID)
		var membership chatMembership
		var conversationID int64
		if fetchErr == nil {
			if detail.ID != chatID || detail.AccountID != opts.AccountID {
				fetchErr = errors.New("roster chat does not match the requested account and conversation")
			} else {
				// Metadata-only failures are recorded separately from missing
				// messages. Reuse the normal roster writer and its exact count.
				if err := imp.store.SetSourceConversationRosterRepairPendingContext(ctx, sourceID, chatID, true); err != nil {
					cancelProbe()
					cancelRequest()
					return false, fmt.Errorf("mark roster repair pending: %w", err)
				}
				conversationID, _, membership, err = imp.ensureConversation(probeCtx, syncID, sourceID, detail, opts, &ImportSummary{})
				if err != nil && probeCtx.Err() == nil {
					cancelProbe()
					cancelRequest()
					return false, fmt.Errorf("repair archived roster: %w", err)
				}
				fetchErr = err
			}
		}
		timedOut := errors.Is(probeCtx.Err(), context.DeadlineExceeded)
		cancelProbe()
		cancelRequest()
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if fetchErr == nil {
			// Quiet rooms skip syncChat's deferred stats refresh, so update the
			// denormalized conversation counts after their roster is repaired.
			statsCtx, cancelStats := opts.conversationStatsContext(ctx)
			statsErr := imp.store.RecomputeConversationStatsForConversationContext(statsCtx, conversationID)
			cancelStats()
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if statsErr != nil {
				statsErr = opts.budgetError(ctx, statsErr)
				if errors.Is(statsErr, errBeeperBudgetExpired) {
					cs.RosterRetryAfter = formatWatermark(time.Now().UTC().Add(24 * time.Hour))
					imp.recordItem(syncID, chatID, "membership", store.SyncRunItemStatusSkipped, "beeper_roster_repair_deferred", statsErr)
					return true, nil
				}
				return false, fmt.Errorf("recompute conversation stats after roster repair: %w", statsErr)
			}
			pendingCtx := ctx
			var cancelPending context.CancelFunc
			if opts.stopRequested() {
				pendingCtx, cancelPending = opts.finalizeContext(ctx)
			}
			if cancelPending != nil {
				defer cancelPending()
			}
			if err := imp.store.SetSourceConversationRosterRepairPendingContext(pendingCtx, sourceID, chatID, false); err != nil {
				return false, fmt.Errorf("clear roster repair pending marker: %w", err)
			}
			if err := imp.refreshConversationPolicyAndRoster(pendingCtx, conversationID, sourceID, membership.roster, !membership.roster.Participants.HasMore, opts, &ImportSummary{}); err != nil {
				return false, fmt.Errorf("refresh repaired roster policy: %w", err)
			}
		}
		if fetchErr == nil && membership.known {
			cs.RosterRetryAfter = ""
			if opts.stopRequested() {
				return true, nil
			}
			continue
		}
		cs.RosterRetryAfter = formatWatermark(now.Add(24 * time.Hour))
		if errors.Is(fetchErr, ErrNotFound) {
			cs.Gone = true
		}
		imp.recordItem(syncID, chatID, "membership", store.SyncRunItemStatusSkipped, "beeper_roster_repair_deferred", fetchErr)
		if opts.budgetExpired() || timedOut {
			return true, nil // Spend at most one timeout on optional work.
		}
		if opts.stopRequested() {
			return true, nil
		}
	}
	return false, nil
}
