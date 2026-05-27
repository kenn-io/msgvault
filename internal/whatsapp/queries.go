package whatsapp

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
)

// hasColumn checks whether a table has a given column using PRAGMA table_info.
// Results are cached per (db pointer, table) to avoid repeated PRAGMA queries
// while correctly handling multiple databases with different schemas.
var (
	columnCache   = make(map[columnCacheKey]map[string]bool)
	columnCacheMu sync.Mutex
)

type columnCacheKey struct {
	db    *sql.DB
	table string
}

func hasColumn(db *sql.DB, table, column string) bool {
	columnCacheMu.Lock()
	defer columnCacheMu.Unlock()

	key := columnCacheKey{db: db, table: table}
	cols, ok := columnCache[key]
	if !ok {
		cols = make(map[string]bool)
		rows, err := db.Query(
			fmt.Sprintf("PRAGMA table_info(%s)", table),
		)
		if err == nil {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var cid int
				var name, colType string
				var notNull, pk int
				var dfltValue sql.NullString
				if err := rows.Scan(
					&cid, &name, &colType,
					&notNull, &dfltValue, &pk,
				); err == nil {
					cols[name] = true
				}
			}
			err = rows.Err()
		}
		// Only cache a fully-read column set; on query or iteration error
		// re-probe next time rather than memoizing a partial result.
		if err == nil {
			columnCache[key] = cols
		}
	}
	return cols[column]
}

// resetColumnCache clears the cached column info (for testing).
func resetColumnCache() {
	columnCacheMu.Lock()
	defer columnCacheMu.Unlock()
	columnCache = make(map[columnCacheKey]map[string]bool)
}

// fetchChats returns all non-hidden chats from the WhatsApp database.
// Joins with the jid table to get JID details for each chat.
// Handles old WhatsApp schemas that lack the group_type column.
func fetchChats(db *sql.DB) ([]waChat, error) {
	hasGroupType := hasColumn(db, "chat", "group_type")

	groupTypeExpr := "0"
	if hasGroupType {
		groupTypeExpr = "COALESCE(c.group_type, 0)"
	}

	rows, err := db.Query(fmt.Sprintf(`
		SELECT
			c._id,
			c.jid_row_id,
			j.raw_string,
			COALESCE(j.user, ''),
			COALESCE(j.server, ''),
			c.subject,
			%s,
			COALESCE(c.hidden, 0),
			COALESCE(c.sort_timestamp, 0)
		FROM chat c
		JOIN jid j ON c.jid_row_id = j._id
		WHERE COALESCE(c.hidden, 0) = 0
		ORDER BY c.sort_timestamp DESC
	`, groupTypeExpr))
	if err != nil {
		return nil, fmt.Errorf("fetch chats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var chats []waChat
	for rows.Next() {
		var c waChat
		if err := rows.Scan(
			&c.RowID, &c.JIDRowID, &c.RawString, &c.User, &c.Server,
			&c.Subject, &c.GroupType, &c.Hidden,
			&c.LastMessageTimestamp,
		); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// fetchMessages returns messages for a chat, batched after a given _id.
// Messages are ordered by _id ascending for deterministic resumability.
// Joins with jid to resolve sender information.
func fetchMessages(db *sql.DB, chatRowID int64, afterID int64, limit int) ([]waMessage, error) {
	rows, err := db.Query(`
		SELECT
			m._id,
			m.chat_row_id,
			COALESCE(m.from_me, 0),
			COALESCE(m.key_id, ''),
			m.sender_jid_row_id,
			sj.raw_string,
			sj.user,
			sj.server,
			COALESCE(m.timestamp, 0),
			COALESCE(m.message_type, 0),
			m.text_data,
			COALESCE(m.status, 0),
			COALESCE(m.starred, 0)
		FROM message m
		LEFT JOIN jid sj ON m.sender_jid_row_id = sj._id
		WHERE m.chat_row_id = ?
		  AND m._id > ?
		ORDER BY m._id ASC
		LIMIT ?
	`, chatRowID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("fetch messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []waMessage
	for rows.Next() {
		var m waMessage
		if err := rows.Scan(
			&m.RowID, &m.ChatRowID, &m.FromMe, &m.KeyID,
			&m.SenderJIDRowID, &m.SenderRawString, &m.SenderUser, &m.SenderServer,
			&m.Timestamp, &m.MessageType, &m.TextData,
			&m.Status, &m.Starred,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// fetchMedia returns media metadata for a batch of message row IDs.
// Returns a map of message_row_id → waMedia.
// Handles old WhatsApp schemas that lack the media_caption column.
func fetchMedia(db *sql.DB, messageRowIDs []int64) (map[int64]waMedia, error) {
	if len(messageRowIDs) == 0 {
		return make(map[int64]waMedia), nil
	}

	hasCaption := hasColumn(db, "message_media", "media_caption")

	result := make(map[int64]waMedia)

	// Process in chunks to stay within SQLite's parameter limit.
	const chunkSize = 500
	for i := 0; i < len(messageRowIDs); i += chunkSize {
		end := min(i+chunkSize, len(messageRowIDs))
		chunk := messageRowIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for j, id := range chunk {
			placeholders[j] = "?"
			args[j] = id
		}

		captionExpr := "NULL"
		if hasCaption {
			captionExpr = "mm.media_caption"
		}

		query := fmt.Sprintf(`
			SELECT
				mm.message_row_id,
				mm.mime_type,
				%s,
				mm.file_size,
				mm.file_path,
				mm.width,
				mm.height,
				mm.media_duration
			FROM message_media mm
			WHERE mm.message_row_id IN (%s)
		`, captionExpr, strings.Join(placeholders, ","))

		rows, err := db.Query(query, args...)
		if err != nil {
			return nil, fmt.Errorf("fetch media: %w", err)
		}
		if err := scanMediaChunk(rows, result); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// scanMediaChunk scans one media query's rows into result and closes the rows.
// Taking ownership of a single *sql.Rows here keeps the deferred Close scoped
// to one query so it cannot leak across the caller's chunk loop.
func scanMediaChunk(rows *sql.Rows, result map[int64]waMedia) error {
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var m waMedia
		if err := rows.Scan(
			&m.MessageRowID, &m.MimeType, &m.MediaCaption,
			&m.FileSize, &m.FilePath, &m.Width, &m.Height,
			&m.MediaDuration,
		); err != nil {
			return fmt.Errorf("scan media: %w", err)
		}
		result[m.MessageRowID] = m
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate media: %w", err)
	}
	return nil
}

// fetchReactions returns reactions for a batch of message row IDs.
// Returns a map of message_row_id → []waReaction.
func fetchReactions(db *sql.DB, messageRowIDs []int64) (map[int64][]waReaction, error) {
	if len(messageRowIDs) == 0 {
		return make(map[int64][]waReaction), nil
	}

	result := make(map[int64][]waReaction)

	const chunkSize = 500
	for i := 0; i < len(messageRowIDs); i += chunkSize {
		end := min(i+chunkSize, len(messageRowIDs))
		chunk := messageRowIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for j, id := range chunk {
			placeholders[j] = "?"
			args[j] = id
		}

		// WhatsApp stores reactions in message_add_on (metadata) joined with
		// message_add_on_reaction (the actual emoji). The link to the original
		// message is via parent_message_row_id.
		query := fmt.Sprintf(`
			SELECT
				ao.parent_message_row_id,
				ao.sender_jid_row_id,
				sj.raw_string,
				sj.user,
				sj.server,
				ar.reaction,
				COALESCE(ar.sender_timestamp, 0)
			FROM message_add_on ao
			JOIN message_add_on_reaction ar ON ar.message_add_on_row_id = ao._id
			LEFT JOIN jid sj ON ao.sender_jid_row_id = sj._id
			WHERE ao.parent_message_row_id IN (%s)
			  AND ar.reaction IS NOT NULL
			  AND ar.reaction != ''
		`, strings.Join(placeholders, ","))

		rows, err := db.Query(query, args...)
		if err != nil {
			// Table might not exist in older DB versions
			if isTableNotFound(err) {
				return result, nil
			}
			return nil, fmt.Errorf("fetch reactions: %w", err)
		}

		if err := scanReactionChunk(rows, result); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// scanReactionChunk scans one reaction query's rows into result and closes the
// rows. Owning a single *sql.Rows here scopes the deferred Close to one query
// so it cannot leak across the caller's chunk loop.
func scanReactionChunk(rows *sql.Rows, result map[int64][]waReaction) error {
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var r waReaction
		if err := rows.Scan(
			&r.MessageRowID, &r.SenderJIDRowID,
			&r.SenderRawString, &r.SenderUser, &r.SenderServer,
			&r.ReactionValue, &r.Timestamp,
		); err != nil {
			return fmt.Errorf("scan reaction: %w", err)
		}
		result[r.MessageRowID] = append(result[r.MessageRowID], r)
	}
	return rows.Err()
}

// fetchGroupParticipants returns all participants of a group chat.
// In the WhatsApp schema, group_participants.gjid and .jid are TEXT fields
// containing raw JID strings (e.g., "447700900000@s.whatsapp.net"),
// not integer row IDs.
func fetchGroupParticipants(db *sql.DB, groupJIDRawString string) ([]waGroupMember, error) {
	rows, err := db.Query(`
		SELECT
			gp.gjid,
			gp.jid,
			COALESCE(j.user, ''),
			COALESCE(j.server, ''),
			COALESCE(gp.admin, 0)
		FROM group_participants gp
		LEFT JOIN jid j ON gp.jid = j.raw_string
		WHERE gp.gjid = ?
	`, groupJIDRawString)
	if err != nil {
		return nil, fmt.Errorf("fetch group participants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var members []waGroupMember
	for rows.Next() {
		var m waGroupMember
		if err := rows.Scan(
			&m.GroupJID, &m.MemberJID,
			&m.MemberUser, &m.MemberServer, &m.Admin,
		); err != nil {
			return nil, fmt.Errorf("scan group participant: %w", err)
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// fetchQuotedMessages returns quoted message references for a batch of message row IDs.
// Returns a map of message_row_id → waQuoted (the message that contains the quote).
func fetchQuotedMessages(db *sql.DB, messageRowIDs []int64) (map[int64]waQuoted, error) {
	if len(messageRowIDs) == 0 {
		return make(map[int64]waQuoted), nil
	}

	result := make(map[int64]waQuoted)

	const chunkSize = 500
	for i := 0; i < len(messageRowIDs); i += chunkSize {
		end := min(i+chunkSize, len(messageRowIDs))
		chunk := messageRowIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for j, id := range chunk {
			placeholders[j] = "?"
			args[j] = id
		}

		query := fmt.Sprintf(`
			SELECT
				mq.message_row_id,
				mq.key_id
			FROM message_quoted mq
			WHERE mq.message_row_id IN (%s)
			  AND mq.key_id IS NOT NULL
			  AND mq.key_id != ''
		`, strings.Join(placeholders, ","))

		rows, err := db.Query(query, args...)
		if err != nil {
			if isTableNotFound(err) {
				return result, nil
			}
			return nil, fmt.Errorf("fetch quoted messages: %w", err)
		}

		if err := scanQuotedChunk(rows, result); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// scanQuotedChunk scans one quoted-message query's rows into result and closes
// the rows. Owning a single *sql.Rows here scopes the deferred Close to one
// query so it cannot leak across the caller's chunk loop.
func scanQuotedChunk(rows *sql.Rows, result map[int64]waQuoted) error {
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var q waQuoted
		if err := rows.Scan(&q.MessageRowID, &q.QuotedKeyID); err != nil {
			return fmt.Errorf("scan quoted message: %w", err)
		}
		result[q.MessageRowID] = q
	}
	return rows.Err()
}

// fetchLidMap reads the WhatsApp jid_map table to build a mapping from
// lid JID row IDs to their corresponding phone JIDs. The jid_map table
// links two jid rows: one with server="lid" and one with the real phone
// (server="s.whatsapp.net"). Returns an empty map gracefully if the
// jid_map table doesn't exist (older WhatsApp DB versions).
func fetchLidMap(db *sql.DB) (map[int64]waLidMapping, error) {
	result := make(map[int64]waLidMapping)

	rows, err := db.Query(`
		SELECT
			jm.lid_row_id,
			COALESCE(phone_jid.user, ''),
			COALESCE(phone_jid.server, '')
		FROM jid_map jm
		JOIN jid phone_jid ON jm.jid_row_id = phone_jid._id
	`)
	if err != nil {
		if isTableNotFound(err) {
			return result, nil
		}
		return nil, fmt.Errorf("fetch lid map: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var m waLidMapping
		if err := rows.Scan(&m.LidRowID, &m.PhoneUser, &m.PhoneServer); err != nil {
			return nil, fmt.Errorf("scan lid mapping: %w", err)
		}
		result[m.LidRowID] = m
	}
	return result, rows.Err()
}

// isTableNotFound returns true if the error indicates a missing table.
func isTableNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}
