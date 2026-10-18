package store

import (
	"fmt"
	"strings"
)

const loopQueueConflict = `ON CONFLICT(conversation_id) DO UPDATE SET
 revision=loop_conversation_queue.revision+1,
 feed_bound=CASE WHEN loop_conversation_queue.revision=loop_conversation_queue.processed_revision
   THEN excluded.feed_bound ELSE loop_conversation_queue.feed_bound END,
 reason=excluded.reason, queued_at=excluded.queued_at`

// loopSQLiteEnqueue resolves the target from a live parent row. Pending work
// retains its original bound (including NULL) and any rescan epoch.
func loopSQLiteEnqueue(from, conversation, guard, reason string) string {
	return fmt.Sprintf(`INSERT INTO loop_conversation_queue(conversation_id, revision, feed_bound, reason, queued_at)
 SELECT %s, 1, (SELECT feed_frontier FROM loop_derivation_state WHERE singleton=1), %s, CURRENT_TIMESTAMP
 FROM %s WHERE %s
 %s;`, conversation, reason, from, guard, loopQueueConflict)
}

// EnsureLoopProjectionTriggers runs after legacy message columns have been
// migrated. It also restores recipient triggers after a legacy table rebuild.
func (d *SQLiteDialect) EnsureLoopProjectionTriggers(q querier) error {
	var statements []string
	for _, association := range []string{"labels", "recipients"} {
		for _, event := range []string{"insert", "update", "delete"} {
			name := "trg_loop_queue_" + association + "_" + event
			var body strings.Builder
			if event != "insert" {
				body.WriteString(loopSQLiteEnqueue("messages", "conversation_id", "id=OLD.message_id AND conversation_id IS NOT NULL", "'"+association+"'"))
			}
			if event != "delete" {
				body.WriteString(loopSQLiteEnqueue("messages", "conversation_id", "id=NEW.message_id AND conversation_id IS NOT NULL", "'"+association+"'"))
			}
			statements = append(statements, "DROP TRIGGER IF EXISTS "+name,
				fmt.Sprintf("CREATE TRIGGER %s AFTER %s ON message_%s FOR EACH ROW BEGIN %s END", name, strings.ToUpper(event), association, body.String()))
		}
	}
	messageUpdateColumns := []string{"conversation_id"}
	messageUpdateConditions := []string{"OLD.conversation_id IS NOT NEW.conversation_id"}
	for _, column := range []string{"source_is_from_me", "identity_is_from_me"} {
		var present int
		if err := q.QueryRow("SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name=?", column).Scan(&present); err != nil {
			return fmt.Errorf("inspect message ownership column %s: %w", column, err)
		}
		if present == 0 {
			continue
		}
		messageUpdateColumns = append(messageUpdateColumns, column)
		messageUpdateConditions = append(messageUpdateConditions, fmt.Sprintf("OLD.%s IS NOT NEW.%s", column, column))
	}
	reason := "CASE WHEN OLD.conversation_id IS NOT NEW.conversation_id THEN 'moved' ELSE 'ownership' END"
	body := loopSQLiteEnqueue("conversations", "id", "id=OLD.conversation_id", reason) + loopSQLiteEnqueue("conversations", "id", "id=NEW.conversation_id", reason)
	statements = append(statements,
		"DROP TRIGGER IF EXISTS trg_loop_queue_messages_move",
		fmt.Sprintf(`CREATE TRIGGER trg_loop_queue_messages_move
 AFTER UPDATE OF %s ON messages FOR EACH ROW
 WHEN %s
	 BEGIN %s END`, strings.Join(messageUpdateColumns, ", "), strings.Join(messageUpdateConditions, "\n   OR "), body),
		"DROP TRIGGER IF EXISTS trg_loop_queue_conversation_type_update",
		`CREATE TRIGGER trg_loop_queue_conversation_type_update
 AFTER UPDATE OF conversation_type ON conversations FOR EACH ROW
 WHEN OLD.conversation_type IS NOT NEW.conversation_type
 BEGIN `+loopSQLiteEnqueue("conversations", "id", "id=NEW.id", "'conversation_type'")+` END`,
	)
	for _, event := range []string{"insert", "update", "delete"} {
		name := "trg_loop_queue_conversation_participants_" + event
		var body strings.Builder
		if event != "insert" {
			body.WriteString(loopSQLiteEnqueue("conversations", "id", "id=OLD.conversation_id", "'membership'"))
		}
		if event != "delete" {
			body.WriteString(loopSQLiteEnqueue("conversations", "id", "id=NEW.conversation_id", "'membership'"))
		}
		statement := fmt.Sprintf("CREATE TRIGGER %s AFTER %s ON conversation_participants FOR EACH ROW BEGIN %s END", name, strings.ToUpper(event), body.String())
		if event == "update" {
			statement = fmt.Sprintf(`CREATE TRIGGER %s AFTER UPDATE OF conversation_id, participant_id, role, left_at ON conversation_participants FOR EACH ROW
 WHEN OLD.conversation_id IS NOT NEW.conversation_id OR OLD.participant_id IS NOT NEW.participant_id OR OLD.role IS NOT NEW.role OR OLD.left_at IS NOT NEW.left_at
 BEGIN %s END`, name, body.String())
		}
		statements = append(statements, "DROP TRIGGER IF EXISTS "+name, statement)
	}
	return execLoopTriggerStatements(q, statements)
}

// EnsureLoopProjectionTriggers installs the same guarded invalidation contract
// on PostgreSQL. Only ownership/thread changes can fire the message
// trigger; bookkeeping updates do not add projection work.
func (d *PostgreSQLDialect) EnsureLoopProjectionTriggers(q querier) error {
	statements := []string{
		`CREATE OR REPLACE FUNCTION enqueue_loop_conversation(p_conversation_id BIGINT,p_reason TEXT) RETURNS VOID AS $$
   BEGIN
    INSERT INTO loop_conversation_queue(conversation_id, revision, feed_bound, reason, queued_at)
    SELECT id, 1, (SELECT feed_frontier FROM loop_derivation_state WHERE singleton=1), p_reason, CURRENT_TIMESTAMP
    FROM conversations WHERE id=p_conversation_id
    ` + loopQueueConflict + `;
   END;
   $$ LANGUAGE plpgsql`,
	}
	for _, association := range []string{"labels", "recipients"} {
		for _, event := range []string{"insert", "update", "delete"} {
			function := "queue_loop_" + association + "_" + event
			trigger := "trg_loop_queue_" + association + "_" + event
			var body strings.Builder
			if event != "insert" {
				fmt.Fprintf(&body, "PERFORM enqueue_loop_conversation((SELECT conversation_id FROM messages WHERE id=OLD.message_id), '%s');\n", association)
			}
			if event != "delete" {
				fmt.Fprintf(&body, "PERFORM enqueue_loop_conversation((SELECT conversation_id FROM messages WHERE id=NEW.message_id), '%s');\n", association)
			}
			returned := "NEW"
			if event == "delete" {
				returned = "OLD"
			}
			statements = append(statements,
				fmt.Sprintf("CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$ BEGIN %s RETURN %s; END; $$ LANGUAGE plpgsql", function, body.String(), returned),
				"DROP TRIGGER IF EXISTS "+trigger+" ON message_"+association,
				fmt.Sprintf("CREATE TRIGGER %s AFTER %s ON message_%s FOR EACH ROW EXECUTE FUNCTION %s()", trigger, strings.ToUpper(event), association, function))
		}
	}
	statements = append(statements,
		`CREATE OR REPLACE FUNCTION queue_loop_messages_move() RETURNS trigger AS $$
   DECLARE change_reason TEXT;
   BEGIN
    change_reason := CASE WHEN OLD.conversation_id IS DISTINCT FROM NEW.conversation_id THEN 'moved' ELSE 'ownership' END;
    PERFORM enqueue_loop_conversation(OLD.conversation_id,change_reason);
    PERFORM enqueue_loop_conversation(NEW.conversation_id,change_reason);
    RETURN NEW;
   END;
   $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_loop_queue_messages_move ON messages`,
		`CREATE TRIGGER trg_loop_queue_messages_move
	   AFTER UPDATE OF conversation_id, source_is_from_me, identity_is_from_me ON messages FOR EACH ROW
   WHEN (OLD.conversation_id IS DISTINCT FROM NEW.conversation_id
	      OR OLD.source_is_from_me IS DISTINCT FROM NEW.source_is_from_me
	      OR OLD.identity_is_from_me IS DISTINCT FROM NEW.identity_is_from_me)
   EXECUTE FUNCTION queue_loop_messages_move()`,
		`CREATE OR REPLACE FUNCTION queue_loop_conversation_type() RETURNS trigger AS $$
   BEGIN
    IF OLD.conversation_type IS DISTINCT FROM NEW.conversation_type THEN
     PERFORM enqueue_loop_conversation(NEW.id,'conversation_type');
    END IF;
    RETURN NEW;
   END;
   $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_loop_queue_conversation_type_update ON conversations`,
		`CREATE TRIGGER trg_loop_queue_conversation_type_update
   AFTER UPDATE OF conversation_type ON conversations FOR EACH ROW
   EXECUTE FUNCTION queue_loop_conversation_type()`,
		`CREATE OR REPLACE FUNCTION queue_loop_conversation_participant() RETURNS trigger AS $$
   BEGIN
    IF TG_OP <> 'INSERT' THEN
     PERFORM enqueue_loop_conversation(OLD.conversation_id,'membership');
    END IF;
    IF TG_OP <> 'DELETE' THEN
     PERFORM enqueue_loop_conversation(NEW.conversation_id,'membership');
    END IF;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
   END;
   $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_loop_queue_conversation_participants ON conversation_participants`,
		`CREATE TRIGGER trg_loop_queue_conversation_participants
   AFTER INSERT OR UPDATE OR DELETE ON conversation_participants FOR EACH ROW
   EXECUTE FUNCTION queue_loop_conversation_participant()`)
	return execLoopTriggerStatements(q, statements)
}

func execLoopTriggerStatements(q querier, statements []string) error {
	for i, statement := range statements {
		if _, err := q.Exec(statement); err != nil {
			return fmt.Errorf("install loop projection trigger statement %d: %w", i+1, err)
		}
	}
	return nil
}
