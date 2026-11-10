package store

const (
	SyncItemKindBatchFetchError               = "batch_fetch_error"
	SyncItemKindFetchError                    = "fetch_error"
	SyncItemKindIngestError                   = "ingest_error"
	SyncItemKindAttachmentError               = "attachment_error"
	SyncItemKindRemoteImageError              = "remote_image_error"
	SyncItemKindAttachmentAndRemoteImageError = "attachment_remote_image_error"
)

// IsAcquisitionReplayCandidate identifies outcomes the syncer can retry.
func (item SyncRunItem) IsAcquisitionReplayCandidate() bool {
	if item.SourceMessageID == "" || item.SourceMessageID == "(unknown)" {
		return false
	}
	switch item.Phase {
	case "fetch":
		return item.ErrorKind == SyncItemKindFetchError || item.ErrorKind == SyncItemKindBatchFetchError
	case "ingest":
		switch item.ErrorKind {
		case SyncItemKindIngestError, SyncItemKindAttachmentError, SyncItemKindRemoteImageError, SyncItemKindAttachmentAndRemoteImageError:
			return true
		}
	}
	return false
}
