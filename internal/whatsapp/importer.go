package whatsapp

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

// Importer handles importing WhatsApp messages into the msgvault store.
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

// Import detects the WhatsApp database schema and imports its messages.
func (imp *Importer) Import(ctx context.Context, waDBPath string, opts ImportOptions) (*ImportSummary, error) {
	startTime := time.Now()
	view := *imp
	view.store = imp.store.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill})
	imp = &view
	summary := &ImportSummary{}

	waDB, err := openReadOnlyDatabase(ctx, waDBPath)
	if err != nil {
		return nil, fmt.Errorf("open whatsapp db: %w", err)
	}
	defer func() { _ = waDB.Close() }()

	kind, err := detectDatabaseKind(waDB)
	if err != nil {
		return nil, err
	}
	if err := imp.store.ValidateNativeImportChoice(ctx, "whatsapp", opts.NotifyImports); err != nil {
		return nil, err
	}
	if kind == databaseKindApple {
		return imp.importApple(ctx, waDB, waDBPath, opts)
	}
	if !opts.After.IsZero() || !opts.Before.IsZero() {
		return nil, errors.New("--after and --before are only supported for Apple ChatStorage.sqlite imports")
	}

	// Keep the detailed Android schema checks for compatibility with the
	// existing importer error handling.
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
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(source.ID, syncID)
	imp = &scoped

	// Ensure we complete/fail the sync run on exit.
	var syncErr error
	defer func() {
		if syncErr != nil {
			_ = imp.store.FailSync(syncID, syncErr.Error())
		} else {
			_ = imp.store.CompleteSync(syncID, "")
		}
	}()

	ownershipCtx := context.WithoutCancel(ctx)
	run, err := imp.store.BeginNativeImportAttempt(ownershipCtx, source.ID, "whatsapp", opts.NotifyImports, syncID, 0)
	if err != nil {
		syncErr = err
		return nil, fmt.Errorf("begin Android WhatsApp import: %w", err)
	}
	summary.ImportRunID = run.ID
	imp.store, err = imp.store.WithImportRun(ownershipCtx, run.ID)
	if err != nil {
		syncErr = err
		return summary, fmt.Errorf("bind Android WhatsApp import: %w", err)
	}

	imp.progress.OnStart()

	ownerCounted := false

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

	if len(chats) == 0 {
		if _, err := imp.store.EnsureParticipantByPhone(opts.Phone, opts.DisplayName, "whatsapp"); err != nil {
			syncErr = err
			return nil, fmt.Errorf("ensure empty import owner: %w", err)
		}
		summary.Participants++
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 1000
	}

	totalLimit := opts.Limit
	totalAdded := int64(0)

	for _, chat := range chats {
		if ctx.Err() != nil {
			syncErr = ctx.Err()
			return summary, ctx.Err()
		}

		// Check global limit.
		if totalLimit > 0 && totalAdded >= int64(totalLimit) {
			break
		}

		summary.ChatsProcessed++

		sourceConvID, convType, title := mapConversation(chat)
		imp.progress.OnChatStart(chat.RawString, chatTitle(chat), 0)
		plan := androidConversationPlan{}
		plan.add(opts.Phone, opts.DisplayName, "member")
		if !isGroupChat(chat) {
			if phone := normalizePhone(chat.User, chat.Server); phone != "" {
				plan.add(phone, "", "member")
			}
		} else {
			members, err := fetchGroupParticipants(waDB, chat.RawString)
			if err != nil {
				summary.Errors++
				imp.progress.OnError(fmt.Errorf("fetch group participants for %s: %w", sourceConvID, err))
			} else {
				for _, member := range members {
					if phone := normalizePhone(member.MemberUser, member.MemberServer); phone != "" {
						plan.add(phone, "", mapGroupRole(member.Admin))
					}
				}
			}
		}
		chatParticipantsCounted := false
		importableSeen := false

		// Process messages in batches.
		chatAdded := int64(0)
		afterID := int64(0)

		for {
			if ctx.Err() != nil {
				syncErr = ctx.Err()
				return summary, ctx.Err()
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

				importableSeen = true
				messagePlan := plan.clone()
				senderIndex := -1
				if waMsg.FromMe == 1 {
					senderIndex = 0
				} else {
					phone := ""
					if waMsg.SenderServer.Valid && waMsg.SenderServer.String == "lid" {
						phone = resolveLidSender(waMsg.SenderJIDRowID, waMsg.SenderServer.String, lidMap)
					} else if waMsg.SenderUser.Valid && waMsg.SenderUser.String != "" {
						phone = normalizePhone(waMsg.SenderUser.String, waMsg.SenderServer.String)
					} else if !isGroupChat(chat) {
						phone = normalizePhone(chat.User, chat.Server)
					}
					if phone != "" {
						senderIndex = messagePlan.add(phone, "", "member")
					}
				}
				memberCount := len(messagePlan.participants)
				var nativeReactions []androidReactionPlan
				for _, reaction := range reactionMap[waMsg.RowID] {
					value := mapReaction(reaction)
					if value == "" {
						continue
					}
					phone := opts.Phone
					if reaction.SenderServer.Valid && reaction.SenderServer.String == "lid" {
						phone = resolveLidSender(reaction.SenderJIDRowID, reaction.SenderServer.String, lidMap)
					} else if reaction.SenderUser.Valid && reaction.SenderUser.String != "" {
						phone = normalizePhone(reaction.SenderUser.String, reaction.SenderServer.String)
					}
					if phone == "" {
						continue
					}
					index := -1
					for i, participant := range messagePlan.participants {
						if participant.PhoneNumber == phone {
							index = i
							break
						}
					}
					if index < 0 {
						index = messagePlan.add(phone, "", "")
					}
					nativeReactions = append(nativeReactions, androidReactionPlan{participant: index, value: value, createdAt: time.Unix(reaction.Timestamp/1000, 0).UTC()})
				}
				bodyText := sql.NullString{}
				if waMsg.TextData.Valid && waMsg.TextData.String != "" {
					bodyText = waMsg.TextData
				}
				if media, ok := mediaMap[waMsg.RowID]; ok && media.MediaCaption.Valid && media.MediaCaption.String != "" {
					if bodyText.Valid {
						bodyText.String += "\n\n" + media.MediaCaption.String
					} else {
						bodyText = media.MediaCaption
					}
				}
				rawJSON, err := json.Marshal(waMsg, json.Deterministic(true))
				if err != nil {
					summary.Errors++
					imp.progress.OnError(fmt.Errorf("encode message %s: %w", waMsg.KeyID, err))
					continue
				}
				var readyMedia *store.KeyedAttachmentSnapshot
				preparedMedia := false
				if media, ok := mediaMap[waMsg.RowID]; ok && opts.NotifyImports {
					write := prepareAndroidMedia(media, mapMediaType(waMsg.MessageType), opts)
					preparedMedia = write.StoragePath != ""
					readyMedia = &store.KeyedAttachmentSnapshot{Prefix: "whatsapp:", Writes: []store.AttachmentWrite{write}, PreserveStoredOnFailure: true, InvalidateCacheOnChange: true, PreserveUnmentioned: true}
				}
				messageID, err := imp.store.PersistMessageWithParticipantsContext(ctx, messagePlan.participants, func(ids []int64) *store.MessagePersistData {
					var senderID sql.NullInt64
					if senderIndex >= 0 {
						senderID = sql.NullInt64{Int64: ids[senderIndex], Valid: true}
					}
					members := make([]store.ConversationParticipantRef, 0, len(ids))
					for i, id := range ids[:memberCount] {
						members = append(members, store.ConversationParticipantRef{ParticipantID: id, Role: messagePlan.roles[i]})
					}
					reactions := make([]store.ReactionRef, 0, len(nativeReactions))
					for _, reaction := range nativeReactions {
						reactions = append(reactions, store.ReactionRef{ParticipantID: ids[reaction.participant], Type: reactionTypeEmoji, Value: reaction.value, CreatedAt: reaction.createdAt})
					}
					msg := mapMessage(waMsg, 0, source.ID, senderID)
					return &store.MessagePersistData{Message: &msg, Conversation: &store.ConversationPersistData{SourceConversationID: sourceConvID, ConversationType: convType, Title: title, Participants: members, PreserveExistingParticipants: true}, BodyText: bodyText, PreserveBodyIfOmitted: true, RawMIME: rawJSON, RawFormat: "whatsapp_json", PreserveLabels: true, AddReactionRefs: reactions, ReplyToSourceMessageID: quotedMap[waMsg.RowID].QuotedKeyID, PreserveReplyIfUnresolved: true, KeyedAttachmentSnapshot: readyMedia}
				})
				if err != nil {
					if errors.Is(err, store.ErrReplyPersistence) {
						syncErr = err
						return summary, fmt.Errorf("link reply: %w", err)
					}
					summary.Errors++
					imp.progress.OnError(fmt.Errorf("persist message %s: %w", waMsg.KeyID, err))
					continue
				}
				if !ownerCounted {
					summary.Participants++
					ownerCounted = true
				}
				if !chatParticipantsCounted {
					summary.Participants += int64(len(plan.participants) - 1)
					chatParticipantsCounted = true
				}
				plan = androidConversationPlan{participants: messagePlan.participants[:memberCount], roles: messagePlan.roles[:memberCount]}
				summary.ReactionsAdded += int64(len(nativeReactions))
				summary.MessagesAdded++
				chatAdded++
				totalAdded++

				if readyMedia != nil {
					summary.AttachmentsFound++
					if preparedMedia {
						summary.MediaCopied++
					}
				}
				// Silent native imports retain their best-effort media behavior.
				if media, ok := mediaMap[waMsg.RowID]; ok && !opts.NotifyImports {
					summary.AttachmentsFound++
					mediaType := mapMediaType(waMsg.MessageType)

					storagePath, contentHash, storedSize := imp.handleMediaFile(media, opts)
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

					size := int64(0)
					if storagePath != "" {
						size = storedSize
					} else if media.FileSize.Valid {
						size = media.FileSize.Int64
					}

					// Only insert attachment row when we have actual content.
					// Without --media-dir, storagePath and contentHash are both
					// empty; inserting would create broken records.
					if storagePath != "" || contentHash != "" {
						role := store.AttachmentRoleStandalone
						roleSource := store.AttachmentRoleSourceImporterSemantics
						if mediaType == "sticker" {
							role = store.AttachmentRoleSticker
							roleSource = store.AttachmentRoleSourceProviderExplicit
						}
						err := imp.store.UpsertAttachmentRecord(ctx, messageID, store.AttachmentWrite{
							Filename: filename, MIMEType: mimeType, StoragePath: storagePath,
							ContentHash: contentHash, Size: size, MediaType: mediaType,
							Role: role, RoleSource: roleSource, SourcePartKey: "whatsapp:media",
						})
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
							imp.store.Rebind("SELECT COUNT(*) FROM attachments WHERE message_id = ?"),
							messageID,
						).Scan(&existingCount)
						if existingCount == 0 {
							_ = imp.store.RecomputeMessageAttachmentStats(messageID)
						}
					}

					// Store media metadata in the attachments table is done above.
					// For extra metadata (width, height, duration, media_type),
					// update via a direct SQL call for provider dimensions and duration.
					if mediaType != "" || (media.Width.Valid && media.Width.Int64 > 0) {
						if err := imp.updateAttachmentMetadata(ctx, messageID, contentHash, mediaType, media); err != nil {
							syncErr = err
							return nil, fmt.Errorf("update attachment metadata: %w", err)
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

		// Empty/system-only chats retain their native archival trace without
		// manufacturing a message or notification. Failed message snapshots never
		// take this path, so rolled-back identities cannot leak through cleanup.
		if !importableSeen {
			conversationID, err := imp.store.EnsureConversationWithType(source.ID, sourceConvID, convType, title)
			if err != nil {
				summary.Errors++
				imp.progress.OnError(err)
			} else {
				for i, participant := range plan.participants {
					id, err := imp.store.EnsureParticipantByPhone(participant.PhoneNumber, participant.DisplayName, "whatsapp")
					if err != nil {
						summary.Errors++
						imp.progress.OnError(err)
						continue
					}
					if err := imp.store.EnsureConversationParticipant(conversationID, id, plan.roles[i]); err != nil {
						summary.Errors++
						imp.progress.OnError(err)
					}
				}
				if !ownerCounted {
					summary.Participants++
					ownerCounted = true
				}
				summary.Participants += int64(len(plan.participants) - 1)
			}
		}

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

type androidReactionPlan struct {
	participant int
	value       string
	createdAt   time.Time
}

// androidConversationPlan prepares only native descriptors. Participant SQL
// and additive membership commit together with each canonical message.
type androidConversationPlan struct {
	participants []store.ParticipantPersistData
	roles        []string
}

func (p *androidConversationPlan) add(phone, name, role string) int {
	for i, participant := range p.participants {
		if participant.PhoneNumber == phone {
			if name != "" {
				p.participants[i].DisplayName = name
			}
			if role != "" && role != "member" {
				p.roles[i] = role
			}
			return i
		}
	}
	p.participants = append(p.participants, store.ParticipantPersistData{PhoneNumber: phone, DisplayName: name, IdentifierType: "whatsapp"})
	p.roles = append(p.roles, role)
	return len(p.participants) - 1
}

func (p *androidConversationPlan) clone() androidConversationPlan {
	return androidConversationPlan{participants: append([]store.ParticipantPersistData(nil), p.participants...), roles: append([]string(nil), p.roles...)}
}

// handleMediaFile attempts to find and copy a media file to content-addressed storage.
// Returns (storagePath, contentHash, storedSize). All are empty or zero if the
// file is not found.
func (imp *Importer) handleMediaFile(media waMedia, opts ImportOptions) (string, string, int64) {
	fullPath, info := findAndroidMediaFile(media, opts)
	if info == nil || info.Size() > androidMediaSizeLimit(opts) {
		return "", "", 0
	}
	relStoragePath, contentHash, storedSize, err := export.StoreAttachmentFromPath(opts.AttachmentsDir, fullPath, androidMediaSizeLimit(opts))
	if err != nil {
		// Preserve the native silent path's hash when storage fails after hashing.
		return "", contentHash, 0
	}
	return relStoragePath, contentHash, storedSize
}

func androidMediaSizeLimit(opts ImportOptions) int64 {
	if opts.MaxMediaFileSize > 0 {
		return opts.MaxMediaFileSize
	}
	return 100 * 1024 * 1024
}

// findAndroidMediaFile retains native path normalization and basename fallback.
// It runs before the canonical transaction, as do hashing and file storage.
func findAndroidMediaFile(media waMedia, opts ImportOptions) (string, os.FileInfo) {
	if opts.MediaDir == "" || opts.AttachmentsDir == "" || !media.FilePath.Valid || media.FilePath.String == "" {
		return "", nil
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
		return "", nil
	}
	absFullPath, err := filepath.Abs(fullPath)
	if err != nil {
		return "", nil
	}
	if !strings.HasPrefix(absFullPath, absMediaDir+string(filepath.Separator)) && absFullPath != absMediaDir {
		// Path escapes mediaDir — fall back to base filename only.
		fullPath = filepath.Join(mediaDir, filepath.Base(relPath))
		absFullPath, _ = filepath.Abs(fullPath)
		if !strings.HasPrefix(absFullPath, absMediaDir+string(filepath.Separator)) {
			return "", nil
		}
	}

	// Check file exists.
	info, err := os.Stat(fullPath)
	if err != nil {
		// Try just the filename as fallback.
		fullPath = filepath.Join(mediaDir, filepath.Base(relPath))
		info, err = os.Stat(fullPath)
		if err != nil {
			return "", nil
		}
	}

	return fullPath, info
}

func prepareAndroidMedia(media waMedia, mediaType string, opts ImportOptions) store.AttachmentWrite {
	write := store.AttachmentWrite{SourcePartKey: "whatsapp:media", MediaType: mediaType, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, State: attachmentpolicy.StateFailed}
	if mediaType == "sticker" {
		write.Role, write.RoleSource = store.AttachmentRoleSticker, store.AttachmentRoleSourceProviderExplicit
	}
	if media.MimeType.Valid {
		write.MIMEType = media.MimeType.String
	}
	if media.FilePath.Valid {
		write.Filename = filepath.Base(media.FilePath.String)
	}
	if media.FileSize.Valid && media.FileSize.Int64 > 0 {
		write.Size = media.FileSize.Int64
	}
	if media.Width.Valid && media.Width.Int64 > 0 {
		write.Width = media.Width.Int64
	}
	if media.Height.Valid && media.Height.Int64 > 0 {
		write.Height = media.Height.Int64
	}
	if media.MediaDuration.Valid && media.MediaDuration.Int64 > 0 {
		write.DurationMS = media.MediaDuration.Int64 * 1000
	}
	if opts.MediaDir == "" || opts.AttachmentsDir == "" {
		write.State, write.SkipReason = attachmentpolicy.StateSkipped, attachmentpolicy.SkipPolicyScope
		return write
	}
	fullPath, info := findAndroidMediaFile(media, opts)
	if info == nil {
		return write
	}
	maxSize := androidMediaSizeLimit(opts)
	if info.Size() > maxSize {
		write.State, write.SkipReason = attachmentpolicy.StateSkipped, attachmentpolicy.SkipSizeCap
		return write
	}
	storagePath, hash, size, err := export.StoreAttachmentFromPath(opts.AttachmentsDir, fullPath, maxSize)
	if err != nil {
		return write
	}
	write.StoragePath, write.ContentHash, write.Size, write.State = storagePath, hash, size, attachmentpolicy.StateStored
	return write
}

// updateAttachmentMetadata updates media-specific metadata on an attachment record.
func (imp *Importer) updateAttachmentMetadata(
	ctx context.Context, messageID int64, contentHash, mediaType string, media waMedia,
) error {
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

	return imp.store.UpdateAttachmentMediaMetadataContext(
		ctx, messageID, contentHash, mediaType, width, height, durationMS)
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
		return errors.New("not a valid WhatsApp database: 'message' table not found")
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
		return errors.New("not a valid WhatsApp database: 'jid' table not found")
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
		return errors.New("not a valid WhatsApp database: 'chat' table not found")
	}

	return nil
}
