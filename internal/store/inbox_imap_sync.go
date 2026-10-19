package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
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
}

// Observe all exact copies fetched by this sync, including unchanged reset
// entries. Omitted FLAGS stays unknown even if membership keeps last-known flags.
// These sync requests do not fetch per-message MODSEQ; mailbox HIGHESTMODSEQ
// must never be represented as the exact message's revision or CAS evidence.
func observeIMAPSyncMembershipsTx(ctx context.Context, tx *loggedTx, sourceID int64, observations []imapInboxSyncObservation) error {
	var identifier, kind string
	var config sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT identifier,source_type,sync_config FROM sources WHERE id=?`, sourceID).Scan(&identifier, &kind, &config); err != nil {
		return fmt.Errorf("resolve IMAP Inbox sync source: %w", err)
	}
	accountID, ok := parseIMAPInboxSyncAccount(identifier, kind, config)
	if !ok {
		return nil
	}
	observedAt := time.Now().UTC()
	for _, entry := range observations {
		membership := entry.Membership
		var providerID string
		if err := tx.QueryRowContext(ctx, `SELECT source_message_id FROM messages WHERE id=? AND source_id=?`, entry.MessageID, sourceID).Scan(&providerID); err != nil {
			return fmt.Errorf("resolve IMAP Inbox sync item: %w", err)
		}
		target := inboxcontrol.Target{SourceID: sourceID, SourceType: kind, SourceIdentifier: identifier, AccountID: accountID, Scope: inboxcontrol.ScopeMessage, ItemID: entry.MessageID, ProviderID: providerID, Mailbox: membership.Mailbox, UIDValidity: membership.UIDValidity, UID: membership.UID}
		inbox := strings.EqualFold(membership.Mailbox, "INBOX")
		state := inboxcontrol.State{Target: target, Inbox: &inbox, Location: membership.Mailbox, Revision: fmt.Sprintf("%d:0", membership.UIDValidity), ObservedAt: observedAt}
		if membership.Flags != nil {
			state.Flags = slices.Clone(membership.Flags)
			state.Tags = []string{}
			read := false
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
			return fmt.Errorf("record IMAP Inbox sync observation: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM inbox_provider_states WHERE source_id=? AND mailbox<>'' AND
  (NOT EXISTS(SELECT 1 FROM messages msg WHERE msg.id=inbox_provider_states.item_id AND msg.source_id=inbox_provider_states.source_id AND msg.source_message_id=inbox_provider_states.provider_id)
  OR NOT EXISTS(SELECT 1 FROM imap_message_memberships mm WHERE mm.source_id=inbox_provider_states.source_id AND mm.message_id=inbox_provider_states.item_id AND mm.mailbox=inbox_provider_states.mailbox AND mm.uidvalidity=inbox_provider_states.uidvalidity AND mm.uid=inbox_provider_states.uid))`, sourceID)
	if err != nil {
		return fmt.Errorf("retire IMAP Inbox sync observations: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count retired IMAP Inbox observations: %w", err)
	}
	if rows > 0 {
		return bumpInboxSourceRevisionTx(ctx, tx, sourceID)
	}
	return nil
}

func parseIMAPInboxSyncAccount(identifier, kind string, config sql.NullString) (string, bool) {
	if kind != sourceTypeIMAP || !config.Valid {
		return "", false
	}
	parsed, err := url.Parse(identifier)
	if err != nil || parsed.User == nil || parsed.User.Username() == "" || (parsed.Scheme != "imap" && parsed.Scheme != "imaps" && parsed.Scheme != "imap+starttls") {
		return "", false
	}
	var account struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal([]byte(config.String), &account); err != nil || account.Username != parsed.User.Username() {
		return "", false
	}
	return account.Username, true
}
