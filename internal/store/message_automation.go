package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

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
	return s.messageAutomationContext(ctx, `automation_flags`, id)
}

// LoopFileAutomationContext reads current flags only while the message keeps a
// standalone file, the fact loops cache in place of parsing the original.
func (s *Store) LoopFileAutomationContext(ctx context.Context, id int64) (flags uint32, current bool, err error) {
	return s.messageAutomationContext(ctx, `CASE WHEN EXISTS (SELECT 1 FROM attachments a
 WHERE a.message_id = messages.id AND a.attachment_role = 'standalone') THEN automation_flags END`, id)
}

func (s *Store) messageAutomationContext(ctx context.Context, flagsColumn string, id int64) (uint32, bool, error) {
	var value, version sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT `+flagsColumn+`, automation_rule_version FROM messages WHERE id = ?`, id).Scan(&value, &version)
	if err != nil {
		return 0, false, fmt.Errorf("read message automation: %w", err)
	}
	rawFlags := value.Int64
	if !value.Valid || !version.Valid || version.Int64 != AutomationRuleVersion || rawFlags < 0 || rawFlags > math.MaxUint32 {
		return 0, false, nil
	}
	return uint32(rawFlags), true, nil
}

// PutMessageAutomationContext publishes both derived columns together.
func (s *Store) PutMessageAutomationContext(ctx context.Context, id int64, flags uint32) error {
	_, err := putMessageAutomationWith(ctx, s.db, id, flags)
	if err != nil {
		return fmt.Errorf("write message automation: %w", err)
	}
	return nil
}

// putMessageAutomationWith publishes under the caller's raw snapshot lock.
// Fingerprint validation and transaction ownership stay with the caller.
func putMessageAutomationWith(ctx context.Context, q contextStatementQuerier, id int64, flags uint32) (bool, error) {
	result, err := q.ExecContext(ctx, `UPDATE messages SET automation_flags = ?, automation_rule_version = ? WHERE id = ?
 AND (automation_flags IS DISTINCT FROM ? OR automation_rule_version IS DISTINCT FROM ?)`, flags, AutomationRuleVersion, id, flags, AutomationRuleVersion)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// Install after legacy columns: the schema script also runs on older archives.
// Loops cache a file fact from attachment roles, which arrive with those columns.
func (s *Store) initAutomationTriggers(ctx context.Context) error {
	statement := `CREATE TRIGGER IF NOT EXISTS trg_messages_automation_raw_update
 AFTER UPDATE ON message_raw FOR EACH ROW
 WHEN OLD.message_id IS NOT NEW.message_id OR OLD.raw_data IS NOT NEW.raw_data OR OLD.raw_format IS NOT NEW.raw_format
 OR OLD.compression IS NOT NEW.compression OR OLD.encryption_version IS NOT NEW.encryption_version
 BEGIN
 UPDATE messages SET automation_flags = NULL, automation_rule_version = NULL
 WHERE id IN (OLD.message_id, NEW.message_id) AND (automation_flags IS NOT NULL OR automation_rule_version IS NOT NULL);
 END;
 CREATE TRIGGER IF NOT EXISTS trg_messages_automation_raw_delete
 AFTER DELETE ON message_raw FOR EACH ROW BEGIN
 UPDATE messages SET automation_flags = NULL, automation_rule_version = NULL WHERE id = OLD.message_id AND (automation_flags IS NOT NULL OR automation_rule_version IS NOT NULL);
 END;
 CREATE TRIGGER IF NOT EXISTS trg_loop_evidence_attachments_update
 AFTER UPDATE OF attachment_role, message_id ON attachments FOR EACH ROW
 WHEN OLD.attachment_role = 'standalone'
 AND (NEW.attachment_role IS NOT 'standalone' OR NEW.message_id IS NOT OLD.message_id)
 BEGIN
 DELETE FROM loop_message_evidence WHERE message_id = OLD.message_id;
 END;
 CREATE TRIGGER IF NOT EXISTS trg_loop_evidence_attachments_delete
 AFTER DELETE ON attachments FOR EACH ROW WHEN OLD.attachment_role = 'standalone' BEGIN
 DELETE FROM loop_message_evidence WHERE message_id = OLD.message_id;
 END;`
	if s.IsPostgreSQL() {
		statement = `CREATE OR REPLACE FUNCTION invalidate_messages_automation_raw() RETURNS TRIGGER AS $$
 BEGIN
 IF TG_OP = 'UPDATE' THEN
 IF OLD.message_id IS NOT DISTINCT FROM NEW.message_id AND OLD.raw_data IS NOT DISTINCT FROM NEW.raw_data
 AND OLD.raw_format IS NOT DISTINCT FROM NEW.raw_format AND OLD.compression IS NOT DISTINCT FROM NEW.compression
 AND OLD.encryption_version IS NOT DISTINCT FROM NEW.encryption_version THEN RETURN NULL; END IF;
 UPDATE messages SET automation_flags = NULL, automation_rule_version = NULL WHERE id IN (OLD.message_id, NEW.message_id) AND (automation_flags IS NOT NULL OR automation_rule_version IS NOT NULL);
 ELSE
 UPDATE messages SET automation_flags = NULL, automation_rule_version = NULL WHERE id = OLD.message_id AND (automation_flags IS NOT NULL OR automation_rule_version IS NOT NULL);
 END IF;
 RETURN NULL;
 END;
 $$ LANGUAGE plpgsql;
 DROP TRIGGER IF EXISTS trg_messages_automation_raw ON message_raw;
 CREATE TRIGGER trg_messages_automation_raw AFTER UPDATE OR DELETE ON message_raw
 FOR EACH ROW EXECUTE FUNCTION invalidate_messages_automation_raw();
 CREATE OR REPLACE FUNCTION invalidate_loop_evidence_attachments() RETURNS TRIGGER AS $$
 BEGIN
 IF OLD.attachment_role IS DISTINCT FROM 'standalone' THEN RETURN NULL; END IF;
 IF TG_OP = 'UPDATE' THEN
 IF NEW.attachment_role = 'standalone' AND NEW.message_id IS NOT DISTINCT FROM OLD.message_id THEN RETURN NULL; END IF;
 END IF;
 DELETE FROM loop_message_evidence WHERE message_id = OLD.message_id;
 RETURN NULL;
 END;
 $$ LANGUAGE plpgsql;
 DROP TRIGGER IF EXISTS trg_loop_evidence_attachments ON attachments;
 CREATE TRIGGER trg_loop_evidence_attachments AFTER UPDATE OF attachment_role, message_id OR DELETE ON attachments
 FOR EACH ROW EXECUTE FUNCTION invalidate_loop_evidence_attachments();`
	}
	if _, err := s.db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("install automation invalidation: %w", err)
	}
	return nil
}

// UpdateArchivedEmailAutomationContext refuses evidence from a stale original.
func (s *Store) UpdateArchivedEmailAutomationContext(ctx context.Context, item *ArchivedRawMessage, flags uint32) (changed bool, err error) {
	if item.rawFingerprint == nil {
		return false, errors.New("automation repair requires a scanned MIME fingerprint")
	}
	// A matching scanned pair needs no write. Any subsequent raw replacement
	// invalidates its flags independently.
	if item.automationFlags.Valid && item.automationFlags.Int64 == int64(flags) &&
		item.automationRuleVersion.Valid && item.automationRuleVersion.Int64 == AutomationRuleVersion {
		return false, ctx.Err()
	}
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockArchivedEmailTextContext(ctx, tx, item); err != nil {
			return err
		}
		var writeErr error
		changed, writeErr = putMessageAutomationWith(ctx, tx, item.MessageID, flags)
		if writeErr != nil {
			return writeErr
		}
		if changed {
			return setArchiveMetadataValueTx(ctx, tx, pendingDerivedEmailRevisionKey, "1")
		}
		return nil
	})
	return changed, err
}
