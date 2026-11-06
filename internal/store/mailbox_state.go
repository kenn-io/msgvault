package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ProviderMailboxState is the provider's last observed state, never local is_read.
type ProviderMailboxState struct {
	InboxState string `json:"inbox_state" enum:"inbox,outside,unknown"`
	ReadState  string `json:"read_state" enum:"unread,read,unknown"`
}

// MailboxStateSelectSQL projects observations through their current archive
// identity. The SQL works on SQLite, PostgreSQL and DuckDB; prefix is an internal
// schema qualifier, such as "sqlite_db.". IMAP must still have the exact copy.
func MailboxStateSelectSQL(prefix string) string {
	return fmt.Sprintf(`
 SELECT m.id AS message_id,
 CASE WHEN o.is_inbox THEN 'inbox' WHEN NOT o.is_inbox THEN 'outside' ELSE 'unknown' END AS inbox_state,
 CASE WHEN o.provider_read THEN 'read' WHEN NOT o.provider_read THEN 'unread' ELSE 'unknown' END AS read_state
 FROM %[1]smessages m
 JOIN %[1]ssources s ON s.id=m.source_id
 JOIN %[1]sprovider_mailbox_observations o ON o.message_id=m.id AND o.source_id=m.source_id AND o.provider_id=m.source_message_id AND o.mailbox=''
 WHERE COALESCE(s.source_type,'') IN ('','gmail','msmail') AND COALESCE(m.message_type,'') IN ('','email') AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
 UNION ALL
 SELECT m.id AS message_id,
 CASE WHEN MAX(CASE WHEN o.is_inbox THEN 1 ELSE 0 END)=1 THEN 'inbox'
 WHEN COUNT(o.message_id)>0 AND MAX(CASE WHEN LOWER(mm.mailbox)='inbox' THEN 1 ELSE 0 END)=0 THEN 'outside'
 ELSE 'unknown' END AS inbox_state,
 CASE WHEN MAX(CASE WHEN o.is_inbox AND NOT o.provider_read THEN 1 ELSE 0 END)=1 THEN 'unread'
 WHEN COUNT(CASE WHEN LOWER(mm.mailbox)='inbox' THEN 1 END)>0
 AND COUNT(CASE WHEN LOWER(mm.mailbox)='inbox' THEN 1 END)=SUM(CASE WHEN o.is_inbox AND o.provider_read THEN 1 ELSE 0 END) THEN 'read'
 WHEN MAX(CASE WHEN NOT o.provider_read THEN 1 ELSE 0 END)=1 THEN 'unread'
 WHEN COUNT(mm.uid)>0 AND COUNT(mm.uid)=SUM(CASE WHEN o.provider_read THEN 1 ELSE 0 END) THEN 'read'
 ELSE 'unknown' END AS read_state
 FROM %[1]smessages m JOIN %[1]ssources s ON s.id=m.source_id
 JOIN %[1]simap_message_memberships mm ON mm.message_id=m.id AND mm.source_id=m.source_id
 LEFT JOIN %[1]sprovider_mailbox_observations o ON o.message_id=m.id AND o.source_id=m.source_id AND o.provider_id=m.source_message_id
 AND o.mailbox=mm.mailbox AND o.uidvalidity=mm.uidvalidity AND o.uid=mm.uid
 WHERE s.source_type='imap' AND COALESCE(m.message_type,'') IN ('','email') AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
 GROUP BY m.id`, prefix)
}

// ProviderMailboxStateContext reads metadata for one message. A missing or stale
// observation is explicitly Unknown, including on archives created before sync
// started recording observations.
func (s *Store) ProviderMailboxStateContext(ctx context.Context, messageID int64) (ProviderMailboxState, error) {
	state := ProviderMailboxState{InboxState: "unknown", ReadState: "unknown"}
	err := s.db.QueryRowContext(ctx, "SELECT inbox_state,read_state FROM ("+MailboxStateSelectSQL("")+") mailbox WHERE message_id=?", messageID).Scan(&state.InboxState, &state.ReadState)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("read provider mailbox state: %w", err)
	}
	return state, nil
}

func observeGmailMailboxTx(q querier, messageID int64, complete bool) error {
	// A history delta has only part of a label set. It may refresh an established
	// snapshot, but it cannot turn a legacy partial archive into a complete one.
	guard := ""
	if !complete {
		guard = " AND EXISTS (SELECT 1 FROM provider_mailbox_observations old WHERE old.message_id=m.id AND old.provider_id=m.source_message_id AND old.mailbox='')"
	}
	_, err := q.Exec(`INSERT INTO provider_mailbox_observations(message_id,source_id,provider_id,is_inbox,provider_read)
 SELECT m.id,m.source_id,m.source_message_id,
 EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=m.id AND l.source_id=m.source_id AND l.source_label_id='INBOX' AND l.label_type='system'),
 NOT EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=m.id AND l.source_id=m.source_id AND l.source_label_id='UNREAD' AND l.label_type='system')
 FROM messages m JOIN sources s ON s.id=m.source_id WHERE m.id=? AND COALESCE(s.source_type,'') IN ('','gmail') AND COALESCE(m.message_type,'') IN ('','email')`+guard+`
 ON CONFLICT(message_id,mailbox,uidvalidity,uid) DO UPDATE SET source_id=excluded.source_id,provider_id=excluded.provider_id,is_inbox=excluded.is_inbox,provider_read=excluded.provider_read`, messageID)
	return err
}

func observeIMAPMailboxTx(q querier, sourceID, messageID int64, o IMAPMembershipObservation) error {
	var read *bool
	if o.Flags != nil {
		v := slices.Contains(o.Flags, `\Seen`)
		read = &v
	}
	_, err := q.Exec(`INSERT INTO provider_mailbox_observations(message_id,source_id,provider_id,mailbox,uidvalidity,uid,is_inbox,provider_read)
 SELECT id,source_id,source_message_id,?,?,?,?,? FROM messages WHERE id=? AND source_id=?
 ON CONFLICT(message_id,mailbox,uidvalidity,uid) DO UPDATE SET source_id=excluded.source_id,provider_id=excluded.provider_id,is_inbox=excluded.is_inbox,provider_read=excluded.provider_read`, o.Mailbox, o.UIDValidity, o.UID, strings.EqualFold(o.Mailbox, "INBOX"), read, messageID, sourceID)
	return err
}

// ObserveMicrosoftMailboxContext saves the fields returned by a delta or lookup
// for one exact immutable provider ID. A missing isRead leaves a new row
// Unknown, but does not erase a value learned from an earlier complete response.
func (s *Store) ObserveMicrosoftMailboxContext(ctx context.Context, sourceID int64, providerID string, inbox, read *bool) error {
	return s.withSyncSourceWriteContext(ctx, sourceID, func(q querier) error {
		_, err := q.Exec(`INSERT INTO provider_mailbox_observations(message_id,source_id,provider_id,is_inbox,provider_read)
 SELECT m.id,m.source_id,m.source_message_id,?,? FROM messages m JOIN sources s ON s.id=m.source_id
 WHERE m.source_id=? AND m.source_message_id=? AND s.source_type='msmail'
 ON CONFLICT(message_id,mailbox,uidvalidity,uid) DO UPDATE SET source_id=excluded.source_id,provider_id=excluded.provider_id,is_inbox=excluded.is_inbox,provider_read=COALESCE(excluded.provider_read,provider_mailbox_observations.provider_read)`, inbox, read, sourceID, providerID)
		return err
	})
}
