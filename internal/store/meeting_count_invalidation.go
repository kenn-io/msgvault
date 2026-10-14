package store

import (
	"context"
	"fmt"
	"strings"
)

// Cached meeting keys depend on evidence outside their transcript message.
// Advance the existing full-repair revision in the evidence transaction. The
// PostgreSQL backend has no Parquet cache and reads these keys live.
func (s *Store) installMeetingCountInvalidationContext(ctx context.Context) error {
	if s.IsPostgreSQL() {
		return nil
	}
	const bump = `INSERT INTO archive_metadata(key,value) VALUES('derived_data_revision','1')
 ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT);`
	tables := []string{
		"calendar_events", "calendar_event_attendees",
		"meeting_link_keys", "meeting_link_decisions",
		"person_contact_points", "person_participants",
		"participant_identifiers",
	}
	for _, table := range tables {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			statement := fmt.Sprintf("CREATE TRIGGER IF NOT EXISTS meeting_counts_%s_%s AFTER %s ON %s BEGIN %s END", table, strings.ToLower(event), event, table, bump)
			if _, err := s.db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("install meeting count invalidation: %w", err)
			}
		}
	}
	for _, item := range []struct{ name, columns, changed string }{
		{"sources", "sync_config,source_type,identifier", "OLD.sync_config IS NOT NEW.sync_config OR OLD.source_type IS NOT NEW.source_type OR OLD.identifier IS NOT NEW.identifier"},
		{"participants", "email_address,phone_number", "OLD.email_address IS NOT NEW.email_address OR OLD.phone_number IS NOT NEW.phone_number"},
	} {
		statement := fmt.Sprintf("CREATE TRIGGER IF NOT EXISTS meeting_counts_%s_update AFTER UPDATE OF %s ON %s WHEN %s BEGIN %s END", item.name, item.columns, item.name, item.changed, bump)
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("install meeting count invalidation: %w", err)
		}
		statement = fmt.Sprintf("CREATE TRIGGER IF NOT EXISTS meeting_counts_%s_delete AFTER DELETE ON %s BEGIN %s END", item.name, item.name, bump)
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("install meeting count deletion invalidation: %w", err)
		}
	}
	// Replace the original broad message triggers. Ordinary message edits do
	// not affect the cached meeting population; transcript changes and calendar
	// visibility changes do.
	for _, name := range []string{
		"meeting_counts_messages_update",
		"meeting_counts_messages_delete",
		"meeting_counts_messages_update_v2",
	} {
		if _, err := s.db.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+name); err != nil {
			return fmt.Errorf("replace meeting count invalidation trigger: %w", err)
		}
	}
	const messageChanges = `OLD.source_id IS NOT NEW.source_id OR OLD.message_type IS NOT NEW.message_type OR OLD.sent_at IS NOT NEW.sent_at OR OLD.deleted_at IS NOT NEW.deleted_at OR OLD.deleted_from_source_at IS NOT NEW.deleted_from_source_at OR OLD.sender_id IS NOT NEW.sender_id`
	const transcriptChanges = `(OLD.message_type = 'meeting_transcript' OR NEW.message_type = 'meeting_transcript') AND (` + messageChanges + `)`
	const calendarVisibilityChanges = `(OLD.message_type = 'calendar_event' OR NEW.message_type = 'calendar_event') AND (OLD.deleted_at IS NOT NEW.deleted_at OR OLD.deleted_from_source_at IS NOT NEW.deleted_from_source_at OR OLD.message_type IS NOT NEW.message_type)`
	updateMessage := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS meeting_counts_messages_update_v3
 AFTER UPDATE OF source_id,message_type,sent_at,deleted_at,deleted_from_source_at,sender_id ON messages
 WHEN %s OR %s
 BEGIN %s END`, transcriptChanges, calendarVisibilityChanges, bump)
	if _, err := s.db.ExecContext(ctx, updateMessage); err != nil {
		return fmt.Errorf("install meeting count message update invalidation: %w", err)
	}
	deleteMessage := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS meeting_counts_messages_delete_v2
 AFTER DELETE ON messages WHEN OLD.message_type = 'meeting_transcript'
 BEGIN %s END`, bump)
	if _, err := s.db.ExecContext(ctx, deleteMessage); err != nil {
		return fmt.Errorf("install meeting count message deletion invalidation: %w", err)
	}
	return nil
}
