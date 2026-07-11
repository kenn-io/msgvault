package beeper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// defaultMaxMediaBytes caps individual attachment downloads (config
// max_media_mb overrides).
const defaultMaxMediaBytes = int64(100 << 20)

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
	case "img":
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

// persistAttachments downloads a message's media into content-addressed
// storage and replaces the message's Beeper attachment rows. Media already
// downloaded for this message (matched by source_attachment_id) is kept
// as-is, so re-persisting a message never re-fetches its attachments. Failed
// or over-cap downloads leave a pending marker row (the asset URL in
// storage_path, no content hash) so BackfillMedia can retry them; the message
// itself is always archived regardless.
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
	if maxBytes <= 0 {
		maxBytes = defaultMaxMediaBytes
	}
	refs := make([]store.AttachmentRef, 0, len(m.Attachments))
	for i := range m.Attachments {
		att := &m.Attachments[i]
		ref := assetRef(att)
		if ref == "" {
			continue
		}
		sourceAttID := beeperAttachmentID(ref)
		if prev, ok := existing[sourceAttID]; ok && prev.ContentHash != "" {
			refs = append(refs, prev)
			continue
		}
		marker := store.AttachmentRef{
			Filename:           att.FileName,
			MimeType:           att.MimeType,
			StoragePath:        ref,
			Size:               declaredSize(att),
			SourceAttachmentID: sourceAttID,
		}
		// Every failure leaves the marker as a pending row (BackfillMedia
		// retries it); only unexpected failures also count as errors.
		pend := func(status, kind string, err error) {
			imp.recordItem(syncID, m.ID, "attachment", status, kind, err)
			refs = append(refs, marker)
			sum.AttachmentsPending++
			if status == store.SyncRunItemStatusError {
				sum.Errors++
			}
		}
		if att.FileSize > 0 && int64(att.FileSize) > maxBytes {
			pend(store.SyncRunItemStatusSkipped, "beeper_media_too_large",
				fmt.Errorf("attachment %s is %d bytes (cap %d)", ref, int64(att.FileSize), maxBytes))
			continue
		}
		data, err := imp.client.GetAssetBytes(ctx, ref, maxBytes)
		if err == nil && len(data) == 0 {
			err = errors.New("empty asset response")
		}
		if errors.Is(err, ErrAssetTooLarge) {
			// The declared fileSize was absent or wrong; the capped reader
			// caught it. Same outcome as the declared-size check above.
			pend(store.SyncRunItemStatusSkipped, "beeper_media_too_large", err)
			continue
		}
		if err != nil {
			pend(store.SyncRunItemStatusError, "beeper_media_error", err)
			continue
		}
		ma := &mime.Attachment{Filename: att.FileName, ContentType: att.MimeType, Content: data}
		storagePath, serr := export.StoreAttachmentFile(opts.AttachmentsDir, ma)
		if serr != nil || storagePath == "" {
			pend(store.SyncRunItemStatusError, "beeper_media_error", serr)
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
		}
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

// BackfillMedia retries pending Beeper attachment downloads for one account:
// every message that still has a pending marker is re-fetched from the API
// and its attachments re-persisted. Idempotent (content-addressed storage,
// replace-by-prefix rows).
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
	defer func() {
		if err != nil {
			_ = imp.store.FailSync(syncID, err.Error())
		}
	}()
	// Preserve the carried-forward state even if this run is interrupted.
	imp.checkpointNow(syncID, state, sum)

	// This pass trusts stored message IDs, which are only unique per Beeper
	// installation — the same guard the main sync runs applies here, or a
	// reinstall could attach some other message's media to archived rows.
	if err = imp.verifyAnchors(ctx, syncID, src.ID, state); err != nil {
		return sum, err
	}
	// This run's state becomes the newest completed baseline: like the main
	// sync, never persist it with the reinstall guard weakened.
	imp.rearmAnchors(ctx, nil, state)
	stateBlob, merr := state.Marshal()
	if merr != nil {
		err = merr
		return sum, err
	}

	var pending []store.BeeperPendingAttachmentMessage
	pending, err = imp.store.ListBeeperPendingAttachmentMessages(src.ID)
	if err != nil {
		return sum, err
	}
	for _, item := range pending {
		if err = ctx.Err(); err != nil {
			return sum, err
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
		imp.persistAttachments(ctx, syncID, item.MessageID, m, opts, sum)
		sum.MessagesProcessed++
		if opts.Progress != nil && sum.MessagesProcessed%100 == 0 {
			opts.Progress(fmt.Sprintf("media backfill: %d messages, %d downloaded, %d still pending",
				sum.MessagesProcessed, sum.AttachmentsDownloaded, sum.AttachmentsPending))
		}
	}
	if err = imp.store.CompleteSync(syncID, stateBlob); err != nil {
		return sum, err
	}
	sum.Duration = time.Since(start)
	return sum, nil
}
