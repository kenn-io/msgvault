package beeper

import (
	"context"
	"encoding/hex"
	"mime"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
)

const attachmentMirrorPageSize = 100
const attachmentMirrorLimit = 2 << 30

// AttachmentMirror extends the stored-media route with ordinary file uploads.
// Store writes use the same daemon operation gate; streaming runs outside it.
type AttachmentMirror struct {
	store       *store.Store
	blobs       *attachmentstore.Store
	client      *docbankmedia.Client
	destination string
	config      config.DocbankIntegrationConfig
	gate        func(context.Context) (func(), bool)
}

func NewAttachmentMirror(st *store.Store, blobs *attachmentstore.Store, client *docbankmedia.Client, destination string, cfg config.DocbankIntegrationConfig) *AttachmentMirror {
	return &AttachmentMirror{store: st, blobs: blobs, client: client, destination: destination, config: cfg}
}

func (w *AttachmentMirror) WithOperationGate(gate func(context.Context) (func(), bool)) *AttachmentMirror {
	w.gate = gate
	return w
}

func (w *AttachmentMirror) gated(ctx context.Context, step func() error) error {
	if w.gate == nil {
		return step()
	}
	release, ok := w.gate(ctx)
	if !ok {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errBeeperMediaGateBusy
	}
	defer release()
	return step()
}

// RunBatch discovers one bounded page and delivers at most one unique digest.
// Missing consent still permits local discovery but makes no remote requests.
func (w *AttachmentMirror) RunBatch(ctx context.Context) (store.DocbankAttachmentSummary, error) {
	var result store.DocbankAttachmentSummary
	if !w.config.Enabled || !w.config.AttachmentMirror {
		return result, nil
	}
	err := w.gated(ctx, func() error {
		candidates, err := w.store.ScanDocbankAttachments(ctx, w.destination, attachmentMirrorPageSize)
		if err != nil {
			return err
		}
		reasons := make([]string, len(candidates))
		for i, c := range candidates {
			reasons[i] = attachmentSkipReason(c, w.config)
		}
		return w.store.SaveDocbankAttachmentScan(ctx, w.destination, strings.TrimRight(strings.TrimSpace(w.config.URL), "/"), w.config.MirrorCollection(), candidates, reasons, attachmentMirrorPageSize)
	})
	if err != nil {
		return result, endMediaPass(err)
	}
	if w.config.AttachmentUploadConsent && w.client != nil && w.blobs != nil {
		delivery, ok, err := w.store.NextDocbankAttachment(ctx, w.destination, w.currentFilter())
		if err != nil {
			return result, err
		}
		if ok {
			// Match the audio worker's byte-scaled timeout for large attachments.
			actionCtx, cancel := context.WithTimeout(ctx, 30*time.Second+time.Duration(delivery.Size/(256<<10))*time.Second)
			ref, uploadErr := w.upload(actionCtx, delivery)
			cancel()
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			reason := ""
			if uploadErr != nil {
				reason = docbankmedia.ErrorCode(uploadErr)
				result.Failed++
			}
			if uploadErr == nil {
				result.Delivered++
			}
			err = w.gated(ctx, func() error {
				return w.store.CompleteDocbankAttachment(ctx, w.destination, delivery.ContentHash, ref, reason, docbankmedia.Retryable(uploadErr))
			})
			if err != nil {
				return result, endMediaPass(err)
			}
		}
	}
	return result, nil
}

func (w *AttachmentMirror) upload(ctx context.Context, d store.DocbankAttachmentDelivery) (store.DocbankAttachmentRef, error) {
	var ref store.DocbankAttachmentRef
	reader, size, err := w.blobs.OpenStream(ctx, d.ContentHash)
	if err != nil {
		return ref, err
	}
	defer func() { _ = reader.Close() }()
	if size != d.Size {
		return ref, docbankmedia.ErrInvalidRequest
	}
	parent, err := w.client.EnsureCollection(ctx, w.config.MirrorCollection())
	if err != nil {
		return ref, err
	}
	receipt, err := w.client.UploadAttachment(ctx, parent, d.Name, d.MIMEType, d.ContentHash, d.Size, reader)
	if err != nil {
		return ref, err
	}
	if err := reader.Close(); err != nil {
		return ref, err
	}
	return store.DocbankAttachmentRef{NodeID: receipt.Node.ID, VersionID: receipt.Node.VersionID, BlobHash: receipt.Node.BlobHash}, nil
}

func attachmentSkipReason(c store.DocbankAttachmentCandidate, cfg config.DocbankIntegrationConfig) string {
	if strings.HasPrefix(c.StoragePath, "http://") || strings.HasPrefix(c.StoragePath, "https://") || c.StoragePath == "" {
		return "bytes_not_stored"
	}
	hash, err := hex.DecodeString(c.ContentHash)
	if err != nil || len(hash) != 32 || strings.ToLower(c.ContentHash) != c.ContentHash {
		return "bytes_not_stored"
	}
	if c.State != "" && c.State != "stored" {
		return "bytes_not_stored"
	}
	if c.Size < 0 || c.Size > attachmentMirrorLimit {
		return "size_limit"
	}
	if cfg.AttachmentMaxBytes > 0 && c.Size > cfg.AttachmentMaxBytes {
		return "size_filtered"
	}
	mediaType, _, err := mime.ParseMediaType(c.MIMEType)
	if err != nil && c.MIMEType != "" {
		return "invalid_mime"
	}
	if len(cfg.AttachmentMIMEClasses) > 0 && !slices.Contains(cfg.AttachmentMIMEClasses, strings.SplitN(mediaType, "/", 2)[0]) {
		return "mime_filtered"
	}
	if len(cfg.AttachmentSourceIDs) > 0 && !slices.Contains(cfg.AttachmentSourceIDs, c.SourceID) {
		return "source_filtered"
	}
	if cfg.AttachmentAfter != "" {
		after, err := time.Parse("2006-01-02", cfg.AttachmentAfter)
		if err != nil || c.SentAt.IsZero() || c.SentAt.Before(after) {
			return "date_filtered"
		}
	}
	return ""
}

func (w *AttachmentMirror) currentFilter() store.DocbankAttachmentFilter {
	filter := store.DocbankAttachmentFilter{MIMEClasses: w.config.AttachmentMIMEClasses, SourceIDs: w.config.AttachmentSourceIDs, MaxBytes: w.config.AttachmentMaxBytes}
	if w.config.AttachmentAfter != "" {
		after, err := time.Parse("2006-01-02", w.config.AttachmentAfter)
		if err == nil {
			filter.After = &after
		}
	}
	return filter
}
