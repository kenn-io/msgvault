package beeper

// The reinstall guard. Beeper message IDs are plain per-installation rowids:
// a reinstall or re-index keeps the (network-derived) chat IDs but re-assigns
// every message ID, which would silently corrupt cursor-based dedup. Anchor
// probes — recent messages remembered with their timestamps across several
// distinct chats — fingerprint the installation and are verified before any
// stored cursor is trusted.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// maxAnchors is how many distinct chats fingerprint the installation.
// Multiple probes keep ordinary churn (the user deleting one anchored chat)
// distinguishable from a reinstall, which invalidates all of them.
const maxAnchors = 3

// archivedSampleTolerance allows for timestamp precision differences between
// the API and the archive's stored sent_at (driver/backend rounding). A
// re-assigned message ID would land at an unrelated timestamp.
const archivedSampleTolerance = 2 * time.Second

// verifyAnchors re-fetches the anchor probe messages. A changed timestamp on
// any probe means the installation re-assigned message IDs: fail fast,
// because continuing would silently duplicate the whole archive. Missing
// probes are ordinary churn (deleted messages or chats) as long as one probe
// survives; when none does — or none was armed — recently archived messages
// arbitrate before any stored cursor is trusted.
func (imp *Importer) verifyAnchors(ctx context.Context, syncID, sourceID int64, state *SyncState) error {
	kept := make([]AnchorProbe, 0, len(state.Anchors))
	for _, a := range state.Anchors {
		m, err := imp.client.GetMessage(ctx, a.ChatID, a.MessageID)
		switch {
		case err == nil:
			if !m.Timestamp.Equal(a.Timestamp) {
				return fmt.Errorf("beeper message IDs appear re-assigned (Beeper Desktop reinstall or re-index?): anchor message %s changed timestamp; remove and re-add this account to avoid duplicate archives", a.MessageID)
			}
			kept = append(kept, a)
		case errors.Is(err, ErrNotFound):
			imp.recordItem(syncID, a.MessageID, "anchor", store.SyncRunItemStatusSkipped, "beeper_anchor_lost", err)
		default:
			// Transient probe failure: neither evidence of a reinstall nor of
			// ordinary churn — retry next run instead of alarming the user.
			return fmt.Errorf("verify beeper sync anchor: %w", err)
		}
	}
	state.Anchors = kept
	if len(kept) == 0 {
		return imp.verifyArchivedSample(ctx, sourceID)
	}
	return nil
}

// verifyArchivedSample checks whether recently archived messages still
// resolve to the same content at the source. It is the tie-breaker when no
// anchor probe survives (and the fallback when none was armed): a match means
// the losses were ordinary churn; different content or nothing resolving
// means the installation was rebuilt and stored cursors must not be trusted.
// A no-op for accounts with nothing archived yet.
func (imp *Importer) verifyArchivedSample(ctx context.Context, sourceID int64) error {
	refs, err := imp.store.ListRecentMessagesForSource(sourceID, 5)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil // nothing archived yet, nothing to corrupt
	}
	for _, ref := range refs {
		m, err := imp.client.GetMessage(ctx, ref.ChatID, ref.SourceMessageID)
		switch {
		case err == nil:
			diff := m.Timestamp.Sub(ref.SentAt)
			if diff < 0 {
				diff = -diff
			}
			if diff <= archivedSampleTolerance {
				return nil // the archive still matches the source
			}
			return fmt.Errorf("beeper message IDs appear re-assigned (Beeper Desktop reinstall or re-index?): archived message %s resolves to different content; remove and re-add this account to avoid duplicate archives", ref.SourceMessageID)
		case errors.Is(err, ErrNotFound):
			continue
		default:
			return fmt.Errorf("verify beeper sync anchor: %w", err)
		}
	}
	return fmt.Errorf("beeper message IDs appear re-assigned (Beeper Desktop reinstall or re-index?): no anchor and none of the %d most recently archived messages resolve at the source; remove and re-add this account to avoid duplicate archives", len(refs))
}

// rearmAnchors tops the anchor set back up to maxAnchors, probing the heads
// of the most recently active chats first and falling back to chats known
// from prior runs when this run enumerated none (quiet account) — the guard
// must not stay weakened just because nothing happened lately. Best-effort; a
// failure leaves the remaining slots for the next run to fill.
func (imp *Importer) rearmAnchors(ctx context.Context, chats []Chat, state *SyncState) {
	if len(state.Anchors) >= maxAnchors {
		return
	}
	anchored := make(map[string]bool, len(state.Anchors))
	for _, a := range state.Anchors {
		anchored[a.ChatID] = true
	}
	const maxProbes = 8
	candidates := make([]string, 0, maxProbes)
	for i := range chats {
		if len(candidates) >= maxProbes {
			break
		}
		if !anchored[chats[i].ID] {
			candidates = append(candidates, chats[i].ID)
		}
	}
	for chatID := range state.Chats {
		if len(candidates) >= maxProbes {
			break
		}
		if !anchored[chatID] {
			candidates = append(candidates, chatID)
		}
	}
	for _, chatID := range candidates {
		if len(state.Anchors) >= maxAnchors {
			return
		}
		page, err := imp.client.ListMessagesPage(ctx, chatID, "", "")
		if err != nil {
			return
		}
		armAnchorFromPage(state, chatID, page.Items)
	}
}

// armAnchorFromPage adds an anchor probe from a page of messages when the
// anchor set is below target and the chat is not already anchored.
func armAnchorFromPage(state *SyncState, chatID string, items []Message) {
	if len(state.Anchors) >= maxAnchors {
		return
	}
	for _, a := range state.Anchors {
		if a.ChatID == chatID {
			return
		}
	}
	if a := anchorFrom(chatID, items); a != nil {
		state.Anchors = append(state.Anchors, *a)
	}
}

// anchorFrom picks a stable anchor message from a page: a regular content
// message (not a reaction, tombstone, or hidden event), preferring the newest.
func anchorFrom(chatID string, items []Message) *AnchorProbe {
	var best *Message
	for i := range items {
		m := &items[i]
		if m.Type == "REACTION" || m.IsDeleted || m.IsHidden {
			continue
		}
		if best == nil || m.Timestamp.After(best.Timestamp) {
			best = m
		}
	}
	if best == nil {
		return nil
	}
	return &AnchorProbe{ChatID: chatID, MessageID: best.ID, Timestamp: best.Timestamp}
}
