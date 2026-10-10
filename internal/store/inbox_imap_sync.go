package store

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type imapInboxSyncObservation struct {
	Membership IMAPMembershipObservation
	MessageID  int64
	Revision   string
}

// Observe only exact memberships fetched during this sync, including unchanged
// reset entries. A flag-only delta updates provider state without touching UI read.
func observeIMAPSyncMembershipsTx(ctx context.Context, tx *loggedTx, sourceID int64, observations []imapInboxSyncObservation) error {
	var identifier, sourceType string
	if err := tx.QueryRowContext(ctx, `SELECT identifier, source_type FROM sources WHERE id = ?`, sourceID).Scan(&identifier, &sourceType); err != nil {
		return fmt.Errorf("resolve IMAP observation source: %w", err)
	}
	parsed, err := url.Parse(identifier)
	// Legacy identifiers cannot supply an authenticated provider account binding.
	// Preserve their existing sync behavior; inbox control remains unavailable.
	if err != nil || sourceType != sourceTypeIMAP || parsed.User == nil || parsed.User.Username() == "" || (parsed.Scheme != sourceTypeIMAP && parsed.Scheme != "imaps" && parsed.Scheme != "imap+starttls") {
		return nil //nolint:nilerr // Legacy sync continues without an authenticated inbox binding.
	}
	observedAt := time.Now().UTC()
	for _, entry := range observations {
		membership := entry.Membership
		var providerID string
		if err := tx.QueryRowContext(ctx, `SELECT source_message_id FROM messages WHERE id = ? AND source_id = ?`, entry.MessageID, sourceID).Scan(&providerID); err != nil {
			return fmt.Errorf("resolve IMAP observation item: %w", err)
		}
		target := inboxcontrol.Target{SourceID: sourceID, SourceType: sourceType, SourceIdentifier: identifier, AccountID: parsed.User.Username(), Scope: inboxcontrol.ScopeMessage, ItemID: entry.MessageID, ProviderID: providerID, Mailbox: membership.Mailbox, UIDValidity: membership.UIDValidity, UID: membership.UID}
		inbox := strings.EqualFold(membership.Mailbox, "INBOX")
		state := inboxcontrol.State{Target: target, Inbox: &inbox, Location: membership.Mailbox, Revision: entry.Revision, ObservedAt: observedAt}
		if membership.Flags != nil {
			read := false
			state.Flags = slices.Clone(membership.Flags)
			for _, flag := range membership.Flags {
				if strings.EqualFold(flag, `\Seen`) {
					read = true
				}
				if !strings.HasPrefix(flag, `\`) {
					state.Tags = append(state.Tags, flag)
				}
			}
			state.Read = &read
		}
		if _, err := observeInboxStateTx(ctx, tx, state); err != nil {
			return fmt.Errorf("record IMAP sync observation: %w", err)
		}
	}
	// Retire vanished memberships and obsolete canonical provider identities.
	result, err := tx.ExecContext(ctx, `DELETE FROM inbox_provider_states WHERE source_id = ? AND mailbox <> ''
  AND (NOT EXISTS (SELECT 1 FROM messages msg
   WHERE msg.id = inbox_provider_states.item_id AND msg.source_id = inbox_provider_states.source_id
    AND msg.source_message_id = inbox_provider_states.provider_id)
  OR NOT EXISTS (SELECT 1 FROM imap_message_memberships m
   WHERE m.source_id = inbox_provider_states.source_id AND m.message_id = inbox_provider_states.item_id
    AND m.mailbox = inbox_provider_states.mailbox AND m.uidvalidity = inbox_provider_states.uidvalidity AND m.uid = inbox_provider_states.uid))`, sourceID)
	if err != nil {
		return fmt.Errorf("retire IMAP sync observations: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count retired IMAP observations: %w", err)
	}
	if rows > 0 {
		return bumpInboxSourceRevisionTx(ctx, tx, sourceID)
	}
	return nil
}
