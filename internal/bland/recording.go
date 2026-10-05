package bland

import (
	"context"
	"errors"
	"io"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/callsync"
	"go.kenn.io/msgvault/internal/store"
)

func recordingKey(id string) string { return spec.RowPrefix + id }

// record stores the call's audio once. The attachment row is the record of
// its state: a permanent failure becomes a failed row, and a retryable one
// leaves it pending and is returned as providerErr for the next relist.
// err is a local write failure that stops the run.
func (r *importRun) record(ctx context.Context, messageID int64, c *Call, wanted, counted bool) (providerErr, err error) {
	key := recordingKey(c.ID)
	rows, err := r.St.MessageProviderAttachments(messageID, spec.RowPrefix)
	if err != nil {
		return nil, err
	}
	row, exists := rows[key]
	if exists && (row.ContentHash != "" || !r.Opts.Full && !callsync.Waiting(row)) {
		return nil, nil
	}
	if !exists && !wanted {
		return nil, nil
	}
	write := store.AttachmentWrite{
		Filename: "call-recording.mp3", MIMEType: "audio/mpeg", SourceAttachmentID: key, SourcePartKey: key,
		MediaType: "audio", Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		State: attachmentpolicy.StatePending,
	}
	reason := r.Opts.MediaPolicy.Evaluate(attachmentpolicy.Conversation{Type: "meeting", ParticipantCount: 2}, 0)
	switch {
	case reason != "":
		write.State, write.SkipReason = attachmentpolicy.StateSkipped, reason
	default:
		capBytes := r.Opts.MediaPolicy.MaxBytes
		if capBytes <= 0 {
			capBytes = attachmentpolicy.DefaultChatMaxBytes
		}
		media, openErr := r.imp.client.OpenRecording(ctx, c.ID, capBytes)
		if errors.Is(openErr, ErrNotFound) {
			// Bland may still be preparing the recording; one still missing
			// when the call leaves the relist window ages out unavailable.
			callsync.NotReady(&write)
		} else {
			var storeErr error
			providerErr, storeErr = r.Fetch(ctx, c.ID, func(int64) (io.ReadCloser, error) {
				if openErr != nil {
					return nil, openErr
				}
				return media.Body, nil
			}, exists && row.State == attachmentpolicy.StateFailed, &write)
			if storeErr != nil {
				return nil, storeErr
			}
			if write.State == attachmentpolicy.StateStored && media.MIME == "audio/wav" {
				write.Filename, write.MIMEType = "call-recording.wav", media.MIME
			}
		}
	}
	// Rewriting an identical row would only invalidate caches; a new row or a
	// changed one updates the meeting, which the cache refresh after a failed
	// run depends on. counted means the meeting already was.
	if exists && row.State == write.State && row.SkipReason == write.SkipReason && row.ContentHash == write.ContentHash && int64(row.Size) == write.Size {
		return providerErr, nil
	}
	if !counted {
		r.Sum.MeetingsUpdated++
	}
	if err := r.St.UpsertAttachmentRecordWithStats(ctx, messageID, write, true); err != nil {
		return nil, err
	}
	return providerErr, nil
}
