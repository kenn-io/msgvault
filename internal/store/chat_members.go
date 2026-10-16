package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

// chatMembersRefreshBatch bounds one refresh transaction, so a large import
// is applied in short write transactions instead of one long one.
const chatMembersRefreshBatch = 200

// ErrChatMembersStale reports pending chat membership changes that a
// read-only Store cannot apply.
var ErrChatMembersStale = errors.New("chat membership projection has pending changes")

var chatConversationTypesSQL = "('" + strings.Join(identityindex.ChatConversationTypes, "', '") + "')"

// installChatMembers creates the chat membership projection. chat_members
// holds every live sender and non-mention recipient of each chat, with the
// recipient display name as alias, so discovery never scans messages.
// Triggers queue a chat in chat_members_dirty when its messages or
// recipients are updated or deleted, or its type changes. Inserts are queued
// by UpsertMessage and replaceMessageRecipientsTx instead, because per-row
// INSERT triggers slow every import.
func (s *Store) installChatMembers(ctx context.Context, tx *loggedTx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS chat_members (
			conversation_id BIGINT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
			participant_id BIGINT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
			alias TEXT NOT NULL,
			PRIMARY KEY (conversation_id, participant_id, alias)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chat_members_participant ON chat_members(participant_id)`,
		`CREATE TABLE IF NOT EXISTS chat_members_dirty (
			conversation_id BIGINT PRIMARY KEY
		)`,
		// chat_anchors holds each chat's newest visible message. SQLite stores
		// sent_at as text with mixed offsets, so only a parsed sort finds the
		// newest instant; doing that at refresh keeps it out of every search.
		// message_id has no foreign key: its share lock would make a refresh
		// wait on writers that lock a message and then wait on the claimed chat.
		// Discovery validates the anchor when it reads it instead.
		`CREATE TABLE IF NOT EXISTS chat_anchors (
			conversation_id BIGINT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
			message_id BIGINT NOT NULL
		)`,
		// Discovery checks each member against the source's own sent
		// messages; this keeps that an index probe instead of a scan.
		`CREATE INDEX IF NOT EXISTS idx_messages_source_owner
			ON messages(sender_id, source_id) WHERE source_is_from_me = TRUE`,
	}
	if s.IsPostgreSQL() {
		statements = append(statements, postgresChatMemberTriggers()...)
	} else {
		statements = append(statements, sqliteChatMemberTriggers()...)
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("install chat members projection: %w", err)
		}
	}
	return nil
}

// sqliteChatMemberTriggers use upserts, not INSERT OR IGNORE: SQLite lets the
// conflict clause of the statement that fires a trigger override OR IGNORE.
func sqliteChatMemberTriggers() []string {
	markMessage := func(messageExpr string) string {
		return `INSERT INTO chat_members_dirty (conversation_id)
			SELECT c.id FROM messages m JOIN conversations c ON c.id = m.conversation_id
			WHERE m.id = ` + messageExpr + ` AND c.conversation_type IN ` + chatConversationTypesSQL + `
			ON CONFLICT (conversation_id) DO NOTHING;`
	}
	return []string{
		`DROP TRIGGER IF EXISTS trg_chat_members_messages_update`,
		`CREATE TRIGGER trg_chat_members_messages_update
			AFTER UPDATE OF conversation_id, sender_id, deleted_at, sent_at ON messages FOR EACH ROW
			WHEN OLD.conversation_id IS NOT NEW.conversation_id OR OLD.sender_id IS NOT NEW.sender_id
				OR OLD.deleted_at IS NOT NEW.deleted_at OR OLD.sent_at IS NOT NEW.sent_at
			BEGIN
				INSERT INTO chat_members_dirty (conversation_id)
				SELECT id FROM conversations
				WHERE id IN (OLD.conversation_id, NEW.conversation_id)
				AND conversation_type IN ` + chatConversationTypesSQL + `
				ON CONFLICT (conversation_id) DO NOTHING;
			END`,
		`DROP TRIGGER IF EXISTS trg_chat_members_messages_delete`,
		`CREATE TRIGGER trg_chat_members_messages_delete
			AFTER DELETE ON messages FOR EACH ROW
			BEGIN
				INSERT INTO chat_members_dirty (conversation_id)
				SELECT id FROM conversations
				WHERE id = OLD.conversation_id AND conversation_type IN ` + chatConversationTypesSQL + `
				ON CONFLICT (conversation_id) DO NOTHING;
			END`,
		`DROP TRIGGER IF EXISTS trg_chat_members_recipients_update`,
		`CREATE TRIGGER trg_chat_members_recipients_update
			AFTER UPDATE ON message_recipients FOR EACH ROW
			WHEN OLD.message_id IS NOT NEW.message_id OR OLD.participant_id IS NOT NEW.participant_id
				OR OLD.recipient_type IS NOT NEW.recipient_type OR OLD.display_name IS NOT NEW.display_name
			BEGIN ` + markMessage("OLD.message_id") + markMessage("NEW.message_id") + ` END`,
		`DROP TRIGGER IF EXISTS trg_chat_members_recipients_delete`,
		`CREATE TRIGGER trg_chat_members_recipients_delete
			AFTER DELETE ON message_recipients FOR EACH ROW
			BEGIN ` + markMessage("OLD.message_id") + ` END`,
		`DROP TRIGGER IF EXISTS trg_chat_members_conversation_type`,
		`CREATE TRIGGER trg_chat_members_conversation_type
			AFTER UPDATE OF conversation_type ON conversations FOR EACH ROW
			WHEN OLD.conversation_type IS NOT NEW.conversation_type
			BEGIN
				INSERT INTO chat_members_dirty (conversation_id) VALUES (NEW.id)
				ON CONFLICT (conversation_id) DO NOTHING;
			END`,
	}
}

func postgresChatMemberTriggers() []string {
	return []string{
		// Message UPDATE triggers enqueue before journaling; fresh inserts
		// journal before their application-side enqueue. Lock the enabled
		// journal clock before every enqueue takes a dirty-chat row lock,
		// including INSERT ... ON CONFLICT from either path.
		`CREATE OR REPLACE FUNCTION lock_chat_members_journal_clock() RETURNS trigger AS $$
		 BEGIN
		     PERFORM pg_advisory_xact_lock_shared(
		         hashtextextended('msgvault.embedding_change_clock', 0));
		     PERFORM sequence FROM embedding_change_clock
		     WHERE singleton = 1 AND enabled FOR NO KEY UPDATE;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_chat_members_dirty_journal_clock ON chat_members_dirty`,
		`CREATE TRIGGER trg_chat_members_dirty_journal_clock
		     BEFORE INSERT ON chat_members_dirty FOR EACH ROW
		     EXECUTE FUNCTION lock_chat_members_journal_clock()`,
		`CREATE OR REPLACE FUNCTION mark_chat_members_dirty(p_conversation_id BIGINT) RETURNS VOID AS $$
		 BEGIN
		     INSERT INTO chat_members_dirty (conversation_id)
		     SELECT id FROM conversations
		     WHERE id = p_conversation_id AND conversation_type IN ` + chatConversationTypesSQL + `
		     ON CONFLICT (conversation_id) DO UPDATE SET conversation_id = excluded.conversation_id;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION chat_members_messages_changed() RETURNS trigger AS $$
		 BEGIN
		     IF TG_OP = 'DELETE' THEN
		         PERFORM mark_chat_members_dirty(OLD.conversation_id);
		         RETURN OLD;
		     END IF;
		     PERFORM mark_chat_members_dirty(OLD.conversation_id);
		     PERFORM mark_chat_members_dirty(NEW.conversation_id);
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION chat_members_recipients_changed() RETURNS trigger AS $$
		 BEGIN
		     PERFORM mark_chat_members_dirty((SELECT conversation_id FROM messages WHERE id = OLD.message_id));
		     IF TG_OP = 'DELETE' THEN
		         RETURN OLD;
		     END IF;
		     PERFORM mark_chat_members_dirty((SELECT conversation_id FROM messages WHERE id = NEW.message_id));
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION chat_members_conversation_type_changed() RETURNS trigger AS $$
		 BEGIN
		     INSERT INTO chat_members_dirty (conversation_id) VALUES (NEW.id)
		     ON CONFLICT (conversation_id) DO UPDATE SET conversation_id = excluded.conversation_id;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_chat_members_messages_update ON messages`,
		`CREATE TRIGGER trg_chat_members_messages_update
		     AFTER UPDATE OF conversation_id, sender_id, deleted_at, sent_at ON messages FOR EACH ROW
		     WHEN (OLD.conversation_id IS DISTINCT FROM NEW.conversation_id
		         OR OLD.sender_id IS DISTINCT FROM NEW.sender_id
		         OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
		         OR OLD.sent_at IS DISTINCT FROM NEW.sent_at)
		     EXECUTE FUNCTION chat_members_messages_changed()`,
		`DROP TRIGGER IF EXISTS trg_chat_members_messages_delete ON messages`,
		`CREATE TRIGGER trg_chat_members_messages_delete
		     AFTER DELETE ON messages FOR EACH ROW
		     EXECUTE FUNCTION chat_members_messages_changed()`,
		`DROP TRIGGER IF EXISTS trg_chat_members_recipients_delete ON message_recipients`,
		`CREATE TRIGGER trg_chat_members_recipients_delete
		     AFTER DELETE ON message_recipients FOR EACH ROW
		     EXECUTE FUNCTION chat_members_recipients_changed()`,
		`DROP TRIGGER IF EXISTS trg_chat_members_recipients_update ON message_recipients`,
		`CREATE TRIGGER trg_chat_members_recipients_update
		     AFTER UPDATE ON message_recipients FOR EACH ROW
		     WHEN (OLD.message_id IS DISTINCT FROM NEW.message_id
		         OR OLD.participant_id IS DISTINCT FROM NEW.participant_id
		         OR OLD.recipient_type IS DISTINCT FROM NEW.recipient_type
		         OR OLD.display_name IS DISTINCT FROM NEW.display_name)
		     EXECUTE FUNCTION chat_members_recipients_changed()`,
		`DROP TRIGGER IF EXISTS trg_chat_members_conversation_type ON conversations`,
		`CREATE TRIGGER trg_chat_members_conversation_type
		     AFTER UPDATE OF conversation_type ON conversations FOR EACH ROW
		     WHEN (OLD.conversation_type IS DISTINCT FROM NEW.conversation_type)
		     EXECUTE FUNCTION chat_members_conversation_type_changed()`,
	}
}

// enqueueChatMembers queues a chat after UpsertMessage writes one of its
// messages. Non-chat conversations are ignored. Enqueues update an existing
// marker instead of ignoring it, so the writer holds the marker's row lock
// until it commits and a PostgreSQL refresh cannot claim the chat early.
func enqueueChatMembers(q querier, conversationID int64) error {
	if _, err := q.Exec(`INSERT INTO chat_members_dirty (conversation_id)
		SELECT id FROM conversations WHERE id = ? AND conversation_type IN `+chatConversationTypesSQL+`
		ON CONFLICT (conversation_id) DO UPDATE SET conversation_id = excluded.conversation_id`, conversationID); err != nil {
		return fmt.Errorf("enqueue chat members for conversation %d: %w", conversationID, err)
	}
	return nil
}

// enqueueChatMembersForMessage queues a message's chat after its recipients
// are inserted. One statement per recipient set replaces a per-row trigger.
func enqueueChatMembersForMessage(q querier, messageID int64) error {
	if _, err := q.Exec(`INSERT INTO chat_members_dirty (conversation_id)
		SELECT c.id FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.id = ? AND c.conversation_type IN `+chatConversationTypesSQL+`
		ON CONFLICT (conversation_id) DO UPDATE SET conversation_id = excluded.conversation_id`, messageID); err != nil {
		return fmt.Errorf("enqueue chat members for message %d: %w", messageID, err)
	}
	return nil
}

// rebuildChatMembersContext queues every chat and rebuilds the projection in
// one maintenance transaction. Migration and subset copies use it after
// writing messages without UpsertMessage.
func (s *Store) rebuildChatMembersContext(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if err := s.installChatMembers(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(
			`INSERT OR IGNORE INTO chat_members_dirty (conversation_id)
			SELECT id FROM conversations WHERE conversation_type IN `+chatConversationTypesSQL,
		)); err != nil {
			return fmt.Errorf("queue chat members rebuild: %w", err)
		}
		for {
			claimed, err := s.refreshChatMembersBatchTx(ctx, tx)
			if err != nil || claimed < chatMembersRefreshBatch {
				return err
			}
		}
	})
}

// RefreshChatMembersContext applies queued chat changes before discovery
// reads the projection. A clean projection costs one indexed read and takes
// no write lock.
func (s *Store) RefreshChatMembersContext(ctx context.Context) error {
	var dirty bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM chat_members_dirty)`).Scan(&dirty); err != nil {
		return fmt.Errorf("check chat members projection: %w", err)
	}
	if !dirty {
		return nil
	}
	if s.readOnly {
		return ErrChatMembersStale
	}
	for {
		claimed := 0
		err := retryBusyWriteErr(ctx, s, "refresh chat members", func() error {
			// Source removal takes the identity row before its table locks.
			// Sharing that row first keeps this refresh from holding the
			// queue while removal waits on it and then blocks the refresh.
			return s.withAttributionTxContext(ctx, attributionLock{}, func(tx *loggedTx) error {
				var err error
				claimed, err = s.refreshChatMembersBatchTx(ctx, tx)
				return err
			})
		})
		if err != nil || claimed < chatMembersRefreshBatch {
			return err
		}
	}
}

// refreshChatMembersBatchTx claims up to one batch of queued chats and
// rebuilds their rows from live messages. The claim is the transaction's
// first write, and PostgreSQL refreshes serialize on an advisory lock, so
// two refreshes never rebuild the same chat at once.
func (s *Store) refreshChatMembersBatchTx(ctx context.Context, tx *loggedTx) (int, error) {
	if s.IsPostgreSQL() {
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended('msgvault.chat_members', 0))`); err != nil {
			return 0, fmt.Errorf("lock chat members refresh: %w", err)
		}
	}
	// Writers row-lock a queued chat until they commit (see enqueueChatMembers).
	// PostgreSQL skips those chats instead of rebuilding them from data that
	// does not yet include the write; they stay queued for the next refresh.
	// SQLite's single writer cannot overlap an open write.
	skipLocked := ""
	if s.IsPostgreSQL() {
		skipLocked = " FOR UPDATE SKIP LOCKED"
	}
	rows, err := tx.QueryContext(ctx, s.dialect.Rebind(`DELETE FROM chat_members_dirty
		WHERE conversation_id IN (
			SELECT conversation_id FROM chat_members_dirty ORDER BY conversation_id LIMIT ?`+skipLocked+`
		) RETURNING conversation_id`), chatMembersRefreshBatch)
	if err != nil {
		return 0, fmt.Errorf("claim dirty chat members: %w", err)
	}
	ids := []any{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan dirty chat members: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close dirty chat members: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read dirty chat members: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if s.chatMembersAfterClaimHook != nil {
		s.chatMembersAfterClaimHook()
	}
	if err := s.rebuildChatProjectionTx(ctx, tx, ids); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// rebuildChatProjectionTx rewrites the member and anchor rows of claimed
// chats from their live messages. A chat that is gone or no longer a chat
// type keeps no rows.
func (s *Store) rebuildChatProjectionTx(ctx context.Context, tx *loggedTx, ids []any) error {
	in := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ") + ")"
	for _, table := range []string{"chat_members", "chat_anchors"} {
		if _, err := tx.ExecContext(ctx, s.dialect.Rebind(`DELETE FROM `+table+` WHERE conversation_id IN `+in), ids...); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	live := LiveMessagesWhere("m", false)
	chats := `JOIN conversations c ON c.id = m.conversation_id
		WHERE m.conversation_id IN ` + in + ` AND c.conversation_type IN ` + chatConversationTypesSQL + ` AND ` + live
	members := `INSERT INTO chat_members (conversation_id, participant_id, alias)
		SELECT m.conversation_id, m.sender_id, '' FROM messages m ` + chats + ` AND m.sender_id IS NOT NULL
		UNION
		SELECT m.conversation_id, r.participant_id, COALESCE(r.display_name, '') FROM messages m
		JOIN message_recipients r ON r.message_id = m.id ` + chats + ` AND r.recipient_type <> 'mention'`
	if _, err := tx.ExecContext(ctx, s.dialect.Rebind(members), append(ids, ids...)...); err != nil {
		return fmt.Errorf("rebuild chat members: %w", err)
	}
	// Dated messages win over undated ones; ties go to the later-stored row.
	sentKey := sqliteutil.TimestampKeyFunction + "(m.sent_at)"
	if s.IsPostgreSQL() {
		sentKey = "m.sent_at"
	}
	anchors := `INSERT INTO chat_anchors (conversation_id, message_id)
		SELECT conversation_id, id FROM (
			SELECT m.conversation_id, m.id, ROW_NUMBER() OVER (
				PARTITION BY m.conversation_id
				ORDER BY CASE WHEN ` + sentKey + ` IS NULL THEN 1 ELSE 0 END, ` + sentKey + ` DESC, m.id DESC
			) AS newest
			FROM messages m ` + chats + `
		) ranked WHERE newest = 1`
	if _, err := tx.ExecContext(ctx, s.dialect.Rebind(anchors), ids...); err != nil {
		return fmt.Errorf("rebuild chat anchors: %w", err)
	}
	return nil
}
