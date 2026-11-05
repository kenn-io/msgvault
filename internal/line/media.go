package line

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"mime"
	"net/http"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
)

func mediaMessage(kind string) bool {
	switch kind {
	case "image", "video", "audio", "file":
		return true
	}
	return false
}

// media stores a provider occurrence before fetching bytes. Its stable key and
// terminal states make retries/restarts safe, including zero-byte content.
func (p *Processor) media(ctx context.Context, messageID int64, e Event, deleted bool) error {
	if e.Message == nil || !mediaMessage(e.Message.Type) {
		return nil
	}
	m := e.Message
	if m.Type != "file" && m.ContentProvider != nil && m.ContentProvider.Type == "external" {
		return nil
	}
	refs, err := p.store.MessageProviderAttachments(messageID, "line:")
	if err != nil {
		return err
	}
	key := "line:" + m.ID
	if prior, found := refs[key]; found {
		if !attachmentpolicy.RetryEligible(prior.State) {
			return nil
		}
	}
	filename := m.FileName
	if filename == "" {
		filename = m.Type + "-" + m.ID
	}
	raw := m.Raw
	if len(raw) == 0 {
		raw, err = json.Marshal(m)
		if err != nil {
			return err
		}
	}
	write := store.AttachmentWrite{Filename: filename, MIMEType: "application/octet-stream", SourceAttachmentID: key, SourcePartKey: key, MediaType: m.Type, DurationMS: m.Duration, Metadata: string(raw), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceProviderExplicit, State: attachmentpolicy.StatePending, Size: max(m.FileSize, 0)}
	save := func(state attachmentpolicy.DownloadState, reason attachmentpolicy.SkipReason) error {
		write.State = state
		write.SkipReason = reason
		if err := p.store.UpsertAttachmentRecordWithStats(ctx, messageID, write, false); err != nil {
			return err
		}
		jobctx.RecordProgress(ctx)
		return nil
	}
	if deleted {
		return save(attachmentpolicy.StateUnavailable, attachmentpolicy.SkipSourceUnavailable)
	}
	if m.Type != "file" && m.ContentProvider != nil && m.ContentProvider.Type != "" && m.ContentProvider.Type != "line" {
		return save(attachmentpolicy.StateUnavailable, attachmentpolicy.SkipSourceUnavailable)
	}
	policy := p.mediaPolicy
	if reason := policy.Evaluate(attachmentpolicy.Conversation{}, m.FileSize); reason != "" {
		return save(attachmentpolicy.StateSkipped, reason)
	}
	if _, found := refs[key]; !found {
		if err = save(attachmentpolicy.StatePending, ""); err != nil {
			return err
		}
	}
	response, err := p.client.Content(ctx, m.ID, p.mediaPolicy.MaxBytes)
	if err != nil {
		if saveErr := save(attachmentpolicy.StateFailed, attachmentpolicy.SkipFetchFailure); saveErr != nil {
			return saveErr
		}
		return retryEvent(err)
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusAccepted:
		if err = save(attachmentpolicy.StatePending, ""); err != nil {
			return err
		}
		return retryEvent(errors.New("LINE media is still being prepared"))
	case http.StatusNotFound, http.StatusGone:
		return save(attachmentpolicy.StateUnavailable, attachmentpolicy.SkipSourceUnavailable)
	}
	if parsed, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type")); parseErr == nil {
		write.MIMEType = parsed
	}
	source := &export.SourceReader{R: response.Body}
	path, hash, size, err := p.storeMediaBody(ctx, source, response.ContentLength)
	if errors.Is(err, export.ErrAttachmentTooLarge) {
		write.Size = size
		return save(attachmentpolicy.StateSkipped, attachmentpolicy.SkipSizeCap)
	}
	if err != nil {
		if source.Err == nil && ctx.Err() == nil {
			return fmt.Errorf("store LINE media: %w", err)
		}
		if saveErr := save(attachmentpolicy.StateFailed, attachmentpolicy.SkipFetchFailure); saveErr != nil {
			return saveErr
		}
		return retryEvent(fmt.Errorf("fetch LINE media: %w", err))
	}
	write.StoragePath = path
	write.ContentHash = hash
	write.Size = size
	return save(attachmentpolicy.StateStored, "")
}

// storeMediaBody streams fetched bytes into storage. An oversized body reports
// ErrAttachmentTooLarge with the size the skip marker should record.
func (p *Processor) storeMediaBody(ctx context.Context, source *export.SourceReader, contentLength int64) (string, string, int64, error) {
	maxBytes := p.mediaPolicy.MaxBytes
	if contentLength > maxBytes {
		return "", "", int64(attachmentpolicy.OversizeMarkerSize(maxBytes, contentLength)), export.ErrAttachmentTooLarge
	}
	path, hash, size, err := export.StoreAttachmentStream(ctx, p.attachmentsDir, source, maxBytes)
	if errors.Is(err, export.ErrAttachmentTooLarge) {
		size = int64(attachmentpolicy.OversizeMarkerSize(maxBytes, 0))
	}
	return path, hash, size, err
}

func (p *Processor) finishUnstoredMedia(ctx context.Context, messageID int64, state attachmentpolicy.DownloadState, reason attachmentpolicy.SkipReason) error {
	refs, err := p.store.MessageProviderAttachments(messageID, "line:")
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if !attachmentpolicy.RetryEligible(ref.State) {
			continue
		}
		write := store.AttachmentWrite{Filename: ref.Filename, MIMEType: ref.MimeType, Size: int64(ref.Size), SourceAttachmentID: ref.SourceAttachmentID, SourcePartKey: ref.SourcePartKey, MediaType: ref.MediaType, DurationMS: ref.DurationMS, Metadata: ref.Metadata, Role: ref.Role, RoleSource: ref.RoleSource, State: state, SkipReason: reason}
		if err = p.store.UpsertAttachmentRecordWithStats(ctx, messageID, write, false); err != nil {
			return err
		}
		jobctx.RecordProgress(ctx)
	}
	return nil
}

func (p *Processor) expireMedia(ctx context.Context, sourceMessageID string) error {
	if sourceMessageID == "" {
		return nil
	}
	message, err := p.store.LineMessageContext(ctx, p.sourceID, sourceMessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return p.finishUnstoredMedia(ctx, message.ID, attachmentpolicy.StateFailed, attachmentpolicy.SkipFetchFailure)
}
