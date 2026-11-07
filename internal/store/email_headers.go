package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/emailattribution"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

const emailReplyMetadataKey = "email_in_reply_to"
const emailReplyBatchSize = 200

// RecordEmailHeadersContext fills missing imported header facts without replacing
// the archived message. The cache revision and a repaired RFC ID commit together.
func (s *Store) RecordEmailHeadersContext(ctx context.Context, sourceID, messageID int64, rfcID, inReplyTo string) error {
	return s.recordEmailHeadersContext(ctx, sourceID, messageID, rfcID, inReplyTo, "", false)
}

const pstThreadMetadataKey = "pst_thread_key"

// RecordPstEmailHeadersContext repairs accepted PST header facts without
// replacing archived content. A stored ID that differs from rfcID wins, and the
// message keeps its stored facts. ReconcilePstEmailThreadsContext reads the
// durable thread key, so finalization can resume after an interruption.
func (s *Store) RecordPstEmailHeadersContext(
	ctx context.Context, sourceID, messageID int64, rfcID, inReplyTo, threadKey string,
) error {
	threadKey = mime.NormalizeMessageID(threadKey)
	return s.recordEmailHeadersContext(ctx, sourceID, messageID, rfcID, inReplyTo, threadKey, true)
}

func (s *Store) recordEmailHeadersContext(
	ctx context.Context, sourceID, messageID int64, rfcID, inReplyTo, threadKey string, pst bool,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	rfcID, inReplyTo = mime.NormalizeMessageID(rfcID), mime.NormalizeMessageID(inReplyTo)
	if rfcID == "" && inReplyTo == "" && threadKey == "" {
		return nil
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockEmailHeaderRow(ctx, tx, messageID); err != nil {
			return err
		}
		var storedID, metadata sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT rfc822_message_id, metadata FROM messages
   WHERE id = ? AND source_id = ? AND message_type = 'email'`+s.dialect.SelectForUpdate(),
			messageID, sourceID).Scan(&storedID, &metadata); err != nil {
			return fmt.Errorf("read email header target: %w", err)
		}
		if pst && storedID.String != "" && rfcID != "" && mime.NormalizeMessageID(storedID.String) != rfcID {
			return nil
		}
		if inReplyTo != "" || threadKey != "" {
			encoded, changed, err := mergeEmailHeaderMetadata(metadata, inReplyTo, threadKey)
			if err != nil {
				return fmt.Errorf("message %d: %w", messageID, err)
			}
			if changed {
				if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE messages SET metadata = %s WHERE id = ?`,
					s.dialect.JSONBindExpr()), encoded, messageID); err != nil {
					return err
				}
			}
		}
		if storedID.String != "" || rfcID == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET rfc822_message_id = ? WHERE id = ?`, rfcID, messageID); err != nil {
			return err
		}
		return s.bumpDerivedDataRevisionContext(ctx, tx)
	})
}

// mergeEmailHeaderMetadata adds header facts that metadata does not already
// hold. A thread key is dropped when the stored parent differs from inReplyTo,
// because the key would then describe a different reply chain.
func mergeEmailHeaderMetadata(metadata sql.NullString, inReplyTo, threadKey string) (string, bool, error) {
	fields := make(map[string]jsontext.Value)
	if metadata.Valid && metadata.String != "" {
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
			return "", false, fmt.Errorf("decode email metadata: %w", err)
		}
		if fields == nil {
			fields = make(map[string]jsontext.Value)
		}
	}
	if value, ok := fields[emailReplyMetadataKey]; ok && inReplyTo != "" && threadKey != "" {
		var storedParent string
		if err := json.Unmarshal(value, &storedParent); err != nil {
			return "", false, fmt.Errorf("decode email parent ID: %w", err)
		}
		if mime.NormalizeMessageID(storedParent) != inReplyTo {
			threadKey = ""
		}
	}
	changed := false
	for _, fact := range []struct{ key, value string }{
		{emailReplyMetadataKey, inReplyTo}, {pstThreadMetadataKey, threadKey},
	} {
		if _, exists := fields[fact.key]; fact.value == "" || exists {
			continue
		}
		value, err := json.Marshal(fact.value, json.Deterministic(true))
		if err != nil {
			return "", false, err
		}
		fields[fact.key] = value
		changed = true
	}
	if !changed {
		return "", false, nil
	}
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return "", false, err
	}
	return string(encoded), true, nil
}

func (s *Store) lockEmailHeaderRow(ctx context.Context, tx *loggedTx, messageID int64) error {
	// A completed generation fence already reserves SQLite's writer slot for
	// this transaction. Avoid rewriting the content_changed_at index merely
	// to acquire that same reservation again.
	if s.dialect.DriverName() == sqliteutil.DriverName() && tx.syncGenerationFenced {
		return nil
	}
	// Acquire the SQLite writer slot before reading. This column is outside the
	// last-modified trigger, so an idempotent repair does not change message facts.
	if query := s.dialect.RowWriterLockSQL("messages", "content_changed_at"); query != "" {
		if _, err := tx.ExecContext(ctx, query, messageID); err != nil {
			return fmt.Errorf("lock email header row: %w", err)
		}
	}
	return nil
}

// addEmailReplyFactTx adds a parent ID to a source's email whose metadata
// names no parent yet, and reports whether it wrote. Unreadable metadata stays
// as stored: the fact is optional, and rewriting a damaged payload would
// discard what it holds.
func (s *Store) addEmailReplyFactTx(ctx context.Context, tx *loggedTx, sourceID, messageID int64, inReplyTo string) (bool, error) {
	inReplyTo = mime.NormalizeMessageID(inReplyTo)
	if inReplyTo == "" {
		return false, nil
	}
	var metadata sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT metadata FROM messages
   WHERE id = ? AND source_id = ? AND message_type = 'email'`+s.dialect.SelectForUpdate(),
		messageID, sourceID).Scan(&metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read email metadata: %w", err)
	}
	encoded, changed, err := mergeEmailHeaderMetadata(metadata, inReplyTo, "")
	if err != nil || !changed {
		return false, nil //nolint:nilerr // Unreadable metadata is preserved without the optional fact.
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE messages SET metadata = %s WHERE id = ?`,
		s.dialect.JSONBindExpr()), encoded, messageID); err != nil {
		return false, fmt.Errorf("record email reply header: %w", err)
	}
	return true, nil
}

// EmailReplyBackfillPage reports one page of BackfillEmailReplyHeadersContext.
type EmailReplyBackfillPage struct {
	// LastID is the last message read, or zero when no message remains.
	LastID int64
	// Scanned counts messages read. Recorded counts parents added to metadata.
	// Undecodable counts stored payloads whose headers could not be read.
	Scanned, Recorded, Undecodable int
}

// BackfillEmailReplyHeadersContext reads the In-Reply-To header of up to one
// page of a source's archived emails with id > afterID whose metadata names no
// parent, and records the parents it finds in one transaction. It reads only a
// bounded prefix of each stored payload, never bodies or attachments.
func (s *Store) BackfillEmailReplyHeadersContext(ctx context.Context, sourceID, afterID int64) (EmailReplyBackfillPage, error) {
	var page EmailReplyBackfillPage
	if err := s.requireSyncSource(sourceID); err != nil {
		return page, err
	}
	type fact struct {
		id     int64
		parent string
	}
	var facts []fact
	err := func() error {
		rows, err := s.db.QueryContext(ctx, `SELECT m.id, `+s.dialect.BlobPrefixSQL("r.raw_data")+`, r.compression
  FROM messages m JOIN message_raw r ON r.message_id = m.id
  WHERE m.source_id = ? AND m.id > ? AND m.message_type = 'email'
   AND m.deleted_at IS NULL AND r.raw_format = 'mime'
   AND (m.metadata IS NULL OR CAST(m.metadata AS TEXT) NOT LIKE '%"email_in_reply_to"%')
  ORDER BY m.id LIMIT ?`, accountHeaderRawPrefix, sourceID, afterID, emailReplyBatchSize)
		if err != nil {
			return fmt.Errorf("list archived email headers: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var prefix []byte
			var compression sql.NullString
			if err := rows.Scan(&page.LastID, &prefix, &compression); err != nil {
				return err
			}
			page.Scanned++
			block, err := decodeMessageRawHeaderBounded(prefix, compression, emailattribution.MaxHeaderBytes)
			if err != nil {
				page.Undecodable++
				continue
			}
			if parent := mime.ReplyParentFromHeaders(block); parent != "" {
				facts = append(facts, fact{page.LastID, parent})
			}
		}
		return rows.Err()
	}()
	if err != nil || len(facts) == 0 {
		return page, err
	}
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		page.Recorded = 0
		if err := s.lockEmailHeaderRow(ctx, tx, facts[0].id); err != nil {
			return err
		}
		for _, f := range facts {
			recorded, err := s.addEmailReplyFactTx(ctx, tx, sourceID, f.id, f.parent)
			if err != nil {
				return err
			}
			if recorded {
				page.Recorded++
			}
		}
		return nil
	})
	if err != nil {
		page.Recorded = 0
	}
	return page, err
}

// ResolveEmailReplyParentsContext links unique archived parents in bounded
// pages. checkpoint runs after a page's writes commit; repeating a page after
// interruption is safe because existing links are never overwritten.
func (s *Store) ResolveEmailReplyParentsContext(ctx context.Context, sourceID, afterID int64, checkpoint func(int64) error) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	for {
		ids, err := s.unresolvedEmailReplyPage(ctx, sourceID, afterID)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return ctx.Err()
		}
		if err := s.resolveEmailReplies(ctx, sourceID, ids); err != nil {
			return err
		}
		afterID = ids[len(ids)-1]
		if checkpoint != nil {
			if err := checkpoint(afterID); err != nil {
				return fmt.Errorf("checkpoint email replies: %w", err)
			}
		}
	}
}

// LinkEmailRepliesSinceContext links replies around the messages a source
// archived with afterID < id <= throughID. Replies in that range look up their
// parents. Older unresolved replies are checked only in the conversations of
// new messages that carry a Message-ID, so a parent archived after its reply
// is linked without rereading the rest of the source. A late parent filed in a
// different conversation from its reply stays unlinked until
// ResolveEmailReplyParentsContext walks the whole source again.
func (s *Store) LinkEmailRepliesSinceContext(ctx context.Context, sourceID, afterID, throughID int64) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if throughID <= afterID {
		return nil
	}
	if err := s.ResolveEmailReplyParentsContext(ctx, sourceID, afterID, nil); err != nil || afterID == 0 {
		return err
	}
	seen := make(map[int64]struct{})
	for cursor := afterID; ; {
		conversations, last, err := s.emailParentConversationPage(ctx, sourceID, cursor, throughID)
		if err != nil {
			return err
		}
		if last == 0 {
			return ctx.Err()
		}
		cursor = last
		fresh := conversations[:0]
		for _, id := range conversations {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				fresh = append(fresh, id)
			}
		}
		if len(fresh) == 0 {
			continue
		}
		children, err := s.unresolvedEmailRepliesIn(ctx, sourceID, afterID, fresh)
		if err != nil {
			return err
		}
		for start := 0; start < len(children); start += emailReplyBatchSize {
			if err := s.resolveEmailReplies(ctx, sourceID, children[start:min(start+emailReplyBatchSize, len(children))]); err != nil {
				return err
			}
		}
	}
}

// LatestSourceMessageIDContext returns the highest message ID a source holds,
// or zero when it holds none.
func (s *Store) LatestSourceMessageIDContext(ctx context.Context, sourceID int64) (int64, error) {
	var id sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM messages WHERE source_id = ?`, sourceID).Scan(&id); err != nil {
		return 0, fmt.Errorf("read latest message of source %d: %w", sourceID, err)
	}
	return id.Int64, nil
}

// emailParentConversationPage returns the conversations of up to one page of
// messages with afterID < id <= throughID that can be a reply's parent, and
// the last message ID read; zero means the range is exhausted.
func (s *Store) emailParentConversationPage(ctx context.Context, sourceID, afterID, throughID int64) ([]int64, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, conversation_id FROM messages
  WHERE source_id = ? AND id > ? AND id <= ? AND message_type = 'email'
   AND deleted_at IS NULL AND rfc822_message_id IS NOT NULL
  ORDER BY id LIMIT ?`, sourceID, afterID, throughID, emailReplyBatchSize)
	if err != nil {
		return nil, 0, fmt.Errorf("list new email parents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var conversations []int64
	var last int64
	for rows.Next() {
		var conversation sql.NullInt64
		if err := rows.Scan(&last, &conversation); err != nil {
			return nil, 0, err
		}
		if conversation.Valid {
			conversations = append(conversations, conversation.Int64)
		}
	}
	return conversations, last, rows.Err()
}

// unresolvedEmailRepliesIn lists unresolved replies with id <= throughID in
// the given conversations, using the conversation index.
func (s *Store) unresolvedEmailRepliesIn(ctx context.Context, sourceID, throughID int64, conversations []int64) ([]int64, error) {
	args := make([]any, 0, len(conversations)+2)
	for _, id := range conversations {
		args = append(args, id)
	}
	args = append(args, sourceID, throughID)
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM messages
  WHERE conversation_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(conversations)), ",")+`)
   AND source_id = ? AND id <= ? AND message_type = 'email'
   AND reply_to_message_id IS NULL AND deleted_at IS NULL
   AND CAST(metadata AS TEXT) LIKE '%"email_in_reply_to"%'
  ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list unresolved email replies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) unresolvedEmailReplyPage(ctx context.Context, sourceID, afterID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM messages
  WHERE source_id = ? AND message_type = 'email' AND id > ?
   AND reply_to_message_id IS NULL AND deleted_at IS NULL
   AND CAST(metadata AS TEXT) LIKE '%"email_in_reply_to"%'
  ORDER BY id LIMIT ?`, sourceID, afterID, emailReplyBatchSize)
	if err != nil {
		return nil, fmt.Errorf("list email replies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// resolveEmailReplies links each child in ids to its unique parent. A read
// selects the candidates first, so children whose parent is missing or
// ambiguous cost no write; one transaction then checks and links the rest.
func (s *Store) resolveEmailReplies(ctx context.Context, sourceID int64, ids []int64) error {
	var candidates []int64
	for _, id := range ids {
		parent, err := s.uniqueEmailReplyParent(ctx, s.db, sourceID, id, "")
		if err != nil {
			return err
		}
		if parent != 0 {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return ctx.Err()
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		for _, id := range candidates {
			if err := s.lockEmailHeaderRow(ctx, tx, id); err != nil {
				return err
			}
			parent, err := s.uniqueEmailReplyParent(ctx, tx, sourceID, id, s.dialect.SelectForUpdate())
			if err != nil {
				return err
			}
			if parent == 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET reply_to_message_id = ?
   WHERE id = ? AND source_id = ? AND reply_to_message_id IS NULL`, parent, id, sourceID); err != nil {
				return err
			}
		}
		return nil
	})
}

// uniqueEmailReplyParent returns the one visible same-source email whose
// Message-ID the unresolved child names, or zero when there is none, more
// than one, or the child names itself. lock is appended to the child read.
func (s *Store) uniqueEmailReplyParent(ctx context.Context, q storeQuerier, sourceID, childID int64, lock string) (int64, error) {
	var metadata sql.NullString
	var reply sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT metadata, reply_to_message_id FROM messages
   WHERE id = ? AND source_id = ? AND message_type = 'email' AND deleted_at IS NULL`+lock, childID, sourceID).Scan(&metadata, &reply)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if reply.Valid || !metadata.Valid {
		return 0, nil
	}
	parentRFCID := mime.NormalizeMessageID(storedEmailReplyParent(metadata.String))
	if parentRFCID == "" {
		return 0, nil
	}
	// The SQLite canonical index compares BLOB bytes; PostgreSQL compares TEXT.
	var canonical any = parentRFCID
	if !s.IsPostgreSQL() {
		canonical = []byte(parentRFCID)
	}
	rows, err := q.QueryContext(ctx, fmt.Sprintf(`SELECT id FROM messages
   WHERE %s = ? AND source_id = ? AND message_type = 'email' AND deleted_at IS NULL
   ORDER BY id LIMIT 2`, s.dialect.RFC822CanonicalIDExpr("rfc822_message_id")), canonical, sourceID)
	if err != nil {
		return 0, fmt.Errorf("find email parent: %w", err)
	}
	var parents []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		parents = append(parents, id)
	}
	rowErr := rows.Err()
	closeErr := rows.Close()
	if rowErr != nil {
		return 0, rowErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if len(parents) != 1 || parents[0] == childID {
		return 0, nil
	}
	return parents[0], nil
}

// storedEmailReplyParent reads the parent Message-ID fact from message
// metadata. A damaged fact names no parent: failing instead would stop every
// later sync from linking the rest of the source.
func storedEmailReplyParent(metadata string) string {
	var fields struct {
		Parent string `json:"email_in_reply_to"`
	}
	if err := json.Unmarshal([]byte(metadata), &fields); err != nil {
		return ""
	}
	return fields.Parent
}
