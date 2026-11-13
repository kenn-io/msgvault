package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

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
		if err := s.linkEmailRepliesToParentTx(ctx, tx, sourceID, messageID); err != nil {
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
// bounded prefix of each stored payload, never bodies or attachments. Emails
// that dedup hid are read too: the backfill runs once, and UndoDedup can
// restore them later.
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
   AND r.raw_format = 'mime'
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

// EmailReplyLinkBoundContext returns an instant that every message change
// still in flight stamps at or after, so a pass that reads up to it and the
// next pass that starts from it together skip no committed change.
func (s *Store) EmailReplyLinkBoundContext(ctx context.Context) (time.Time, error) {
	bounds, err := s.dialect.ReadWatermarkBounds(ctx, s.db.DB)
	if err != nil {
		return time.Time{}, fmt.Errorf("read email reply link bound: %w", err)
	}
	return bounds.CommitBound, nil
}

// LinkChangedEmailRepliesContext links replies around the emails of a source
// whose content_changed_at is in [since, through]. Every write that is still
// in flight at a bound from EmailReplyLinkBoundContext stamps at or after it,
// so a caller that resumes from through misses nothing and repeats only the
// rows stamped at that instant, which is safe. The stamp moves on insert
// and whenever metadata, conversation_id or deleted_at change, so the range
// covers new mail, parents recorded on older rows, and deletions that free an
// ID for reuse. Changed replies look up their parents. Older unresolved
// replies are checked only in the conversations of changed messages that
// carry a Message-ID, so a parent archived after its reply is linked without
// rereading the rest of the source. A late parent filed in a different
// conversation from its reply stays unlinked until
// ResolveEmailReplyParentsContext walks the whole source again.
func (s *Store) LinkChangedEmailRepliesContext(ctx context.Context, sourceID int64, since, through time.Time) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if through.Before(since) {
		return nil
	}
	seen := make(map[int64]struct{})
	var cursor *changedEmailCursor
	for {
		page, err := s.readChangedEmailPage(ctx, sourceID, since, through, cursor)
		if err != nil {
			return err
		}
		if len(page.ids) == 0 {
			return ctx.Err()
		}
		cursor = &page.last
		if err := s.resolveEmailReplies(ctx, sourceID, page.replies); err != nil {
			return err
		}
		fresh := page.conversations[:0]
		for _, id := range page.conversations {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				fresh = append(fresh, id)
			}
		}
		if len(fresh) == 0 {
			continue
		}
		children, err := s.unresolvedEmailRepliesIn(ctx, s.db, sourceID, fresh)
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

// changedEmailCursor is the (content_changed_at, id) of the last row a page
// read; the next page starts after it.
type changedEmailCursor struct {
	at time.Time
	id int64
}

type changedEmailPage struct {
	ids []int64
	// replies are the unresolved replies among the page's rows.
	replies []int64
	// conversations hold the page's rows that carry a Message-ID, so they can
	// be a waiting reply's parent. Deleted rows count: removing one of two
	// parents with the same ID can leave the other one unique.
	conversations []int64
	last          changedEmailCursor
}

// readChangedEmailPage reads one page of a source's emails whose content
// changed in [since, through], in (content_changed_at, id) order.
func (s *Store) readChangedEmailPage(
	ctx context.Context, sourceID int64, since, through time.Time, after *changedEmailCursor,
) (changedEmailPage, error) {
	var page changedEmailPage
	// The source_id index would otherwise win and read every row of the source.
	index := ""
	if !s.IsPostgreSQL() {
		index = " INDEXED BY idx_messages_content_changed_at"
	}
	query := `SELECT id, content_changed_at,
	   CASE WHEN COALESCE(rfc822_message_id, '') <> '' THEN conversation_id END,
	   CASE WHEN reply_to_message_id IS NULL AND deleted_at IS NULL
	         AND CAST(metadata AS TEXT) LIKE '%"email_in_reply_to"%' THEN 1 ELSE 0 END
	  FROM messages` + index + `
	  WHERE content_changed_at >= ?`
	args := []any{s.dialect.TimestampParam(since)}
	if after != nil {
		at := s.dialect.TimestampParam(after.at)
		query += ` AND (content_changed_at > ? OR id > ?)`
		args = []any{at, at, after.id}
	}
	query += ` AND content_changed_at <= ? AND source_id = ? AND message_type = 'email'
	  ORDER BY content_changed_at, id LIMIT ?`
	args = append(args, s.dialect.TimestampParam(through), sourceID, emailReplyBatchSize)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, fmt.Errorf("list changed emails: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var at requiredTimestamp
		var conversation sql.NullInt64
		var unresolved int
		if err := rows.Scan(&id, &at, &conversation, &unresolved); err != nil {
			return page, err
		}
		page.ids = append(page.ids, id)
		page.last = changedEmailCursor{at: at.Time, id: id}
		if unresolved == 1 {
			page.replies = append(page.replies, id)
		}
		if conversation.Valid {
			page.conversations = append(page.conversations, conversation.Int64)
		}
	}
	return page, rows.Err()
}

// unresolvedEmailRepliesIn lists unresolved replies in the given
// conversations, using the conversation index.
func (s *Store) unresolvedEmailRepliesIn(ctx context.Context, q storeQuerier, sourceID int64, conversations []int64) ([]int64, error) {
	args := make([]any, 0, len(conversations)+1)
	for _, id := range conversations {
		args = append(args, id)
	}
	args = append(args, sourceID)
	rows, err := q.QueryContext(ctx, `SELECT id FROM messages
  WHERE conversation_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(conversations)), ",")+`)
   AND source_id = ? AND message_type = 'email'
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

// emailReplyLinkTx is a transaction that can link email replies.
type emailReplyLinkTx interface {
	storeQuerier
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// connTx runs statements on a connection whose transaction began with
// explicit SQL, so helpers written for loggedTx can join that transaction.
type connTx struct {
	conn   *sql.Conn
	rebind func(string) string
}

func (c connTx) QueryContext(ctx context.Context, query string, args ...any) (*loggedRows, error) {
	query = c.rebind(query)
	start := time.Now()
	rows, err := c.conn.QueryContext(ctx, query, args...) //nolint:rowserrcheck // caller owns rows.Err
	if err != nil {
		return nil, err
	}
	return &loggedRows{Rows: rows, query: query, args: args, reqID: RequestIDFromContext(ctx), start: start}, nil
}

func (c connTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return c.conn.QueryRowContext(ctx, c.rebind(query), args...)
}

func (c connTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return c.conn.ExecContext(ctx, c.rebind(query), args...)
}

// linkEmailRepliesToParentTx links the unresolved replies in an email's
// conversation after the email gains a Message-ID. A rfc822_message_id write
// leaves content_changed_at alone, so LinkChangedEmailRepliesContext would not
// see the new parent.
func (s *Store) linkEmailRepliesToParentTx(ctx context.Context, tx emailReplyLinkTx, sourceID, parentID int64) error {
	var conversation sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM messages
   WHERE id = ? AND source_id = ? AND message_type = 'email'`, parentID, sourceID).Scan(&conversation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read email parent conversation: %w", err)
	}
	if !conversation.Valid {
		return nil
	}
	children, err := s.unresolvedEmailRepliesIn(ctx, tx, sourceID, []int64{conversation.Int64})
	if err != nil {
		return err
	}
	for _, child := range children {
		parent, err := s.uniqueEmailReplyParent(ctx, tx, sourceID, child, s.dialect.SelectForUpdate())
		if err != nil {
			return err
		}
		if parent == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET reply_to_message_id = ?
   WHERE id = ? AND source_id = ? AND reply_to_message_id IS NULL`, parent, child, sourceID); err != nil {
			return fmt.Errorf("link email reply %d: %w", child, err)
		}
	}
	return nil
}

// emailMessageIDKey names a Message-ID within one source, in the canonical
// form the parent lookup compares.
type emailMessageIDKey struct {
	sourceID  int64
	canonical string
}

// emailMessageIDKeysTx lists the distinct Message-IDs of the emails that match
// where, an SQL condition on the unaliased messages table.
func (s *Store) emailMessageIDKeysTx(ctx context.Context, tx emailReplyLinkTx, where string) ([]emailMessageIDKey, error) {
	rows, err := tx.QueryContext(ctx, `SELECT source_id, `+s.dialect.RFC822CanonicalIDExpr("rfc822_message_id")+`
   FROM messages WHERE message_type = 'email' AND COALESCE(rfc822_message_id, '') <> '' AND (`+where+`)`)
	if err != nil {
		return nil, fmt.Errorf("list email Message-IDs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[emailMessageIDKey]struct{})
	var keys []emailMessageIDKey
	for rows.Next() {
		var key emailMessageIDKey
		var canonical []byte
		if err := rows.Scan(&key.sourceID, &canonical); err != nil {
			return nil, err
		}
		key.canonical = string(canonical)
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	return keys, rows.Err()
}

// linkEmailRepliesToMessageIDsTx links the waiting replies of every live email
// that carries one of keys. A write that removes one of two copies of a
// parent calls it after the removal, when the other copy can be unique.
func (s *Store) linkEmailRepliesToMessageIDsTx(ctx context.Context, tx emailReplyLinkTx, keys []emailMessageIDKey) error {
	for _, key := range keys {
		var canonical any = key.canonical
		if !s.IsPostgreSQL() {
			canonical = []byte(key.canonical)
		}
		ids, err := func() ([]int64, error) {
			rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT id FROM messages
   WHERE %s = ? AND source_id = ? AND message_type = 'email' AND deleted_at IS NULL
   ORDER BY id`, s.dialect.RFC822CanonicalIDExpr("rfc822_message_id")), canonical, key.sourceID)
			if err != nil {
				return nil, fmt.Errorf("find emails with Message-ID: %w", err)
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
		}()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := s.linkEmailRepliesToParentTx(ctx, tx, key.sourceID, id); err != nil {
				return err
			}
		}
	}
	return nil
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
