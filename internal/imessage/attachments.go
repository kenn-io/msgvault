package imessage

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

// ErrAttachmentImport marks attachment failures. File failures let the scan
// finish its remaining work; a failed archive write ends it.
var ErrAttachmentImport = errors.New("import iMessage attachments")

// errAttachmentStore marks a failed archive database write, which ends the run.
var errAttachmentStore = errors.New("persist iMessage attachments")

// WithAttachments selects the archive destination, optional copied Messages
// folder, and shared media policy. An empty destination records metadata only.
// An empty root uses ~/Library/Messages without rebasing foreign paths. Only its
// Attachments and StickerCache trees are read.
// A zero byte cap uses the shared chat cap; zero participants means unlimited.
func WithAttachments(directory, root string, policy attachmentpolicy.Policy) ClientOption {
	return func(c *Client) {
		c.attachmentsDir, c.messagesRoot, c.rebaseAttachments = directory, root, root != ""
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
	c.attachmentQuery, c.participantCounts = "", map[int64]int{}
	if len(a) == 0 && len(j) == 0 {
		return c.schemaMismatch()
	}
	if !a["filename"] || !j["message_id"] || !j["attachment_id"] {
		return c.schemaMismatch()
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
	fields = append(fields, column(a, "a", "total_bytes", "0"), column(a, "a", "is_sticker", "0"))
	c.attachmentQuery = "SELECT " + strings.Join(fields, ",") + ` FROM attachment a WHERE a.ROWID IN (
		SELECT j.attachment_id FROM message_attachment_join j WHERE j.message_id = ?)
		ORDER BY a.ROWID`
	return nil
}

// schemaMismatch rejects an attachment schema the importer cannot read. Metadata-only
// runs never depended on it before, so they skip attachments instead of failing.
func (c *Client) schemaMismatch() error {
	if c.attachmentsDir == "" {
		return nil
	}
	return errors.New("unsupported attachment schema: expected attachment.filename and message_attachment_join identities; rerun with --no-attachments to import messages without them")
}

type sourceAttachment struct {
	ref  store.AttachmentRef
	path string
}

// sameAttachmentRef compares metadata as JSON, since Postgres reorders jsonb text.
func sameAttachmentRef(a, b store.AttachmentRef) bool {
	if !meetingarchive.JSONEvidenceEqual([]byte(a.Metadata), []byte(b.Metadata)) {
		return false
	}
	a.Metadata, b.Metadata = "", ""
	return a == b
}

func attachmentWrite(r store.AttachmentRef) store.AttachmentWrite {
	return store.AttachmentWrite{
		Filename: r.Filename, MIMEType: r.MimeType, StoragePath: r.StoragePath, ContentHash: r.ContentHash,
		Size: int64(r.Size), SourceAttachmentID: r.SourceAttachmentID, MediaType: r.MediaType,
		Width: r.Width, Height: r.Height, DurationMS: r.DurationMS, Metadata: r.Metadata,
		Role: r.Role, RoleSource: r.RoleSource, SourcePartKey: r.SourcePartKey, ContentID: r.ContentID,
		State: r.State, SkipReason: r.SkipReason,
	}
}

// audioUTIMIMETypes maps the audio UTIs the importer recognizes to the MIME type the Files view
// groups on, for rows where chat.db has no mime_type.
var audioUTIMIMETypes = map[string]string{
	"com.apple.coreaudio-format":   "audio/x-caf",
	"public.mp3":                   "audio/mpeg",
	"public.mpeg-4-audio":          "audio/mp4",
	"public.aiff-audio":            "audio/x-aiff",
	"com.microsoft.waveform-audio": "audio/x-wav",
}

func (c *Client) readAttachments(ctx context.Context, rowID int64) ([]sourceAttachment, error) {
	rows, err := c.db.QueryContext(ctx, c.attachmentQuery, rowID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var attachments []sourceAttachment
	for rows.Next() {
		var id, size, sticker int64
		var guid, filename, name, mimeType, uti string
		if err := rows.Scan(&id, &guid, &filename, &name, &mimeType, &uti, &size, &sticker); err != nil {
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
		metadata := map[string]any{"rowid": id, "guid": strings.ToValidUTF8(guid, "\uFFFD"),
			"filename": strings.ToValidUTF8(filename, "\uFFFD"), "transfer_name": strings.ToValidUTF8(transferName, "\uFFFD"),
			"uti": strings.ToValidUTF8(uti, "\uFFFD"), "mime_type": strings.ToValidUTF8(mimeType, "\uFFFD"), "total_bytes": size}
		encoded, err := json.Marshal(metadata, json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		ref := store.AttachmentRef{Filename: strings.ToValidUTF8(name, "\uFFFD"), MimeType: strings.ToValidUTF8(mimeType, "\uFFFD"),
			StoragePath: key, SourceAttachmentID: key, SourcePartKey: key, Metadata: string(encoded), Role: store.AttachmentRoleStandalone,
			RoleSource: store.AttachmentRoleSourceImporterSemantics, State: attachmentpolicy.StatePending}
		if sticker != 0 {
			ref.Role, ref.RoleSource = store.AttachmentRoleSticker, store.AttachmentRoleSourceProviderExplicit
		}
		if size > 0 {
			ref.Size = attachmentpolicy.OversizeMarkerSize(0, size)
		}
		if strings.HasPrefix(strings.ToLower(mimeType), "audio/") {
			ref.MediaType = "audio"
		} else if audioMIME, ok := audioUTIMIMETypes[uti]; ok {
			ref.MediaType = "audio"
			if mimeType == "" {
				ref.MimeType = audioMIME
			}
		}
		attachments = append(attachments, sourceAttachment{ref, filename})
	}
	return attachments, rows.Err()
}

// importAttachments writes only the occurrences that changed, as gvoice does. Archived rows stay
// when chat.db drops their join row, and an archived capture survives an unreadable source.
func (c *Client) importAttachments(ctx context.Context, s *store.Store, messageID int64, msg *messageRow, summary *ImportSummary) error {
	previous, err := s.MessageIMessageAttachmentsContext(ctx, messageID)
	if err != nil {
		return err
	}
	var attachments []sourceAttachment
	if c.attachmentQuery != "" {
		attachments, err = c.readAttachments(ctx, msg.ROWID)
		if err != nil {
			return err
		}
	}
	if len(attachments) == 0 {
		if len(previous) > 0 {
			return nil
		}
		// With no rows to count, the archived flag follows the source flag.
		flag := msg.HasAttachments != 0
		_, err := s.DB().ExecContext(ctx, s.Rebind(`UPDATE messages SET has_attachments = ? WHERE id = ? AND has_attachments <> ?`),
			flag, messageID, flag)
		return err
	}
	conversation := attachmentpolicy.Conversation{Type: "direct_chat", ParticipantCount: 2}
	if msg.ChatROWID != nil {
		if count, ok := c.participantCounts[*msg.ChatROWID]; ok {
			conversation.ParticipantCount = count
		}
	}
	var captureErrs []error
	for _, attachment := range attachments {
		ref := attachment.ref
		old, existing := previous[ref.SourceAttachmentID]
		// A GUID identifies the file; rows without one are identified by their source filename.
		var before, after struct {
			GUID     string `json:"guid"`
			Filename string `json:"filename"`
		}
		captured := existing && json.Unmarshal([]byte(old.Metadata), &before) == nil &&
			json.Unmarshal([]byte(ref.Metadata), &after) == nil && before.GUID == after.GUID &&
			(before.GUID != "" || before.Filename == after.Filename)
		// A metadata-only run attempts no file, so the media policy has nothing to skip.
		if c.attachmentsDir != "" {
			if reason := c.mediaPolicy.Evaluate(conversation, int64(ref.Size)); reason != "" {
				ref.State, ref.SkipReason = attachmentpolicy.StateSkipped, reason
			} else if err := c.captureAttachment(ctx, &ref, attachment.path); err != nil {
				captureErrs = append(captureErrs, err)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// This run's outcome counts even when the archived bytes keep the row stored.
		switch ref.State {
		case attachmentpolicy.StateSkipped:
			summary.AttachmentsSkipped++
		case attachmentpolicy.StateUnavailable:
			summary.AttachmentsUnavailable++
		case attachmentpolicy.StatePending, attachmentpolicy.StateStored, attachmentpolicy.StateFailed:
		}
		// An archived occurrence without a fresh capture keeps its stored bytes. Blob health is
		// checked when the bytes are read, not on import. UpsertAttachmentRecordPreservingStored
		// does not fit: it preserves only an empty incoming StoragePath and ContentHash, while this
		// importer writes its placeholder StoragePath key on unstored rows, and the GUID/filename
		// identity check lives here.
		if captured && ref.State != attachmentpolicy.StateStored && old.State == attachmentpolicy.StateStored && old.ContentHash != "" {
			ref.StoragePath, ref.ContentHash, ref.Size = old.StoragePath, old.ContentHash, old.Size
			ref.State, ref.SkipReason = attachmentpolicy.StateStored, ""
		}
		if existing && sameAttachmentRef(old, ref) {
			continue
		}
		if err := s.UpsertAttachmentRecordWithStats(ctx, messageID, attachmentWrite(ref), messageID <= c.maxMessageID); err != nil {
			return errors.Join(append(captureErrs, fmt.Errorf("%w: %w", errAttachmentStore, err))...)
		}
	}
	return errors.Join(captureErrs...)
}

// chatParticipantCount returns the chat's roster size including the owner, read once per run.
// The first read also records it on the conversation, which media purge needs to apply the
// participant limit, so every chat in the run must reach it whether or not it has attachments.
func (c *Client) chatParticipantCount(ctx context.Context, s *store.Store, convID, chatROWID int64) (int, error) {
	if count, ok := c.participantCounts[chatROWID]; ok {
		return count, nil
	}
	var count int
	var ownerPresent bool
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT j.handle_id),
		EXISTS (SELECT 1 FROM chat_handle_join cj JOIN handle h ON h.ROWID = cj.handle_id WHERE cj.chat_id = ? AND h.id = ?)
		FROM chat_handle_join j WHERE j.chat_id = ?`, chatROWID, c.ownerHandle, chatROWID).Scan(&count, &ownerPresent)
	if err != nil {
		return 0, fmt.Errorf("count attachment chat participants: %w", err)
	}
	if !ownerPresent {
		count++
	}
	if err := s.SetConversationMemberCountContext(ctx, convID, count); err != nil {
		return 0, fmt.Errorf("%w: %w", errAttachmentStore, err)
	}
	c.participantCounts[chatROWID] = count
	return count, nil
}

// captureAttachment streams the source file into content-addressed storage.
func (c *Client) captureAttachment(ctx context.Context, ref *store.AttachmentRef, filename string) error {
	f, err := openAttachment(c.messagesRoot, filename, c.rebaseAttachments)
	if err != nil {
		if errors.Is(err, errOutsideAttachmentRoot) {
			ref.State, ref.SkipReason = attachmentpolicy.StateSkipped, attachmentpolicy.SkipAccountPolicy
		} else {
			ref.State, ref.SkipReason = attachmentpolicy.StateUnavailable, attachmentpolicy.SkipSourceUnavailable
		}
		return nil
	}
	defer func() { _ = f.Close() }()
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
		return fmt.Errorf("store iMessage attachment bytes: %w", err)
	default:
		ref.StoragePath, ref.ContentHash, ref.Size, ref.State = path, hash, int(size), attachmentpolicy.StateStored
	}
	return nil
}
