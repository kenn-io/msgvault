package signalandroid

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// fetchRecipients loads every recipient keyed by its local row ID.
func fetchRecipients(ctx context.Context, db *sql.DB, s *schema) (map[int64]sigRecipient, error) {
	query := fmt.Sprintf(`
		SELECT r._id, COALESCE(r.%s, ''), COALESCE(r.%s, ''), COALESCE(r.group_id, ''), %s
		FROM recipient r`, s.recipientACI, s.recipientE164, s.recipientName)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query recipients: %w", err)
	}
	defer func() { _ = rows.Close() }()
	recipients := make(map[int64]sigRecipient)
	for rows.Next() {
		var r sigRecipient
		if err := rows.Scan(&r.ID, &r.ACI, &r.E164, &r.GroupID, &r.Name); err != nil {
			return nil, fmt.Errorf("scan recipient: %w", err)
		}
		r.ACI = normalizeACI(r.ACI)
		r.E164 = normalizeE164(r.E164)
		r.Name = strings.TrimSpace(r.Name)
		recipients[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recipients: %w", err)
	}
	return recipients, nil
}

// fetchGroupTitles maps stored group IDs to their titles.
func fetchGroupTitles(ctx context.Context, db *sql.DB, s *schema) (map[string]string, error) {
	titles := make(map[string]string)
	if !s.hasGroups {
		return titles, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT group_id, COALESCE(title, '') FROM groups`)
	if err != nil {
		return nil, fmt.Errorf("query groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var groupID, title string
		if err := rows.Scan(&groupID, &title); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		titles[groupID] = strings.TrimSpace(title)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate groups: %w", err)
	}
	return titles, nil
}

// fetchGroupMembers returns the recipient IDs of a group's current members,
// from group_membership or, in older databases, the groups.members list.
func fetchGroupMembers(ctx context.Context, db *sql.DB, s *schema, groupID string) ([]int64, error) {
	if s.hasGroupMembership {
		rows, err := db.QueryContext(ctx,
			`SELECT recipient_id FROM group_membership WHERE group_id = ? ORDER BY recipient_id`, groupID)
		if err != nil {
			return nil, fmt.Errorf("query group membership: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var members []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scan group member: %w", err)
			}
			members = append(members, id)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate group membership: %w", err)
		}
		return members, nil
	}
	if !s.hasGroups || !s.groupsMembers {
		return nil, nil
	}
	var list sql.NullString
	err := db.QueryRowContext(ctx, `SELECT members FROM groups WHERE group_id = ?`, groupID).Scan(&list)
	if err == sql.ErrNoRows || !list.Valid {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query group members: %w", err)
	}
	var members []int64
	for field := range strings.SplitSeq(list.String, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64); err == nil {
			members = append(members, id)
		}
	}
	return members, nil
}

// fetchThreads lists the chats that hold at least one message.
func fetchThreads(ctx context.Context, db *sql.DB, s *schema) ([]sigThread, error) {
	query := fmt.Sprintf(`
		SELECT t._id, t.%s FROM thread t
		WHERE EXISTS (SELECT 1 FROM message m WHERE m.thread_id = t._id)
		ORDER BY t._id`, s.threadRecipient)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query threads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var threads []sigThread
	for rows.Next() {
		var t sigThread
		var recipient sql.NullInt64
		if err := rows.Scan(&t.ID, &recipient); err != nil {
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		if !recipient.Valid {
			continue
		}
		t.RecipientID = recipient.Int64
		threads = append(threads, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate threads: %w", err)
	}
	return threads, nil
}

// fetchMessages returns up to limit visible messages of a thread after
// afterRowID. A visible message is the latest revision of a message; it
// carries the date and author of its original revision, which is how Signal
// itself identifies the message.
func fetchMessages(ctx context.Context, db *sql.DB, s *schema, threadID, afterRowID int64, limit int) ([]sigMessage, error) {
	query := fmt.Sprintf(`
		SELECT m._id, m.original_message_id,
		       COALESCE(o.from_recipient_id, m.from_recipient_id),
		       COALESCE(o.date_sent, m.date_sent),
		       COALESCE(o.date_received, m.date_received),
		       m.date_sent, m.type, m.body,
		       COALESCE(m.quote_id, 0), COALESCE(m.quote_author, 0),
		       COALESCE(%s, 0), COALESCE(%s, 0), COALESCE(%s, 0),
		       COALESCE(%s, -1), COALESCE(%s, 0), COALESCE(%s, 0)
		FROM message m
		LEFT JOIN message o ON o._id = m.original_message_id
		WHERE m.thread_id = ? AND m.latest_revision_id IS NULL AND m._id > ?
		ORDER BY m._id
		LIMIT ?`,
		s.messageColumn("story_type", "0"),
		s.messageColumn("parent_story_id", "0"),
		s.messageColumn("remote_deleted", "0"),
		s.messageColumn("scheduled_date", "-1"),
		s.messageColumn("view_once", "0"),
		s.messageColumn("revision_number", "0"),
	)
	rows, err := db.QueryContext(ctx, query, threadID, afterRowID, limit)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var messages []sigMessage
	for rows.Next() {
		var m sigMessage
		if err := rows.Scan(&m.RowID, &m.OriginalRowID, &m.AuthorID, &m.DateSent, &m.DateReceived,
			&m.RevisionSent, &m.Type, &m.Body, &m.QuoteID, &m.QuoteAuthor,
			&m.StoryType, &m.ParentStoryID, &m.RemoteDeleted,
			&m.ScheduledDate, &m.ViewOnce, &m.RevisionNumber); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return messages, nil
}

// fetchAttachments returns the attachments of the given message rows keyed
// by message row, in display order. Quote thumbnails are left out: they
// copy an attachment of the quoted message.
func fetchAttachments(ctx context.Context, db *sql.DB, s *schema, messageIDs []int64) (map[int64][]sigAttachment, error) {
	out := make(map[int64][]sigAttachment)
	if s.attachmentTable == "" || len(messageIDs) == 0 {
		return out, nil
	}
	query := fmt.Sprintf(`
		SELECT a._id, a.%s, COALESCE(a.%s, ''), COALESCE(%s, ''), COALESCE(%s, ''),
		       COALESCE(%s, 0), COALESCE(%s, 0), COALESCE(%s, 0),
		       COALESCE(%s, 0), COALESCE(%s, 0), COALESCE(%s, '')
		FROM %s a
		WHERE a.%s IN (%s) AND COALESCE(%s, 0) = 0
		ORDER BY a.%s, COALESCE(%s, 0), a._id`,
		s.attachmentMessage, s.attachmentType,
		s.attachmentColumn("file_name", "''"), s.attachmentColumn("caption", "''"),
		s.attachmentColumn("data_size", "0"), s.attachmentColumn("width", "0"),
		s.attachmentColumn("height", "0"), s.attachmentColumn("voice_note", "0"),
		s.attachmentColumn("video_gif", "0"), s.attachmentColumn("sticker_pack_id", "''"),
		s.attachmentTable, s.attachmentMessage, placeholders(len(messageIDs)),
		s.attachmentColumn("quote", "0"),
		s.attachmentMessage, s.attachmentColumn("display_order", "0"),
	)
	rows, err := db.QueryContext(ctx, query, int64Args(messageIDs)...)
	if err != nil {
		return nil, fmt.Errorf("query attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a sigAttachment
		var voiceNote, videoGIF int64
		if err := rows.Scan(&a.ID, &a.MessageID, &a.ContentType, &a.FileName, &a.Caption,
			&a.DataSize, &a.Width, &a.Height, &voiceNote, &videoGIF, &a.StickerPackID); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		a.VoiceNote = voiceNote != 0
		a.VideoGIF = videoGIF != 0
		out[a.MessageID] = append(out[a.MessageID], a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachments: %w", err)
	}
	return out, nil
}

// fetchReactions returns reactions keyed by the original message row, so a
// reaction recorded against any revision of an edited message is found.
func fetchReactions(ctx context.Context, db *sql.DB, s *schema, rootIDs []int64) (map[int64][]sigReaction, error) {
	out := make(map[int64][]sigReaction)
	if !s.hasReactions || len(rootIDs) == 0 {
		return out, nil
	}
	in := placeholders(len(rootIDs))
	query := fmt.Sprintf(`
		SELECT COALESCE(m.original_message_id, m._id), r.author_id, r.emoji, COALESCE(r.date_sent, 0)
		FROM reaction r
		JOIN message m ON m._id = r.message_id
		WHERE m._id IN (%s) OR m.original_message_id IN (%s)
		ORDER BY r.date_sent, r.rowid`, in, in)
	args := append(int64Args(rootIDs), int64Args(rootIDs)...)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query reactions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r sigReaction
		if err := rows.Scan(&r.RootID, &r.AuthorID, &r.Emoji, &r.DateSent); err != nil {
			return nil, fmt.Errorf("scan reaction: %w", err)
		}
		out[r.RootID] = append(out[r.RootID], r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reactions: %w", err)
	}
	return out, nil
}

// fetchMentions returns mentions keyed by message row.
func fetchMentions(ctx context.Context, db *sql.DB, s *schema, messageIDs []int64) (map[int64][]sigMention, error) {
	out := make(map[int64][]sigMention)
	if !s.hasMentions || len(messageIDs) == 0 {
		return out, nil
	}
	query := fmt.Sprintf(`
		SELECT message_id, recipient_id, COALESCE(range_start, 0)
		FROM mention WHERE message_id IN (%s)
		ORDER BY message_id, range_start`, placeholders(len(messageIDs)))
	rows, err := db.QueryContext(ctx, query, int64Args(messageIDs)...)
	if err != nil {
		return nil, fmt.Errorf("query mentions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var messageID int64
		var m sigMention
		if err := rows.Scan(&messageID, &m.RecipientID, &m.RangeStart); err != nil {
			return nil, fmt.Errorf("scan mention: %w", err)
		}
		out[messageID] = append(out[messageID], m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mentions: %w", err)
	}
	return out, nil
}

// selfRecipientIDs returns the distinct authors of outgoing messages. In
// supported databases that is the account owner alone.
func selfRecipientIDs(ctx context.Context, db *sql.DB) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT from_recipient_id FROM message
		WHERE (type & ?) IN (?, ?, ?, ?)
		GROUP BY from_recipient_id
		ORDER BY COUNT(*) DESC`,
		baseTypeMask, baseOutboxType, baseSendingType, baseSentType, baseSentFailedType)
	if err != nil {
		return nil, fmt.Errorf("query own messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan own message author: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate own message authors: %w", err)
	}
	return ids, nil
}

var attachmentFileName = regexp.MustCompile(`^Attachment_(\d+)_(-?\d+)\.bin$`)

// scanAttachmentFiles maps attachment row IDs to the Attachment_<id>_<unique>.bin
// files signalbackup-tools writes into its output directory.
func scanAttachmentFiles(dir string) (map[int64]string, error) {
	files := make(map[int64]string)
	if dir == "" {
		return files, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read attachment directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		match := attachmentFileName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		id, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			continue
		}
		files[id] = entry.Name()
	}
	return files, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func int64Args(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}
