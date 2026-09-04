package beeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// defaultMaxMediaBytes caps individual attachment downloads (config
// max_media_mb overrides). It mirrors the config-layer default exactly.
const (
	defaultMaxMediaBytes      = attachmentpolicy.DefaultChatMaxBytes
	beeperAttachmentTypeImage = "img"
)

// beeperAttachmentID namespaces Beeper-managed attachment rows in
// attachments.source_attachment_id.
func beeperAttachmentID(assetURL string) string {
	return "beeper:" + assetURL
}

// mediaTypeOf maps a Beeper attachment to msgvault's attachments.media_type.
func mediaTypeOf(att *Attachment) string {
	switch {
	case att.IsSticker:
		return "sticker"
	case att.IsGif:
		return "gif"
	case att.IsVoiceNote:
		return "voice_note"
	}
	switch att.Type {
	case beeperAttachmentTypeImage:
		return "image"
	case "video":
		return "video"
	case "audio":
		return "audio"
	default:
		return "document"
	}
}

// assetRef returns the fetchable reference of an attachment (the mxc:// style
// asset ID, falling back to srcURL), or "" when there is nothing to fetch.
func assetRef(att *Attachment) string {
	if att.ID != "" {
		return att.ID
	}
	return att.SrcURL
}

// declaredSize returns the API-reported attachment size clamped to a sane
// non-negative int (the field arrives as float64).
func declaredSize(att *Attachment) int {
	if att.FileSize <= 0 || att.FileSize > float64(1<<62) {
		return 0
	}
	return int(int64(att.FileSize))
}

func recordOverCap(sum *ImportSummary, size int64, lowerBound bool) {
	if sum.AttachmentsOverCap < math.MaxInt64 {
		sum.AttachmentsOverCap++
	}
	if size < 0 {
		size = 0
		lowerBound = true
	}
	if size > math.MaxInt64-sum.AttachmentsOverCapBytes {
		sum.AttachmentsOverCapBytes = math.MaxInt64
		lowerBound = true
	} else {
		sum.AttachmentsOverCapBytes += size
	}
	if lowerBound && sum.AttachmentsOverCapUnknownSize < math.MaxInt64 {
		sum.AttachmentsOverCapUnknownSize++
	}
}

// shareMetadata renders the attachment_metadata JSON marking media that came
// in as a link preview rather than as something the sender composed, recording
// the URL it previews. Returns "" for ordinary media, which stores NULL.
func shareMetadata(m *Message) string {
	link := sharedLink(m)
	if link == "" {
		return ""
	}
	// Marshal rather than concatenate: the URL is untrusted remote input and
	// must not be able to break out of the JSON value.
	b, err := json.Marshal(struct {
		SharedURL string `json:"shared_url"`
	}{SharedURL: link})
	if err != nil {
		return ""
	}
	return string(b)
}

// persistAttachments downloads a message's media into content-addressed
// storage and replaces the message's Beeper attachment rows. Media already
// downloaded for this message (matched by source_attachment_id) is kept
// as-is, so re-persisting a message never re-fetches its attachments. Failed
// downloads and deliberate policy exclusions leave typed metadata markers;
// BackfillMedia retries only outcomes allowed by the current policy. The
// message itself is always archived regardless.
func (imp *Importer) persistAttachments(ctx context.Context, syncID, messageID int64, m *Message, opts ImportOptions, sum *ImportSummary) {
	existing, err := imp.store.MessageBeeperAttachments(messageID)
	if err != nil {
		sum.Errors++
		return
	}
	// Media removed at the source (e.g. an edit) falls through with zero
	// refs: the replace below clears the stale rows, including pending
	// markers that BackfillMedia would otherwise revisit forever.
	if len(m.Attachments) == 0 && len(existing) == 0 {
		return
	}
	maxBytes := opts.MaxMediaBytes
	if opts.MediaPolicy.MaxBytes > 0 {
		maxBytes = opts.MediaPolicy.MaxBytes
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxMediaBytes
	}
	policy := opts.MediaPolicy
	policy.MaxBytes = maxBytes
	shareMeta := shareMetadata(m)
	isPreview := shareMeta != ""
	refs := make([]store.AttachmentRef, 0, len(m.Attachments))
	for i := range m.Attachments {
		att := &m.Attachments[i]
		ref := assetRef(att)
		if ref == "" {
			continue
		}
		sourceAttID := beeperAttachmentID(ref)
		previous, hadPrevious := existing[sourceAttID]
		if hadPrevious && previous.ContentHash != "" {
			// Re-persisting already-downloaded media: keep the blob as-is but
			// refresh the share marker, so re-running over an existing archive
			// classifies rows stored before this was recorded.
			previous.Metadata = shareMeta
			setBeeperAttachmentRole(&previous, att, isPreview)
			refs = append(refs, previous)
			continue
		}
		marker := store.AttachmentRef{
			Filename:           att.FileName,
			MimeType:           att.MimeType,
			StoragePath:        ref,
			Size:               declaredSize(att),
			SourceAttachmentID: sourceAttID,
			Metadata:           shareMeta,
			State:              attachmentpolicy.StatePending,
		}
		if hadPrevious && previous.Size > marker.Size {
			marker.Size = previous.Size
		}
		// Every unsuccessful fetch leaves a typed marker. Transient failures
		// remain retryable; size exclusions wait until the cap is raised.
		pend := func(status, kind string, sizeUnknown bool, err error) {
			newSizeSkip := status != store.SyncRunItemStatusSkipped || !hadPrevious ||
				previous.State != attachmentpolicy.StateSkipped ||
				previous.SkipReason != attachmentpolicy.SkipSizeCap ||
				int64(previous.Size) <= maxBytes
			if newSizeSkip {
				imp.recordItem(syncID, m.ID, "attachment", status, kind, err)
			}
			if status == store.SyncRunItemStatusSkipped {
				marker.State = attachmentpolicy.StateSkipped
				marker.SkipReason = attachmentpolicy.SkipSizeCap
			} else {
				marker.State = attachmentpolicy.StateFailed
				marker.SkipReason = attachmentpolicy.SkipFetchFailure
			}
			refs = append(refs, marker)
			if marker.State == attachmentpolicy.StateSkipped {
				if newSizeSkip {
					sum.AttachmentsSkipped++
					recordOverCap(sum, int64(marker.Size), sizeUnknown)
				}
			} else {
				sum.AttachmentsPending++
			}
			if status == store.SyncRunItemStatusError {
				sum.Errors++
			}
		}
		if opts.NoMedia || opts.AttachmentsDir == "" {
			refs = append(refs, marker)
			sum.AttachmentsPending++
			continue
		}
		if reason := policy.Evaluate(opts.MediaConversation, int64(marker.Size)); reason != "" {
			if reason == attachmentpolicy.SkipSizeCap {
				marker.SkipReason = reason
				pend(store.SyncRunItemStatusSkipped, "beeper_media_too_large", false,
					fmt.Errorf("attachment %s is %d bytes (cap %d)", ref, marker.Size, maxBytes))
				continue
			}
			marker.State = attachmentpolicy.StateSkipped
			marker.SkipReason = reason
			imp.recordItem(syncID, m.ID, "attachment", store.SyncRunItemStatusSkipped, "beeper_media_policy", nil)
			refs = append(refs, marker)
			sum.AttachmentsSkipped++
			continue
		}
		if att.FileSize > float64(maxBytes) {
			observed := int64(math.MaxInt64)
			if att.FileSize < float64(math.MaxInt64) {
				observed = int64(att.FileSize)
			}
			marker.Size = attachmentpolicy.OversizeMarkerSize(maxBytes, observed)
			pend(store.SyncRunItemStatusSkipped, "beeper_media_too_large", true,
				fmt.Errorf("attachment %s is at least %d bytes (cap %d)", ref, marker.Size, maxBytes))
			continue
		}
		data, err := imp.client.GetAssetBytes(ctx, ref, maxBytes)
		if err == nil && len(data) == 0 {
			err = errors.New("empty asset response")
		}
		if errors.Is(err, ErrAssetTooLarge) {
			// The declared fileSize was absent or wrong; the capped reader
			// caught it. Same outcome as the declared-size check above.
			marker.Size = attachmentpolicy.OversizeMarkerSize(maxBytes, int64(marker.Size))
			pend(store.SyncRunItemStatusSkipped, "beeper_media_too_large", true, err)
			continue
		}
		if err != nil {
			pend(store.SyncRunItemStatusError, "beeper_media_error", false, err)
			continue
		}
		ma := &mime.Attachment{Filename: att.FileName, ContentType: att.MimeType, Content: data}
		storagePath, serr := export.StoreAttachmentFile(opts.AttachmentsDir, ma)
		if serr != nil || storagePath == "" {
			pend(store.SyncRunItemStatusError, "beeper_media_error", false, serr)
			continue
		}
		stored := store.AttachmentRef{
			Filename:           att.FileName,
			MimeType:           att.MimeType,
			StoragePath:        storagePath,
			ContentHash:        ma.ContentHash,
			Size:               len(data),
			SourceAttachmentID: sourceAttID,
			MediaType:          mediaTypeOf(att),
			DurationMS:         int64(att.Duration * 1000),
			Metadata:           shareMeta,
			State:              attachmentpolicy.StateStored,
		}
		setBeeperAttachmentRole(&stored, att, isPreview)
		if att.Size != nil {
			stored.Width = int64(att.Size.Width)
			stored.Height = int64(att.Size.Height)
		}
		refs = append(refs, stored)
		sum.AttachmentsDownloaded++
	}
	if err := imp.store.ReplaceMessageBeeperAttachments(messageID, refs); err != nil {
		sum.Errors++
		return
	}
	if err := imp.store.RecomputeMessageAttachmentStats(messageID); err != nil {
		sum.Errors++
	}
}

func setBeeperAttachmentRole(ref *store.AttachmentRef, att *Attachment, isPreview bool) {
	if att.IsSticker {
		ref.Role = store.AttachmentRoleSticker
		ref.RoleSource = store.AttachmentRoleSourceProviderExplicit
		return
	}
	if isPreview {
		ref.Role = store.AttachmentRolePreview
		ref.RoleSource = store.AttachmentRoleSourceImporterSemantics
		return
	}
	ref.Role = store.AttachmentRoleStandalone
	ref.RoleSource = store.AttachmentRoleSourceImporterSemantics
}

// chatRefresh is one chat's re-read source context: its current conversation
// type and membership, found=false for a chat the source no longer has, or a
// non-nil err for one it could not read.
type chatRefresh struct {
	conversationType string
	membership       chatMembership
	found            bool
	err              error
}

// refreshChatContext re-reads one chat from the source and reconciles the
// archived conversation type and membership record with it, so this backfill
// and every later policy evaluation weigh the source's current truth rather
// than a type or roster the archive predates. The returned error is fatal
// (store failure); a fetch failure is carried in the result instead.
func (imp *Importer) refreshChatContext(
	ctx context.Context, syncID, sourceID int64, chatID string, sum *ImportSummary,
) (*chatRefresh, error) {
	chat, gerr := imp.client.GetChat(ctx, chatID)
	if errors.Is(gerr, ErrNotFound) {
		return &chatRefresh{}, nil
	}
	if gerr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		imp.recordItem(syncID, chatID, "fetch", store.SyncRunItemStatusError, "beeper_fetch_error", gerr)
		sum.FetchErrors++
		sum.Errors++
		return &chatRefresh{err: gerr}, nil
	}
	refresh := &chatRefresh{
		conversationType: conversationType(chat.Type),
		membership:       chatMembershipOf(chat, !chat.Participants.HasMore),
		found:            true,
	}
	convID, err := imp.store.EnsureConversationWithType(sourceID, chatID, refresh.conversationType, chat.Title)
	if err != nil {
		return nil, err
	}
	if refresh.membership.known {
		err = imp.store.SetConversationMemberCount(convID, refresh.membership.count)
	} else {
		err = imp.store.MarkConversationMemberCountUnknown(convID)
	}
	if err != nil {
		return nil, err
	}
	return refresh, nil
}

// clearPendingMarkers removes a message's pending Beeper markers while
// preserving its downloaded (content-hashed) attachment rows.
func (imp *Importer) clearPendingMarkers(messageID int64) error {
	existing, err := imp.store.MessageBeeperAttachments(messageID)
	if err != nil {
		return err
	}
	keep := make([]store.AttachmentRef, 0, len(existing))
	for _, ref := range existing {
		if ref.ContentHash != "" {
			keep = append(keep, ref)
		}
	}
	if err := imp.store.ReplaceMessageBeeperAttachments(messageID, keep); err != nil {
		return err
	}
	return imp.store.RecomputeMessageAttachmentStats(messageID)
}

// BackfillMedia retries eligible Beeper attachment downloads for one account:
// messages with unfinished work, plus exclusions now allowed by current
// policy, are re-fetched and re-persisted. Idempotent (content-addressed
// storage, replace-by-prefix rows).
func (imp *Importer) BackfillMedia(ctx context.Context, opts ImportOptions) (*ImportSummary, error) {
	start := time.Now()
	if opts.AttachmentsDir == "" {
		return nil, errors.New("attachments dir required")
	}
	src, err := imp.store.GetOrCreateSource(sourceTypeBeeper, opts.AccountID)
	if err != nil {
		return nil, err
	}
	sum := &ImportSummary{SourceID: src.ID}
	// This run's sync_runs row becomes the source's newest completed run, and
	// Import loads its cursor_after as the resume baseline — so carry the
	// existing sync state forward verbatim or the next sync would restart
	// from scratch.
	state := imp.loadResumeState(src.ID)
	syncID, err := imp.store.StartSync(src.ID, "beeper_media")
	if err != nil {
		return nil, err
	}
	imp = imp.scopedToSync(src.ID, syncID)
	defer func() {
		if err != nil {
			_ = imp.store.FailSync(syncID, err.Error())
		}
	}()
	// Preserve the carried-forward state even if this run is interrupted.
	imp.checkpointNow(syncID, state, sum)

	policy := opts.MediaPolicy
	if policy.MaxBytes <= 0 {
		policy.MaxBytes = opts.MaxMediaBytes
	}
	if policy.MaxBytes <= 0 {
		policy.MaxBytes = defaultMaxMediaBytes
	}
	policyResult, skipErr := imp.store.ApplyBeeperRetryableAttachmentPolicy(ctx, src.ID, policy)
	if skipErr != nil {
		err = skipErr
		return sum, err
	}
	sum.AttachmentsSkipped += policyResult.NewlySkipped
	sum.AttachmentsOverCap += policyResult.AttachmentsOverCap
	sum.AttachmentsOverCapBytes += policyResult.AttachmentsOverCapBytes
	sum.AttachmentsOverCapUnknownSize += policyResult.AttachmentsOverCapUnknownSize
	pending, err := imp.store.ListBeeperRetryableAttachmentMessages(src.ID, policy)
	if err != nil {
		return sum, err
	}
	if len(pending) > 0 || !policyResult.HasExcluded {
		// Preserve the established guard-refresh behavior for ordinary media
		// runs. Only a pass whose remaining work was entirely excluded by the
		// current policy can safely remain local.
		if err = imp.verifyAnchors(ctx, syncID, src.ID, state); err != nil {
			return sum, err
		}
		// This run's state becomes the newest completed baseline: like the main
		// sync, never persist it with the reinstall guard weakened.
		imp.rearmAnchors(ctx, nil, state)
	}
	stateBlob, merr := state.Marshal()
	if merr != nil {
		err = merr
		return sum, err
	}
	// A direct-scope policy decides on the conversation type alone, and the
	// archived type may predate the mapping of Beeper rooms to channels — a
	// legacy group_chat row would admit a room the current mapping excludes.
	// Re-read each chat's type once from the source before evaluating.
	chatRefreshes := map[string]*chatRefresh{}
	for _, item := range pending {
		if err = ctx.Err(); err != nil {
			return sum, err
		}
		if policy.Scope == attachmentpolicy.ScopeDirect {
			refresh, cached := chatRefreshes[item.ChatID]
			if !cached {
				refresh, err = imp.refreshChatContext(ctx, syncID, src.ID, item.ChatID, sum)
				if err != nil {
					return sum, err
				}
				chatRefreshes[item.ChatID] = refresh
			}
			if refresh.err != nil {
				// The type cannot be resolved, so scope cannot be evaluated.
				// The marker stays retryable rather than trusting the archive.
				sum.AttachmentsPending++
				continue
			}
			if refresh.found {
				item.ConversationType = refresh.conversationType
				item.ParticipantCount = refresh.membership.policyCount(policy)
			}
			// A gone chat falls through: the message fetch below observes it
			// and clears the markers.
		}
		m, gerr := imp.client.GetMessage(ctx, item.ChatID, item.SourceMessageID)
		if errors.Is(gerr, ErrNotFound) {
			// The source message is permanently gone; its pending media can
			// never be fetched. Clear the markers — but keep the message's
			// already-downloaded rows, which remain valid archived media.
			imp.recordItem(syncID, item.SourceMessageID, "attachment", store.SyncRunItemStatusSkipped, "beeper_media_source_gone", gerr)
			if rerr := imp.clearPendingMarkers(item.MessageID); rerr != nil {
				sum.Errors++
			}
			continue
		}
		if gerr != nil {
			// Transient failure (outage, auth): the marker stays — count it
			// still pending so the summary cannot claim a clean state — and
			// the run reports the error.
			imp.recordItem(syncID, item.SourceMessageID, "attachment", store.SyncRunItemStatusError, "beeper_media_error", gerr)
			sum.AttachmentsPending++
			sum.FetchErrors++
			sum.Errors++
			continue
		}
		itemOpts := opts
		itemOpts.MediaConversation = attachmentpolicy.Conversation{
			Type: item.ConversationType, ParticipantCount: item.ParticipantCount,
		}
		imp.persistAttachments(ctx, syncID, item.MessageID, m, itemOpts, sum)
		sum.MessagesProcessed++
		if opts.Progress != nil && sum.MessagesProcessed%100 == 0 {
			opts.Progress(fmt.Sprintf("media backfill: %d messages, %d downloaded, %d still pending",
				sum.MessagesProcessed, sum.AttachmentsDownloaded, sum.AttachmentsPending))
		}
	}
	// The initial checkpoint carries resume state, but its counters predate
	// all media work. Persist the final summary before completing the run so
	// diagnostics and monitoring agree with the returned result.
	imp.checkpointNow(syncID, state, sum)
	if err = imp.store.CompleteSync(syncID, stateBlob); err != nil {
		return sum, err
	}
	sum.Duration = time.Since(start)
	return sum, nil
}
