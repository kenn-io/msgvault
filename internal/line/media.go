package line

import (
	"context"
	"database/sql"
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
func (p *Processor) media(ctx context.Context, messageID int64, e Event, deleted bool) (bool, error) {
	if e.Message == nil || !mediaMessage(e.Message.Type) {
		return false, nil
	}
	m := e.Message
	if m.Type != "file" && m.ContentProvider != nil && m.ContentProvider.Type == "external" {
		return false, nil
	}
	refs, err := p.store.MessageProviderAttachments(messageID, "line:")
	if err != nil {
		return false, err
	}
	key := "line:" + m.ID
	if prior, found := refs[key]; found && !p.mediaPolicy.RetryAllows(prior.State, attachmentpolicy.Conversation{}, int64(prior.Size)) {
		return false, nil
	}
	filename := m.FileName
	if filename == "" {
		filename = m.Type + "-" + m.ID
	}
	write := store.AttachmentWrite{Filename: filename, MIMEType: "application/octet-stream", SourceAttachmentID: key, SourcePartKey: key, MediaType: m.Type, DurationMS: m.Duration, Metadata: string(m.Raw), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceProviderExplicit, State: attachmentpolicy.StatePending, Size: max(m.FileSize, 0)}
	save := func(state attachmentpolicy.DownloadState, reason attachmentpolicy.SkipReason) error {
		write.State = state
		write.SkipReason = reason
		if err := p.store.UpsertLineAttachmentContext(ctx, messageID, write); err != nil {
			return err
		}
		jobctx.RecordProgress(ctx)
		return nil
	}
	if deleted {
		return false, save(attachmentpolicy.StateUnavailable, attachmentpolicy.SkipSourceUnavailable)
	}
	if m.Type != "file" && m.ContentProvider != nil && m.ContentProvider.Type != "" && m.ContentProvider.Type != "line" {
		return false, save(attachmentpolicy.StateUnavailable, attachmentpolicy.SkipSourceUnavailable)
	}
	policy := p.mediaPolicy
	if reason := policy.Evaluate(attachmentpolicy.Conversation{}, m.FileSize); reason != "" {
		return false, save(attachmentpolicy.StateSkipped, reason)
	}
	if _, found := refs[key]; !found {
		if err = save(attachmentpolicy.StatePending, ""); err != nil {
			return false, err
		}
	}
	response, err := p.client.Content(ctx, m.ID, p.mediaPolicy.MaxBytes)
	if err != nil {
		if saveErr := save(attachmentpolicy.StateFailed, attachmentpolicy.SkipFetchFailure); saveErr != nil {
			return false, saveErr
		}
		return false, retryEvent(err)
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusAccepted:
		if err = save(attachmentpolicy.StatePending, ""); err != nil {
			return false, err
		}
		return false, retryEvent(errors.New("LINE media is still being prepared"))
	case http.StatusNotFound, http.StatusGone:
		return false, save(attachmentpolicy.StateUnavailable, attachmentpolicy.SkipSourceUnavailable)
	}
	if parsed, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type")); parseErr == nil {
		write.MIMEType = parsed
	}
	source := &export.SourceReader{R: response.Body}
	path, hash, size, err := p.storeMediaBody(ctx, source, response.ContentLength)
	if errors.Is(err, export.ErrAttachmentTooLarge) {
		write.Size = size
		return false, save(attachmentpolicy.StateSkipped, attachmentpolicy.SkipSizeCap)
	}
	if err != nil {
		if source.Err == nil && ctx.Err() == nil {
			return false, fmt.Errorf("store LINE media: %w", err)
		}
		if saveErr := save(attachmentpolicy.StateFailed, attachmentpolicy.SkipFetchFailure); saveErr != nil {
			return false, saveErr
		}
		return false, retryEvent(fmt.Errorf("fetch LINE media: %w", err))
	}
	write.StoragePath = path
	write.ContentHash = hash
	write.Size = size
	err = save(attachmentpolicy.StateStored, "")
	return err == nil, err
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
		if err = p.store.UpsertLineAttachmentContext(ctx, messageID, write); err != nil {
			return err
		}
		p.recordMutation(ctx, 0, 1)
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
