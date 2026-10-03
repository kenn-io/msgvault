package bland

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/meetingrecording"
	"go.kenn.io/msgvault/internal/store"
)

type StoredRecording struct {
	State      attachmentpolicy.DownloadState `json:"state,omitempty"`
	SkipReason attachmentpolicy.SkipReason    `json:"skip_reason,omitempty"`
	Hash       string                         `json:"hash,omitempty"`
	Path       string                         `json:"path,omitempty"`
	Size       int64                          `json:"size,omitempty"`
	MIME       string                         `json:"mime,omitempty"`
}

func (imp *Importer) record(ctx context.Context, messageID int64, c *Call, previous StoredRecording, o ImportOptions, expired bool) (StoredRecording, error) {
	r := previous
	if r.State == attachmentpolicy.StateUnavailable && !o.Full {
		return r, nil
	}
	if r.Hash != "" && r.Path != "" {
		// Audio may already be durable even when its attachment occurrence
		// write failed. Reconcile the occurrence from the descriptor on retry.
		return r, imp.writeRecording(ctx, messageID, c.ID, r)
	}
	if !o.Full && r.State == attachmentpolicy.StateSkipped && r.SkipReason == attachmentpolicy.SkipSizeCap {
		// A known size cap is terminal for ordinary retries. A full sync can
		// reconsider it after the media policy changes.
		return r, imp.writeRecording(ctx, messageID, c.ID, r)
	}
	if !c.Record && c.RecordingURL == "" && r.State == "" {
		return r, nil
	}
	r.State = attachmentpolicy.StatePending
	r.SkipReason = ""
	conv := attachmentpolicy.Conversation{Type: "meeting", ParticipantCount: 2}
	if reason := o.MediaPolicy.Evaluate(conv, 0); reason != "" {
		r.State = attachmentpolicy.StateSkipped
		r.SkipReason = reason
		return r, imp.writeRecording(ctx, messageID, c.ID, r)
	}
	media, err := imp.client.OpenRecording(ctx, c.ID)
	if errors.Is(err, ErrNotFound) {
		if expired {
			r.State = attachmentpolicy.StateUnavailable
			r.SkipReason = attachmentpolicy.SkipSourceUnavailable
		}
		return r, imp.writeRecording(ctx, messageID, c.ID, r)
	}
	if err != nil {
		r.State = attachmentpolicy.StateFailed
		return r, errors.Join(err, imp.writeRecording(ctx, messageID, c.ID, r))
	}
	defer func() { _ = media.Body.Close() }()
	if reason := o.MediaPolicy.Evaluate(conv, media.Size); reason != "" {
		r.State = attachmentpolicy.StateSkipped
		r.SkipReason = reason
		return r, imp.writeRecording(ctx, messageID, c.ID, r)
	}
	maxBytes := o.MediaPolicy.MaxBytes
	if maxBytes <= 0 {
		maxBytes = attachmentpolicy.DefaultChatMaxBytes
	}
	result, err := meetingrecording.StoreAudio(ctx, o.AttachmentsDir, media.Body, maxBytes)
	if err != nil {
		r.State = attachmentpolicy.StateFailed
		if errors.Is(err, meetingrecording.ErrSizeLimit) {
			r.State = attachmentpolicy.StateSkipped
			r.SkipReason = attachmentpolicy.SkipSizeCap
			return r, imp.writeRecording(ctx, messageID, c.ID, r)
		}
		return r, errors.Join(err, imp.writeRecording(ctx, messageID, c.ID, r))
	}
	r = StoredRecording{State: attachmentpolicy.StateStored, Hash: result.Hash, Size: result.Size, MIME: media.MIME}
	r.Path = result.Path
	return r, imp.writeRecording(ctx, messageID, c.ID, r)
}
func (imp *Importer) writeRecording(ctx context.Context, messageID int64, id string, r StoredRecording) error {
	filename := "call-recording.mp3"
	if r.MIME == "audio/wav" {
		filename = "call-recording.wav"
	}
	return imp.store.UpsertAttachmentRecordWithStats(ctx, messageID, store.AttachmentWrite{
		Filename: filename, MIMEType: r.MIME, StoragePath: r.Path, ContentHash: r.Hash, Size: r.Size,
		SourceAttachmentID: "bland:recording:" + id, SourcePartKey: "bland:recording:" + id, MediaType: "audio",
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, State: r.State, SkipReason: r.SkipReason,
	}, true)
}
