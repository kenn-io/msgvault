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
	pni := "''"
	if s.recipientPNI != "" {
		pni = "COALESCE(r." + s.recipientPNI + ", '')"
	}
	query := fmt.Sprintf(`
		SELECT r._id, COALESCE(r.%s, ''), %s, COALESCE(r.%s, ''), COALESCE(r.group_id, ''), %s
		FROM recipient r`, s.recipientACI, pni, s.recipientE164, s.recipientName)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query recipients: %w", err)
	}
	defer func() { _ = rows.Close() }()
	recipients := make(map[int64]sigRecipient)
	for rows.Next() {
		var r sigRecipient
		if err := rows.Scan(&r.ID, &r.ACI, &r.PNI, &r.E164, &r.GroupID, &r.Name); err != nil {
			return nil, fmt.Errorf("scan recipient: %w", err)
		}
		r.ACI = normalizeACI(r.ACI)
		r.PNI = normalizePNI(r.PNI)
		r.E164 = NormalizePhone(r.E164)
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
// from group_membership.
func fetchGroupMembers(ctx context.Context, db *sql.DB, groupID string) ([]int64, error) {
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
		       COALESCE(o.date_sent, m.date_sent), m.date_sent,
		       COALESCE(o.date_received, m.date_received),
		       m.thread_id, m.type, m.body,
		       COALESCE(m.quote_id, 0), COALESCE(m.quote_author, 0),
		       COALESCE(%s, 0), COALESCE(%s, 0), COALESCE(%s, 0),
		       COALESCE(%s, -1)
		FROM message m
		LEFT JOIN message o ON o._id = m.original_message_id
		WHERE m.thread_id = ? AND m.latest_revision_id IS NULL AND m._id > ?
		ORDER BY m._id
		LIMIT ?`,
		s.messageColumn("story_type", "0"),
		s.messageColumn("parent_story_id", "0"),
		s.deletedMessageExpr(),
		s.messageColumn("scheduled_date", "-1"),
	)
	rows, err := db.QueryContext(ctx, query, threadID, afterRowID, limit)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var messages []sigMessage
	for rows.Next() {
		var m sigMessage
		if err := rows.Scan(&m.RowID, &m.OriginalRowID, &m.AuthorID, &m.DateSent, &m.RevisionDateSent, &m.DateReceived,
			&m.ThreadID, &m.Type, &m.Body, &m.QuoteID, &m.QuoteAuthor,
			&m.StoryType, &m.ParentStoryID, &m.RemoteDeleted,
			&m.ScheduledDate); err != nil {
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
	if len(messageIDs) == 0 {
		return out, nil
	}
	query := fmt.Sprintf(`
		SELECT a._id, a.%s, COALESCE(a.%s, ''), COALESCE(%s, ''), COALESCE(%s, ''),
		       COALESCE(%s, 0), COALESCE(%s, 0),
		       COALESCE(%s, 0), COALESCE(%s, 0), COALESCE(%s, ''),
		       COALESCE(%s, ''), COALESCE(%s, '')
		FROM %s a
		JOIN message m ON m._id = a.%s
		WHERE a.%s IN (%s) AND COALESCE(%s, 0) = 0
		ORDER BY a.%s, COALESCE(%s, 0), a._id`,
		s.attachmentMessage, s.attachmentType,
		s.attachmentColumn("file_name", "''"), s.attachmentColumn("caption", "''"),
		s.attachmentColumn("width", "0"),
		s.attachmentColumn("height", "0"), s.attachmentColumn("voice_note", "0"),
		s.attachmentColumn("video_gif", "0"), s.attachmentColumn("sticker_pack_id", "''"),
		s.messageColumn("link_previews", "''"), s.messageColumn("shared_contacts", "''"),
		s.attachmentTable, s.attachmentMessage, s.attachmentMessage, placeholders(len(messageIDs)),
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
		var previews, contacts string
		if err := rows.Scan(&a.ID, &a.MessageID, &a.ContentType, &a.FileName, &a.Caption,
			&a.Width, &a.Height, &voiceNote, &videoGIF, &a.StickerPackID, &previews, &contacts); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		a.VoiceNote = voiceNote != 0
		a.VideoGIF = videoGIF != 0
		a.Role = attachmentRole(a.ID, previews, contacts)
		out[a.MessageID] = append(out[a.MessageID], a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachments: %w", err)
	}
	return out, nil
}

// fetchReactions returns reactions keyed by the original message row, so a
// reaction recorded against any revision of an edited message is found.
func fetchReactions(ctx context.Context, db *sql.DB, rootIDs []int64) (map[int64][]sigReaction, error) {
	out := make(map[int64][]sigReaction)
	if len(rootIDs) == 0 {
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
func fetchMentions(ctx context.Context, db *sql.DB, messageIDs []int64) (map[int64][]sigMention, error) {
	out := make(map[int64][]sigMention)
	if len(messageIDs) == 0 {
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

// ownerEvidence counts, per recipient, the messages that name it as the
// account owner: the author of outgoing messages and the recipient of
// incoming messages outside groups. V185 copied the author into both
// address columns on old incoming rows, so those rows supply no owner evidence.
func ownerEvidence(ctx context.Context, db *sql.DB) (map[int64]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT owner, COUNT(*) FROM (
			SELECT from_recipient_id AS owner FROM message
			WHERE (type & ?) IN (?, ?, ?, ?, ?)
			UNION ALL
			SELECT m.to_recipient_id FROM message m
			WHERE (m.type & ?) = ?
			  AND m.to_recipient_id <> m.from_recipient_id
			  AND NOT EXISTS (
				SELECT 1 FROM recipient r
				WHERE r._id = m.to_recipient_id AND COALESCE(r.group_id, '') <> '')
		)
		GROUP BY owner`,
		baseTypeMask, baseOutboxType, baseSendingType, baseSentType, baseSentFailedType, baseSendingSkippedType,
		baseTypeMask, baseInboxType)
	if err != nil {
		return nil, fmt.Errorf("query backup owner: %w", err)
	}
	defer func() { _ = rows.Close() }()
	evidence := make(map[int64]int64)
	for rows.Next() {
		var id, count int64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("scan backup owner: %w", err)
		}
		evidence[id] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backup owner: %w", err)
	}
	return evidence, nil
}

var attachmentFileName = regexp.MustCompile(`^Attachment_(\d+)_(-?\d+)\.bin$`)

// scanAttachmentFiles maps attachment row IDs to the Attachment_<id>_<unique>.bin
// files signalbackup-tools writes into its output directory.
func (r *run) scanAttachmentFiles(dir string) (map[int64]string, error) {
	files := make(map[int64]string)
	ambiguous := make(map[int64]bool)
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
		if ambiguous[id] {
			continue
		}
		if previous, ok := files[id]; ok {
			delete(files, id)
			ambiguous[id] = true
			r.summary.Errors++
			r.imp.progress.OnError(fmt.Errorf("multiple attachment files match %s and %s; export the backup into an empty directory", previous, entry.Name()))
			continue
		}
		files[id] = entry.Name()
	}
	return files, nil
}

func int64Args(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// deletedMessageExpr follows V302's deletion marker while retaining old layouts.
func (s *schema) deletedMessageExpr() string {
	if s.messageColumns["deleted_by"] {
		return "(m.deleted_by IS NOT NULL)"
	}
	return s.messageColumn("remote_deleted", "0")
}
