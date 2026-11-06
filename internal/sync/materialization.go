package sync

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/store"
)

// materializationWork is stored as one outcome in the acquisition ledger.
// A combined outcome stays pending until every required operation succeeds.
type materializationWork uint8

const (
	materializeMIME materializationWork = 1 << iota
	archiveRemoteImages
)

func materializationKind(work materializationWork) string {
	switch work {
	case materializeMIME:
		return syncItemKindAttachmentError
	case archiveRemoteImages:
		return syncItemKindRemoteImageError
	default:
		return syncItemKindAttachmentAndRemoteImageError
	}
}

func materializationFromKind(kind string) materializationWork {
	switch kind {
	case syncItemKindAttachmentError:
		return materializeMIME
	case syncItemKindRemoteImageError:
		return archiveRemoteImages
	case syncItemKindAttachmentAndRemoteImageError:
		return materializeMIME | archiveRemoteImages
	default:
		return 0
	}
}

// committedMessageError distinguishes failed post-commit work from a rolled
// back ingest. History must use the committed identity for later events.
type committedMessageError struct {
	messageID int64
	err       error
}

func (e *committedMessageError) Error() string {
	return fmt.Sprintf("finish archived message %d: %v", e.messageID, e.err)
}
func (e *committedMessageError) Unwrap() error { return e.err }

func (s *Syncer) requiredMaterialization(data *messageData) materializationWork {
	work := s.pendingAttachmentRepairs[data.message.SourceMessageID]
	if s.opts.AttachmentsDir != "" {
		if data.mimeIncomplete {
			work |= materializeMIME
		}
		for _, att := range data.attachments {
			if len(att.Content) > 0 {
				work |= materializeMIME
				break
			}
		}
	}
	if s.opts.RemoteImages != nil && data.message.MessageType == store.MessageTypeEmail && data.bodyHTML != "" {
		work |= archiveRemoteImages
	}
	return work
}

func (s *Syncer) archiveMessageRemoteImages(ctx context.Context, id int64, html string, mimePending bool) error {
	if s.opts.RemoteImages == nil {
		return fmt.Errorf("%w: remote image archiving must be enabled to finish pending work", errRemoteImageArchival)
	}
	archived := s.opts.RemoteImages.ArchiveWithOptions(ctx, s.store, s.opts.AttachmentsDir, id, html,
		remoteimage.ArchiveOptions{PreserveExpectedAttachments: mimePending})
	for _, imageErr := range archived.Errors {
		s.logger.Warn("failed to archive remote image", "message", id, "error", imageErr)
	}
	if len(archived.Errors) > 0 {
		return fmt.Errorf("%w: %w", errRemoteImageArchival, errors.Join(archived.Errors...))
	}
	return nil
}

func (s *Syncer) repairPendingAttachments(ctx context.Context, sourceID int64, messageID string) error {
	work := s.pendingAttachmentRepairs[messageID]
	if work == 0 {
		return s.resolveAcquisition(messageID)
	}
	// Older attachment-only debt can predate durable image tracking. Publish
	// the currently enabled image obligation before finishing that old debt,
	// so a later interruption or configuration change cannot hide the work.
	if work&archiveRemoteImages == 0 && s.opts.RemoteImages != nil {
		work |= archiveRemoteImages
		if err := s.writeSyncItem(s.acquisitionSyncID, messageID, syncItemPhaseIngest,
			store.SyncRunItemStatusError, materializationKind(work), errors.New(store.SyncRunItemFilesPendingMessage)); err != nil {
			return fmt.Errorf("%w: extend pending image work: %w", errAcquisitionFailureRecord, err)
		}
	}
	var pendingErr error
	if work&materializeMIME != 0 {
		if err := s.RepairMessageAttachments(ctx, RepairRequest{Reference: messageID, SourceID: sourceID}); err != nil {
			pendingErr = fmt.Errorf("%w: %w", errAttachmentMaterialization, err)
		}
	}
	// Image archival is independent of MIME publication. In particular it must
	// run even when another attachment cannot be materialized.
	if work&archiveRemoteImages != 0 {
		ids, err := s.store.MessageExistsBatchContext(ctx, sourceID, []string{messageID})
		if err == nil {
			message, readErr := s.store.GetMessageContext(ctx, ids[messageID])
			if readErr != nil {
				err = readErr
			} else {
				err = s.archiveMessageRemoteImages(ctx, message.ID, message.BodyHTML, pendingErr != nil)
			}
		}
		if err != nil {
			if !errors.Is(err, errRemoteImageArchival) {
				err = fmt.Errorf("%w: %w", errRemoteImageArchival, err)
			}
			pendingErr = errors.Join(pendingErr, err)
		}
	}
	if pendingErr != nil {
		return pendingErr
	}
	return s.settleAcquisition(messageID)
}

// Replay file obligations independently of Gmail enumeration. The retained
// message is the authority for this work, including after provider deletion.
func (s *Syncer) replayLocalMaterialization(ctx context.Context, syncID, sourceID int64, checkpoint *store.Checkpoint) ([]string, error) {
	ids := make([]string, 0, len(s.pendingAttachmentRepairs))
	for id := range s.pendingAttachmentRepairs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := s.repairPendingAttachments(ctx, sourceID, id); err != nil {
			if ctx.Err() != nil || errors.Is(err, store.ErrSyncRunSuperseded) {
				return nil, err
			}
			if recErr := s.recordAcquisitionFailure(syncID, id, syncItemPhaseIngest, syncItemKindIngestError, err); recErr != nil {
				return nil, recErr
			}
			checkpoint.ErrorsCount++
		}
	}
	return ids, nil
}
