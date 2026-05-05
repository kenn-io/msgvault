package whatsapp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wesm/msgvault/internal/store"
)

// Importer handles importing WhatsApp messages from a decrypted msgstore.db
// into the msgvault store.
type Importer struct {
	store    *store.Store
	progress ImportProgress
}

// NewImporter creates a new WhatsApp importer.
func NewImporter(s *store.Store, progress ImportProgress) *Importer {
	if progress == nil {
		progress = NullProgress{}
	}
	return &Importer{
		store:    s,
		progress: progress,
	}
}

// Import performs the full WhatsApp import from a decrypted msgstore.db.
func (imp *Importer) Import(ctx context.Context, waDBPath string, opts ImportOptions) (*ImportSummary, error) {
	startTime := time.Now()
	summary := &ImportSummary{}

	// Open WhatsApp DB read-only.
	// Use file: URI to safely handle paths containing '?' or other special characters.
	dsn := (&url.URL{
		Scheme:   "file",
		OmitHost: true,
		Path:     waDBPath,
		RawQuery: "mode=ro&_journal_mode=WAL&_busy_timeout=5000",
	}).String()
	waDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open whatsapp db: %w", err)
	}
	defer func() { _ = waDB.Close() }()

	// Verify it's a valid WhatsApp DB.
	if err := verifyWhatsAppDB(waDB); err != nil {
		return nil, err
	}

	// Create or get the WhatsApp source.
	source, err := imp.store.GetOrCreateSource("whatsapp", opts.Phone)
	if err != nil {
		return nil, fmt.Errorf("get or create source: %w", err)
	}

	if opts.DisplayName != "" {
		_ = imp.store.UpdateSourceDisplayName(source.ID, opts.DisplayName)
	}
	summary.SourceID = source.ID

	// Start a sync run for tracking.
	syncID, err := imp.store.StartSync(source.ID, "whatsapp_import")
	if err != nil {
		return nil, fmt.Errorf("start sync: %w", err)
	}

	// Ensure we complete/fail the sync run on exit.
	var syncErr error
	defer func() {
		if syncErr != nil {
			_ = imp.store.FailSync(syncID, syncErr.Error())
		} else {
			_ = imp.store.CompleteSync(syncID, "")
		}
	}()

	imp.progress.OnStart()

	// Create participant for the phone owner (self).
	selfParticipantID, err := imp.store.EnsureParticipantByPhone(opts.Phone, opts.DisplayName, "whatsapp")
	if err != nil {
		syncErr = err
		return nil, fmt.Errorf("ensure self participant: %w", err)
	}
	summary.Participants++

	// Fetch all chats from WhatsApp DB.
	chats, err := fetchChats(waDB)
	if err != nil {
		syncErr = err
		return nil, fmt.Errorf("fetch chats: %w", err)
	}

	// Load lid → phone mapping for resolving "lid" senders.
	lidMap, err := fetchLidMap(waDB)
	if err != nil {
		syncErr = err
		return nil, fmt.Errorf("fetch lid map: %w", err)
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 1000
	}

	// Track key_id → message_id for reply threading within each chat.
	// Scoped per chat to bound memory; cross-chat quotes won't thread
	// but that's rare and the quoted text is still in the message body.
	keyIDToMsgID := make(map[string]int64)

	totalLimit := opts.Limit
	totalAdded := int64(0)

	for _, chat := range chats {
		// Clear reply map per chat to prevent unbounded growth.
		clear(keyIDToMsgID)
		if ctx.Err() != nil {
			syncErr = ctx.Err()
			return nil, ctx.Err()
		}

		// Check global limit.
		if totalLimit > 0 && totalAdded >= int64(totalLimit) {
			break
		}

		summary.ChatsProcessed++

		// Map chat to conversation.
		sourceConvID, convType, title := mapConversation(chat)
		conversationID, err := imp.store.EnsureConversationWithType(source.ID, sourceConvID, convType, title)
		if err != nil {
			summary.Errors++
			imp.progress.OnError(fmt.Errorf("ensure conversation %s: %w", sourceConvID, err))
			continue
		}

		imp.progress.OnChatStart(chat.RawString, chatTitle(chat), 0)

		// For direct chats: add the remote participant.
		if !isGroupChat(chat) && chat.User != "" {
			phone := normalizePhone(chat.User, chat.Server)
			if phone == "" {
				// Non-phone JID (e.g., lid:..., broadcast) — skip.
			} else if participantID, err := imp.store.EnsureParticipantByPhone(phone, "", "whatsapp"); err != nil {
				summary.Errors++
				imp.progress.OnError(fmt.Errorf("ensure participant %s: %w", phone, err))
			} else {
				summary.Participants++
				_ = imp.store.EnsureConversationParticipant(conversationID, participantID, "member")
				_ = imp.store.EnsureConversationParticipant(conversationID, selfParticipantID, "member")
			}
		}

		// For group chats: add all group participants.
		if isGroupChat(chat) {
			members, err := fetchGroupParticipants(waDB, chat.RawString)
			if err != nil {
				summary.Errors++
				imp.progress.OnError(fmt.Errorf("fetch group participants for %s: %w", sourceConvID, err))
			} else {
				for _, member := range members {
					phone := normalizePhone(member.MemberUser, member.MemberServer)
					if phone == "" {
						continue // Non-phone JID — skip.
					}
					participantID, err := imp.store.EnsureParticipantByPhone(phone, "", "whatsapp")
					if err != nil {
						summary.Errors++
						continue
					}
					summary.Participants++
					role := mapGroupRole(member.Admin)
					_ = imp.store.EnsureConversationParticipant(conversationID, participantID, role)
				}
			}
		}

		// Track resolved sender participant IDs for participant fallback.
		// After the message loop, we ensure each sender is a conversation
		// participant — covers groups where group_participants is empty.
		chatSenderIDs := make(map[int64]struct{})

		// Process messages in batches.
		chatAdded := int64(0)
		afterID := int64(0)

		for {
			if ctx.Err() != nil {
				syncErr = ctx.Err()
				return nil, ctx.Err()
			}

			// Check global limit for this batch.
			remaining := batchSize
			if totalLimit > 0 {
				left := int64(totalLimit) - totalAdded
				if left <= 0 {
					break
				}
				if left < int64(remaining) {
					remaining = int(left)
				}
			}

			messages, err := fetchMessages(waDB, chat.RowID, afterID, remaining)
			if err != nil {
				summary.Errors++
				imp.progress.OnError(fmt.Errorf("fetch messages for chat %s after %d: %w", sourceConvID, afterID, err))
				break
			}

			if len(messages) == 0 {
				break
			}

			// Collect message row IDs for batch media/reaction/quote lookups.
			msgRowIDs := make([]int64, len(messages))
			for i, m := range messages {
				msgRowIDs[i] = m.RowID
			}

			// Batch-fetch media, reactions, and quotes.
			mediaMap, err := fetchMedia(waDB, msgRowIDs)
			if err != nil {
				summary.Errors++
				imp.progress.OnError(fmt.Errorf("fetch media: %w", err))
				mediaMap = make(map[int64]waMedia)
			}

			reactionMap, err := fetchReactions(waDB, msgRowIDs)
			if err != nil {
				summary.Errors++
				imp.progress.OnError(fmt.Errorf("fetch reactions: %w", err))
				reactionMap = make(map[int64][]waReaction)
			}

			quotedMap, err := fetchQuotedMessages(waDB, msgRowIDs)
			if err != nil {
				summary.Errors++
				imp.progress.OnError(fmt.Errorf("fetch quoted messages: %w", err))
				quotedMap = make(map[int64]waQuoted)
			}

			for _, waMsg := range messages {
				summary.MessagesProcessed++
				afterID = waMsg.RowID

				// Skip system messages and calls.
				if isSkippedType(waMsg.MessageType) {
					summary.MessagesSkipped++
					continue
				}

				// Skip messages with empty key_id — they can't be uniquely
				// identified for upsert and would collide with each other.
				if waMsg.KeyID == "" {
					summary.MessagesSkipped++
					continue
				}

				// Resolve sender.
				var senderID sql.NullInt64
				if waMsg.FromMe == 1 {
					senderID = sql.NullInt64{Int64: selfParticipantID, Valid: true}
				} else if waMsg.SenderServer.Valid && waMsg.SenderServer.String == "lid" {
					// Lid JID — resolve via jid_map before trying normalizePhone,
					// because lid user strings can be 15 digits and pass E.164
					// validation despite not being real phone numbers.
					phone := resolveLidSender(waMsg.SenderJIDRowID, waMsg.SenderServer.String, lidMap)
					if phone != "" {
						pid, err := imp.store.EnsureParticipantByPhone(phone, "", "whatsapp")
						if err != nil {
							summary.Errors++
							imp.progress.OnError(fmt.Errorf("ensure sender participant %s: %w", phone, err))
						} else {
							senderID = sql.NullInt64{Int64: pid, Valid: true}
						}
					}
				} else if waMsg.SenderUser.Valid && waMsg.SenderUser.String != "" {
					phone := normalizePhone(waMsg.SenderUser.String, waMsg.SenderServer.String)
					if phone != "" {
						pid, err := imp.store.EnsureParticipantByPhone(phone, "", "whatsapp")
						if err != nil {
							summary.Errors++
							imp.progress.OnError(fmt.Errorf("ensure sender participant %s: %w", phone, err))
						} else {
							senderID = sql.NullInt64{Int64: pid, Valid: true}
						}
					}
				} else if !isGroupChat(chat) && waMsg.FromMe == 0 {
					// In a direct chat, the other person is the sender.
					phone := normalizePhone(chat.User, chat.Server)
					if phone != "" {
						pid, err := imp.store.EnsureParticipantByPhone(phone, "", "whatsapp")
						if err == nil {
							senderID = sql.NullInt64{Int64: pid, Valid: true}
						}
					}
				}

				// Track sender for participant fallback.
				if senderID.Valid {
					chatSenderIDs[senderID.Int64] = struct{}{}
				}

				// Build and upsert the message.
				msg := mapMessage(waMsg, conversationID, source.ID, senderID)
				messageID, err := imp.store.UpsertMessage(&msg)
				if err != nil {
					summary.Errors++
					imp.progress.OnError(fmt.Errorf("upsert message %s: %w", waMsg.KeyID, err))
					continue
				}

				// Track for reply threading.
				keyIDToMsgID[waMsg.KeyID] = messageID

				summary.MessagesAdded++
				chatAdded++
				totalAdded++

				// Store message body.
				bodyText := sql.NullString{}
				if waMsg.TextData.Valid && waMsg.TextData.String != "" {
					bodyText = waMsg.TextData
				}
				// Check for media caption as additional body text.
				if media, ok := mediaMap[waMsg.RowID]; ok {
					if media.MediaCaption.Valid && media.MediaCaption.String != "" {
						if bodyText.Valid && bodyText.String != "" {
							// Append caption to body.
							bodyText.String += "\n\n" + media.MediaCaption.String
						} else {
							bodyText = media.MediaCaption
						}
					}
				}
				if bodyText.Valid {
					_ = imp.store.UpsertMessageBody(messageID, bodyText, sql.NullString{})
				}

				// Store raw JSON for re-parsing.
				rawJSON, err := json.Marshal(waMsg)
				if err == nil {
					_ = imp.store.UpsertMessageRawWithFormat(messageID, rawJSON, "whatsapp_json")
				}

				// Handle media/attachments.
				if media, ok := mediaMap[waMsg.RowID]; ok {
					summary.AttachmentsFound++
					mediaType := mapMediaType(waMsg.MessageType)

					storagePath, contentHash := imp.handleMediaFile(media, opts)
					if storagePath != "" {
						summary.MediaCopied++
					}

					mimeType := ""
					if media.MimeType.Valid {
						mimeType = media.MimeType.String
					}

					filename := ""
					if media.FilePath.Valid {
						filename = filepath.Base(media.FilePath.String)
					}

					size := 0
					if media.FileSize.Valid {
						size = int(media.FileSize.Int64)
					}

					// Only insert attachment row when we have actual content.
					// Without --media-dir, storagePath and contentHash are both
					// empty; inserting would create broken records.
					if storagePath != "" || contentHash != "" {
						err := imp.store.UpsertAttachment(messageID, filename, mimeType, storagePath, contentHash, size)
						if err != nil {
							summary.Errors++
							imp.progress.OnError(fmt.Errorf("upsert attachment for message %s: %w", waMsg.KeyID, err))
						}
					} else {
						// No media stored — clear attachment flags set by mapMessage
						// so Parquet/TUI queries don't show phantom attachments.
						// Only clear if the message has no pre-existing attachment rows
						// (a previous import with --media-dir may have created them).
						var existingCount int
						_ = imp.store.DB().QueryRow(
							"SELECT COUNT(*) FROM attachments WHERE message_id = ?",
							messageID,
						).Scan(&existingCount)
						if existingCount == 0 {
							_, _ = imp.store.DB().Exec(
								"UPDATE messages SET has_attachments = 0, attachment_count = 0 WHERE id = ?",
								messageID)
						}
					}

					// Store media metadata in the attachments table is done above.
					// For extra metadata (width, height, duration, media_type),
					// update via a direct SQL call since UpsertAttachment doesn't have those fields.
					if mediaType != "" || (media.Width.Valid && media.Width.Int64 > 0) {
						imp.updateAttachmentMetadata(messageID, contentHash, mediaType, media)
					}
				}

				// Handle quoted/reply messages.
				if quoted, ok := quotedMap[waMsg.RowID]; ok {
					if replyToMsgID, found := keyIDToMsgID[quoted.QuotedKeyID]; found {
						imp.setReplyTo(messageID, replyToMsgID)
					} else if dbMsgID, lookupErr := imp.lookupMessageByKeyID(source.ID, quoted.QuotedKeyID); lookupErr == nil && dbMsgID > 0 {
						// Found in DB from a previous import run or another chat.
						imp.setReplyTo(messageID, dbMsgID)
					}
				}

				// Handle reactions.
				if reactions, ok := reactionMap[waMsg.RowID]; ok {
					for _, r := range reactions {
						reactionType, reactionValue := mapReaction(r)
						if reactionValue == "" {
							continue
						}

						var reactorID int64
						if r.SenderServer.Valid && r.SenderServer.String == "lid" {
							// Lid JID — resolve via jid_map first.
							phone := resolveLidSender(r.SenderJIDRowID, r.SenderServer.String, lidMap)
							if phone == "" {
								continue
							}
							pid, err := imp.store.EnsureParticipantByPhone(phone, "", "whatsapp")
							if err != nil {
								summary.Errors++
								continue
							}
							reactorID = pid
						} else if r.SenderUser.Valid && r.SenderUser.String != "" {
							phone := normalizePhone(r.SenderUser.String, r.SenderServer.String)
							if phone == "" {
								continue // Non-phone JID — skip reaction.
							}
							pid, err := imp.store.EnsureParticipantByPhone(phone, "", "whatsapp")
							if err != nil {
								summary.Errors++
								continue
							}
							reactorID = pid
						} else {
							// Self reaction.
							reactorID = selfParticipantID
						}

						createdAt := time.Unix(r.Timestamp/1000, 0)
						if err := imp.store.UpsertReaction(messageID, reactorID, reactionType, reactionValue, createdAt); err != nil {
							summary.Errors++
							imp.progress.OnError(fmt.Errorf("upsert reaction: %w", err))
						} else {
							summary.ReactionsAdded++
						}
					}
				}

				// FTS indexing.
				if bodyText.Valid {
					senderAddr := ""
					if waMsg.FromMe == 1 {
						senderAddr = opts.Phone
					} else if waMsg.SenderServer.Valid && waMsg.SenderServer.String == "lid" {
						senderAddr = resolveLidSender(waMsg.SenderJIDRowID, waMsg.SenderServer.String, lidMap)
					} else if waMsg.SenderUser.Valid {
						senderAddr = normalizePhone(waMsg.SenderUser.String, waMsg.SenderServer.String)
					}
					_ = imp.store.UpsertFTS(messageID, "", bodyText.String, senderAddr, "", "")
				}
			}

			// Update sync run progress counters (for monitoring, not resume).
			// Resume is not implemented yet — re-running is safe due to upsert dedup.
			_ = imp.store.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
				MessagesProcessed: summary.MessagesProcessed,
				MessagesAdded:     summary.MessagesAdded,
			})

			imp.progress.OnProgress(summary.MessagesProcessed, summary.MessagesAdded, summary.MessagesSkipped)

			// If we got fewer than requested, we've finished this chat.
			if len(messages) < remaining {
				break
			}
		}

		// Participant fallback: ensure every resolved sender is a conversation
		// participant. Covers groups where group_participants is empty (newer
		// WhatsApp versions) and any senders discovered via lid resolution.
		for pid := range chatSenderIDs {
			_ = imp.store.EnsureConversationParticipant(conversationID, pid, "member")
		}
		// Always include self as participant.
		_ = imp.store.EnsureConversationParticipant(conversationID, selfParticipantID, "member")

		imp.progress.OnChatComplete(chat.RawString, chatAdded)
	}

	// Update denormalised conversation counts for the WhatsApp source.
	if err := imp.store.RecomputeConversationStats(source.ID); err != nil {
		imp.progress.OnError(fmt.Errorf("recompute conversation stats: %w", err))
	}

	summary.Duration = time.Since(startTime)
	imp.progress.OnComplete(summary)

	return summary, nil
}

// handleMediaFile attempts to find and copy a media file to content-addressed storage.
// Returns (storagePath, contentHash). Both empty if file not found.
func (imp *Importer) handleMediaFile(media waMedia, opts ImportOptions) (string, string) {
	if opts.MediaDir == "" || opts.AttachmentsDir == "" || !media.FilePath.Valid || media.FilePath.String == "" {
		return "", ""
	}

	mediaDir := opts.MediaDir

	// Sanitize the path from the WhatsApp DB (untrusted data).
	relPath := filepath.Clean(media.FilePath.String)

	// Reject absolute paths — the DB should only contain relative paths.
	if filepath.IsAbs(relPath) {
		relPath = filepath.Base(relPath)
	}

	// Reject directory traversal.
	if relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		relPath = filepath.Base(relPath)
	}

	// Build candidate path and verify it stays within mediaDir.
	fullPath := filepath.Join(mediaDir, relPath)
	absMediaDir, err := filepath.Abs(mediaDir)
	if err != nil {
		return "", ""
	}
	absFullPath, err := filepath.Abs(fullPath)
	if err != nil {
		return "", ""
	}
	if !strings.HasPrefix(absFullPath, absMediaDir+string(filepath.Separator)) && absFullPath != absMediaDir {
		// Path escapes mediaDir — fall back to base filename only.
		fullPath = filepath.Join(mediaDir, filepath.Base(relPath))
		absFullPath, _ = filepath.Abs(fullPath)
		if !strings.HasPrefix(absFullPath, absMediaDir+string(filepath.Separator)) {
			return "", ""
		}
	}

	// Check file exists.
	info, err := os.Stat(fullPath)
	if err != nil {
		// Try just the filename as fallback.
		fullPath = filepath.Join(mediaDir, filepath.Base(relPath))
		info, err = os.Stat(fullPath)
		if err != nil {
			return "", ""
		}
	}

	// Enforce max file size to prevent OOM.
	maxSize := opts.MaxMediaFileSize
	if maxSize <= 0 {
		maxSize = 100 * 1024 * 1024 // 100MB default
	}
	if info.Size() > maxSize {
		return "", ""
	}

	// Open file and compute hash by streaming (no full-file read into memory).
	f, err := os.Open(fullPath)
	if err != nil {
		return "", ""
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxSize+1)); err != nil {
		return "", ""
	}
	contentHash := fmt.Sprintf("%x", h.Sum(nil))

	// Content-addressed storage: <attachmentsDir>/<hash[:2]>/<hash>
	// The storage_path stored in DB is the relative portion: <hash[:2]>/<hash>
	relStoragePath := filepath.Join(contentHash[:2], contentHash)
	absStoragePath := filepath.Join(opts.AttachmentsDir, relStoragePath)

	// Check for dedup — file already stored.
	if _, err := os.Stat(absStoragePath); err == nil {
		return relStoragePath, contentHash
	}

	// Create directory and stream-copy the file.
	absStorageDir := filepath.Dir(absStoragePath)
	if err := os.MkdirAll(absStorageDir, 0750); err != nil {
		return "", contentHash
	}

	// Seek back to beginning of source file for the copy.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", contentHash
	}

	dst, err := os.OpenFile(absStoragePath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			// Race: another goroutine already wrote it.
			return relStoragePath, contentHash
		}
		return "", contentHash
	}

	if _, err := io.Copy(dst, io.LimitReader(f, maxSize)); err != nil {
		_ = dst.Close()
		_ = os.Remove(absStoragePath)
		return "", contentHash
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(absStoragePath)
		return "", contentHash
	}

	return relStoragePath, contentHash
}

// updateAttachmentMetadata updates media-specific metadata on an attachment record.
func (imp *Importer) updateAttachmentMetadata(messageID int64, contentHash, mediaType string, media waMedia) {
	var width, height, durationMS sql.NullInt64
	if media.Width.Valid && media.Width.Int64 > 0 {
		width = media.Width
	}
	if media.Height.Valid && media.Height.Int64 > 0 {
		height = media.Height
	}
	if media.MediaDuration.Valid && media.MediaDuration.Int64 > 0 {
		// WhatsApp stores duration in seconds; msgvault uses milliseconds.
		durationMS = sql.NullInt64{Int64: media.MediaDuration.Int64 * 1000, Valid: true}
	}

	_, _ = imp.store.DB().Exec(`
		UPDATE attachments SET media_type = ?, width = ?, height = ?, duration_ms = ?
		WHERE message_id = ? AND (content_hash = ? OR content_hash IS NULL)
	`, mediaType, width, height, durationMS, messageID, contentHash)
}

// lookupMessageByKeyID looks up a previously imported message by its WhatsApp key_id.
// Returns 0 if not found.
func (imp *Importer) lookupMessageByKeyID(sourceID int64, keyID string) (int64, error) {
	var msgID int64
	err := imp.store.DB().QueryRow(
		`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`,
		sourceID, keyID,
	).Scan(&msgID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return msgID, err
}

// setReplyTo sets the reply_to_message_id on a message.
func (imp *Importer) setReplyTo(messageID, replyToID int64) {
	_, _ = imp.store.DB().Exec(`
		UPDATE messages SET reply_to_message_id = ? WHERE id = ?
	`, replyToID, messageID)
}

// verifyWhatsAppDB checks that the database looks like a WhatsApp msgstore.db.
func verifyWhatsAppDB(db *sql.DB) error {
	// Check for the 'message' table with expected columns.
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'message'
	`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check whatsapp db: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("not a valid WhatsApp database: 'message' table not found")
	}

	// Check for the 'jid' table.
	err = db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'jid'
	`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check whatsapp db: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("not a valid WhatsApp database: 'jid' table not found")
	}

	// Check for the 'chat' table.
	err = db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'chat'
	`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check whatsapp db: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("not a valid WhatsApp database: 'chat' table not found")
	}

	return nil
}
