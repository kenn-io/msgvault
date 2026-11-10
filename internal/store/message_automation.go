package store

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/msgvault/internal/mime"
)

const AutomationRuleVersion = mime.AutomationEvidenceVersion<<8 | 1

const (
	FlagAutoSubmitted   = mime.FlagAutoSubmitted
	FlagPrecedenceBulk  = mime.FlagPrecedenceBulk
	FlagListID          = mime.FlagListID
	FlagListUnsubscribe = mime.FlagListUnsubscribe
	FlagCalendarPart    = mime.FlagCalendarPart
	FlagReplyToNoReply  = mime.FlagReplyToNoReply
	FlagSenderNoReply   = mime.FlagSenderNoReply
	FlagBotSender       = mime.FlagBotSender
	BulkMask            = mime.BulkMask
)

// MessageAutomationContext returns unknown for missing or stale evidence.
func (s *Store) MessageAutomationContext(ctx context.Context, id int64) (flags uint32, current bool, err error) {
	var value, version sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT automation_flags, automation_rule_version FROM messages WHERE id = ?`, id).Scan(&value, &version)
	if err != nil {
		return 0, false, fmt.Errorf("read message automation: %w", err)
	}
	if !value.Valid || !version.Valid || version.Int64 != AutomationRuleVersion {
		return 0, false, nil
	}
	return uint32(value.Int64), true, nil
}

// PutMessageAutomationContext publishes both derived columns together.
func (s *Store) PutMessageAutomationContext(ctx context.Context, id int64, flags uint32) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET automation_flags = ?, automation_rule_version = ? WHERE id = ?`, flags, AutomationRuleVersion, id)
	if err != nil {
		return fmt.Errorf("write message automation: %w", err)
	}
	return nil
}

// Install after legacy columns: the schema script also runs on older archives.
func (s *Store) initAutomationTriggers(ctx context.Context) error {
	statement := `CREATE TRIGGER IF NOT EXISTS trg_messages_automation_raw_update
 AFTER UPDATE ON message_raw FOR EACH ROW
 WHEN OLD.message_id IS NOT NEW.message_id OR OLD.raw_data IS NOT NEW.raw_data OR OLD.raw_format IS NOT NEW.raw_format
 OR OLD.compression IS NOT NEW.compression OR OLD.encryption_version IS NOT NEW.encryption_version
 BEGIN
 UPDATE messages SET automation_flags = NULL, automation_rule_version = NULL
 WHERE id IN (OLD.message_id, NEW.message_id);
 END;
 CREATE TRIGGER IF NOT EXISTS trg_messages_automation_raw_delete
 AFTER DELETE ON message_raw FOR EACH ROW BEGIN
 UPDATE messages SET automation_flags = NULL, automation_rule_version = NULL WHERE id = OLD.message_id;
 END;`
	if s.IsPostgreSQL() {
		statement = `CREATE OR REPLACE FUNCTION invalidate_messages_automation_raw() RETURNS TRIGGER AS $$
 BEGIN
 IF TG_OP = 'UPDATE' THEN
 IF OLD.message_id IS NOT DISTINCT FROM NEW.message_id AND OLD.raw_data IS NOT DISTINCT FROM NEW.raw_data
 AND OLD.raw_format IS NOT DISTINCT FROM NEW.raw_format AND OLD.compression IS NOT DISTINCT FROM NEW.compression
 AND OLD.encryption_version IS NOT DISTINCT FROM NEW.encryption_version THEN RETURN NULL; END IF;
 UPDATE messages SET automation_flags = NULL, automation_rule_version = NULL WHERE id IN (OLD.message_id, NEW.message_id);
 ELSE
 UPDATE messages SET automation_flags = NULL, automation_rule_version = NULL WHERE id = OLD.message_id;
 END IF;
 RETURN NULL;
 END;
 $$ LANGUAGE plpgsql;
 DROP TRIGGER IF EXISTS trg_messages_automation_raw ON message_raw;
 CREATE TRIGGER trg_messages_automation_raw AFTER UPDATE OR DELETE ON message_raw
 FOR EACH ROW EXECUTE FUNCTION invalidate_messages_automation_raw();`
	}
	if _, err := s.db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("install automation invalidation: %w", err)
	}
	return nil
}

// UpdateArchivedEmailAutomationContext refuses evidence from a stale original.
func (s *Store) UpdateArchivedEmailAutomationContext(ctx context.Context, item *ArchivedRawMessage, flags uint32) (changed bool, err error) {
	if item.rawFingerprint == nil {
		return false, fmt.Errorf("automation repair requires a scanned MIME fingerprint")
	}
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockArchivedEmailTextContext(ctx, tx, item); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE messages SET automation_flags = ?, automation_rule_version = ? WHERE id = ?
 AND (automation_flags IS DISTINCT FROM ? OR automation_rule_version IS DISTINCT FROM ?)`, flags, AutomationRuleVersion, item.MessageID, flags, AutomationRuleVersion)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		changed = n > 0
		if changed {
			return setArchiveMetadataValueTx(ctx, tx, pendingDerivedEmailRevisionKey, "1")
		}
		return nil
	})
	return changed, err
}
