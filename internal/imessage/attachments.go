package imessage

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

var errAttachmentImport = errors.New("import iMessage attachments")

// WithAttachments selects the archive destination, optional copied source tree,
// and shared media policy. An empty destination records metadata only. An empty
// root uses ~/Library/Messages/Attachments without rebasing foreign paths.
// A zero byte cap uses the shared chat cap; zero participants means unlimited.
func WithAttachments(directory, root string, policy attachmentpolicy.Policy) ClientOption {
	return func(c *Client) {
		c.attachmentsDir, c.attachmentsRoot, c.rebaseAttachments = directory, root, root != ""
		c.mediaPolicy = policy
	}
}

// Columns are fixed identifiers, never names supplied by chat.db.
func (c *Client) sourceColumns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns[strings.ToLower(name)] = true
	}
	return columns, rows.Err()
}

func (c *Client) prepareAttachments(ctx context.Context) error {
	a, err := c.sourceColumns(ctx, "attachment")
	if err != nil {
		return err
	}
	j, err := c.sourceColumns(ctx, "message_attachment_join")
	if err != nil {
		return err
	}
	c.attachmentQuery = ""
	if len(a) == 0 && len(j) == 0 {
		return nil
	}
	if !a["filename"] || !j["message_id"] || !j["attachment_id"] {
		return errors.New("unsupported attachment schema: expected attachment.filename and message_attachment_join identities")
	}
	m, err := c.sourceColumns(ctx, "message")
	if err != nil {
		return err
	}
	column := func(columns map[string]bool, alias, name, fallback string) string {
		if columns[name] {
			return "COALESCE(" + alias + "." + name + "," + fallback + ")"
		}
		return fallback
	}
	fields := []string{"a.ROWID"}
	for _, name := range []string{"guid", "filename", "transfer_name", "mime_type", "uti"} {
		fields = append(fields, column(a, "a", name, "''"))
	}
	// Preserve source size and audio facts without assuming duration units or attachment mapping.
	fields = append(fields,
		column(a, "a", "total_bytes", "0"),
		column(m, "m", "is_audio_message", "0"),
		column(m, "m", "audio_message_duration", "NULL"),
		column(a, "a", "duration", "NULL"),
	)
	c.attachmentQuery = "SELECT " + strings.Join(fields, ",") + ` FROM attachment a
		JOIN message m ON m.ROWID = ? WHERE a.ROWID IN (
		SELECT j.attachment_id FROM message_attachment_join j WHERE j.message_id = m.ROWID)
		ORDER BY a.ROWID`
	return nil
}

type sourceAttachment struct {
	ref  store.AttachmentRef
	path string
}

func (c *Client) readAttachments(ctx context.Context, rowID int64) ([]sourceAttachment, error) {
	rows, err := c.db.QueryContext(ctx, c.attachmentQuery, rowID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var attachments []sourceAttachment
	for rows.Next() {
		var id, size, audio int64
		var guid, filename, name, mimeType, uti string
		var duration, attachmentDuration sql.NullFloat64
		if err := rows.Scan(&id, &guid, &filename, &name, &mimeType, &uti, &size, &audio, &duration, &attachmentDuration); err != nil {
			return nil, err
		}
		key := "imessage:" + strconv.FormatInt(id, 10)
		transferName := name
		if name == "" {
			name = filename
		}
		name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
		if name == "." || name == ".." || name == "/" || name == "" {
			name = "attachment"
		}
		metadata := map[string]any{"rowid": id, "guid": guid, "filename": filename, "transfer_name": transferName, "uti": uti, "mime_type": mimeType, "total_bytes": size, "is_audio_message": audio != 0}
		if duration.Valid {
			metadata["audio_message_duration"] = duration.Float64
		}
		if attachmentDuration.Valid {
			metadata["duration"] = attachmentDuration.Float64
		}
		encoded, err := json.Marshal(metadata, json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		ref := store.AttachmentRef{Filename: name, MimeType: mimeType, StoragePath: key, SourceAttachmentID: key,
			SourcePartKey: key, Metadata: string(encoded), Role: store.AttachmentRoleStandalone,
			RoleSource: store.AttachmentRoleSourceImporterSemantics, State: attachmentpolicy.StatePending}
		if size > 0 {
			ref.Size = attachmentpolicy.OversizeMarkerSize(0, size)
		}
		if strings.HasPrefix(strings.ToLower(mimeType), "audio/") || slices.Contains([]string{"com.apple.coreaudio-format", "public.mp3", "public.mpeg-4-audio", "public.aiff-audio", "com.microsoft.waveform-audio"}, uti) {
			ref.MediaType = "audio"
			if audio != 0 {
				ref.MediaType = "voice_note"
			}
		}
		attachments = append(attachments, sourceAttachment{ref, filename})
	}
	return attachments, rows.Err()
}

func (c *Client) importAttachments(ctx context.Context, s *store.Store, messageID int64, msg *messageRow) error {
	refs, err := s.MessageIMessageAttachmentsContext(ctx, messageID)
	if err != nil {
		return err
	}
	// Source purging must not discard captured archive bytes.
	for key, ref := range refs {
		if ref.State != attachmentpolicy.StateStored {
			delete(refs, key)
		}
	}
	var attachments []sourceAttachment
	if c.attachmentQuery != "" {
		attachments, err = c.readAttachments(ctx, msg.ROWID)
		if err != nil {
			return err
		}
	} else if len(refs) == 0 {
		return nil
	}
	conversation := attachmentpolicy.Conversation{Type: "direct_chat", ParticipantCount: 2}
	if msg.ChatGUID != nil && strings.Contains(*msg.ChatGUID, ";+;") {
		conversation.Type = "group_chat"
	}
	if msg.ChatROWID != nil && len(attachments) > 0 {
		var ownerPresent bool
		err := c.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT j.handle_id),
			EXISTS (SELECT 1 FROM chat_handle_join cj JOIN handle h ON h.ROWID = cj.handle_id WHERE cj.chat_id = ? AND h.id = ?)
			FROM chat_handle_join j WHERE j.chat_id = ?`, *msg.ChatROWID, c.ownerHandle, *msg.ChatROWID).Scan(&conversation.ParticipantCount, &ownerPresent)
		if err != nil {
			return fmt.Errorf("count attachment chat participants: %w", err)
		}
		if !ownerPresent {
			conversation.ParticipantCount++
		}
	}
	for _, attachment := range attachments {
		ref := attachment.ref
		old, captured := refs[ref.SourceAttachmentID]
		var before, after struct {
			GUID string `json:"guid"`
		}
		captured = captured && json.Unmarshal([]byte(old.Metadata), &before) == nil &&
			json.Unmarshal([]byte(ref.Metadata), &after) == nil && before.GUID == after.GUID &&
			(before.GUID != "" || old.Metadata == ref.Metadata)
		if reason := c.mediaPolicy.Evaluate(conversation, int64(ref.Size)); reason != "" {
			ref.State, ref.SkipReason = attachmentpolicy.StateSkipped, reason
		} else if c.attachmentsDir != "" {
			c.captureAttachment(ctx, &ref, attachment.path)
		}
		if captured && ref.State != attachmentpolicy.StateStored {
			ref.StoragePath, ref.ContentHash, ref.Size = old.StoragePath, old.ContentHash, old.Size
			ref.State, ref.SkipReason = attachmentpolicy.StateStored, ""
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		refs[ref.SourceAttachmentID] = ref
	}
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]store.AttachmentRef, 0, len(keys))
	for _, key := range keys {
		out = append(out, refs[key])
	}
	return s.ReplaceMessageIMessageAttachmentsContext(ctx, messageID, out)
}

func (c *Client) captureAttachment(ctx context.Context, ref *store.AttachmentRef, filename string) {
	f, err := openAttachment(c.attachmentsRoot, filename, c.rebaseAttachments)
	if err != nil {
		ref.State, ref.SkipReason = attachmentpolicy.StateUnavailable, attachmentpolicy.SkipSourceUnavailable
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		ref.State, ref.SkipReason = attachmentpolicy.StateUnavailable, attachmentpolicy.SkipSourceUnavailable
		return
	}
	if c.mediaPolicy.MaxBytes > 0 && info.Size() > c.mediaPolicy.MaxBytes {
		ref.Size = attachmentpolicy.OversizeMarkerSize(c.mediaPolicy.MaxBytes, info.Size())
		ref.State, ref.SkipReason = attachmentpolicy.StateSkipped, attachmentpolicy.SkipSizeCap
		return
	}
	reader := &export.SourceReader{R: f}
	path, hash, size, err := export.StoreAttachmentStream(ctx, c.attachmentsDir, reader, c.mediaPolicy.MaxBytes)
	switch {
	case errors.Is(err, export.ErrAttachmentTooLarge):
		ref.Size = attachmentpolicy.OversizeMarkerSize(c.mediaPolicy.MaxBytes, 0)
		ref.State, ref.SkipReason = attachmentpolicy.StateSkipped, attachmentpolicy.SkipSizeCap
	case reader.Err != nil:
		ref.State, ref.SkipReason = attachmentpolicy.StateUnavailable, attachmentpolicy.SkipSourceUnavailable
	case err != nil:
		ref.State, ref.SkipReason = attachmentpolicy.StateFailed, attachmentpolicy.SkipFetchFailure
	default:
		ref.StoragePath, ref.ContentHash, ref.Size, ref.State = path, hash, int(size), attachmentpolicy.StateStored
	}
}
