package beeper

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"

	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
)

const (
	beeperMediaScanPageSize    = 100
	beeperMediaJournalPageSize = 100
	beeperMediaActionTimeout   = 30 * time.Second
	// ponytail: an upload gets one extra second per 256 KiB (~2 Mbit/s); lower the rate if slower links time out.
	beeperMediaUploadRate      = 256 << 10
	beeperMediaTranscriptLimit = 16 << 20
	// ponytail: the pinned inspector stops at 1 GiB, below Docbank's 2 GiB upload ceiling; raise with the Docbank pin.
	beeperMediaSourceLimit = 1 << 30
)

var (
	errBeeperMediaRawInvalid         = errors.New("source_raw_invalid")
	errBeeperMediaPartMissing        = errors.New("source_part_missing")
	errBeeperMediaPartAmbiguous      = errors.New("source_part_ambiguous")
	errBeeperMediaUnsupported        = errors.New("unsupported_media")
	errBeeperMediaTranscriptInvalid  = errors.New("source_transcript_invalid")
	errBeeperMediaTranscriptTooLarge = errors.New("source_transcript_too_large")
	errBeeperMediaSourceChanged      = errors.New("source_changed")
	errBeeperMediaSourceUnavailable  = errors.New("source_unavailable")
	errBeeperMediaNoLiveOccurrence   = errors.New("no_live_occurrence")
	errBeeperMediaGateBusy           = errors.New("operation gate busy")
)

// MediaDescriptor contains immutable claims for one verified Beeper media
// version. It intentionally carries no transcript text or credentials.
type MediaDescriptor struct {
	Occurrence           docbankmedia.Occurrence
	SourceType           string
	SourceIdentifier     string
	SourceConversationID string
	SourceMessageID      string
	SourceAttachmentID   string
	SourcePartKey        string
	SourceSHA256         string
	ByteLength           int64
	RawHash              string
	TranscriptSHA256     string
	Language             string
	Filename             string
	MIMEType             string
	ProcessingKey        string
}

// MediaBatchResult counts one bounded pass. Retained counts a retention
// receipt committed by this pass.
type MediaBatchResult struct {
	Examined  int
	Pending   int
	Retained  int
	Blocked   int
	Journaled int
}

// MediaSubmitter discovers current Beeper media, persists source mappings and
// performs at most one Docbank action per bounded pass.
type MediaSubmitter struct {
	store         *store.Store
	blobs         *attachmentstore.Store
	client        *docbankmedia.Client
	destination   string
	spoolDir      string
	actionTimeout time.Duration
	uploadRate    int64
	gate          func(context.Context) (func(), bool)
}

// NewMediaSubmitter returns a worker for one destination. A nil client keeps
// discovery local and sends nothing.
func NewMediaSubmitter(
	st *store.Store, blobs *attachmentstore.Store, client *docbankmedia.Client, destination, spoolDir string,
) *MediaSubmitter {
	return &MediaSubmitter{
		store: st, blobs: blobs, client: client, destination: destination, spoolDir: spoolDir,
		actionTimeout: beeperMediaActionTimeout, uploadRate: beeperMediaUploadRate,
	}
}

// WithOperationGate makes Store writes wait for the daemon operation gate.
// Spooling and HTTP run outside it; a refused gate ends the pass.
func (w *MediaSubmitter) WithOperationGate(gate func(context.Context) (func(), bool)) *MediaSubmitter {
	w.gate = gate
	return w
}

// RunBatch commits bounded discovery before selecting one remote step. A
// remote failure is recorded on its operation and never fails the pass.
func (w *MediaSubmitter) RunBatch(ctx context.Context) (MediaBatchResult, error) {
	if w == nil || w.store == nil || w.destination == "" {
		return MediaBatchResult{}, errors.New("beeper media submitter is not configured")
	}
	archiveUID, err := w.store.ArchiveUIDContext(ctx)
	if err != nil {
		return MediaBatchResult{}, err
	}
	result, err := w.discover(ctx, archiveUID)
	if err != nil || w.client == nil || w.blobs == nil {
		return result, endMediaPass(err)
	}
	operation, ok, err := w.store.NextBeeperMediaOperation(ctx, w.destination, time.Now().UTC())
	if err != nil || !ok {
		return result, endMediaPass(err)
	}
	// ponytail: one remote action per pass (~1,440/day); batch once a measured backlog outgrows it.
	retained, err := w.runOperation(ctx, archiveUID, operation)
	if retained {
		result.Retained++
	}
	return result, endMediaPass(err)
}

// endMediaPass treats a busy gate as the end of the pass; the saved operation
// ID replays on the next one.
func endMediaPass(err error) error {
	if errors.Is(err, errBeeperMediaGateBusy) {
		return nil
	}
	return err
}

// gated runs one Store step under the operation gate so backup freeze sees no writer.
func (w *MediaSubmitter) gated(ctx context.Context, step func() error) error {
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

func (w *MediaSubmitter) prepareOperation(
	ctx context.Context, operation store.BeeperMediaOperation,
) (store.BeeperMediaOperation, error) {
	var prepared store.BeeperMediaOperation
	err := w.gated(ctx, func() (err error) {
		prepared, err = w.store.PrepareBeeperMediaOperation(ctx, operation)
		return err
	})
	return prepared, err
}

// liveMappings is gated because the Store revokes stale mappings while listing.
func (w *MediaSubmitter) liveMappings(ctx context.Context, processingKey string, limit int) ([]store.BeeperMediaMapping, error) {
	var mappings []store.BeeperMediaMapping
	err := w.gated(ctx, func() (err error) {
		mappings, err = w.store.ListLiveBeeperMediaMappings(ctx, w.destination, processingKey, limit)
		return err
	})
	return mappings, err
}

func (w *MediaSubmitter) discover(ctx context.Context, archiveUID string) (MediaBatchResult, error) {
	var result MediaBatchResult
	consumer, err := w.store.GetAttachmentChangeConsumer(ctx, store.BeeperMediaAttachmentConsumerKey)
	var created bool
	if errors.Is(err, store.ErrAttachmentChangeConsumerMissing) {
		err = w.gated(ctx, func() (err error) {
			consumer, created, err = w.store.RegisterAttachmentChangeConsumer(ctx, store.BeeperMediaAttachmentConsumerKey)
			return err
		})
	}
	if err != nil {
		return result, err
	}
	scan, err := w.store.LoadBeeperMediaScan(ctx, w.destination)
	if err != nil {
		return result, err
	}
	before := scan
	if !consumer.ReconciliationComplete && (created || scan.BaselineSequence != consumer.BaselineSequence) {
		// A registration completes only after a full pass that began after it.
		scan = store.BeeperMediaScan{DestinationKey: w.destination, BaselineSequence: consumer.BaselineSequence}
	}
	if !consumer.ReconciliationComplete || !time.Now().Before(scan.NextFullScanAt) {
		if scan.PassHighWater == 0 {
			if scan.PassHighWater, err = w.store.BeeperMediaAttachmentHighWater(ctx); err != nil {
				return result, err
			}
		}
		candidates, err := w.store.ListBeeperMediaCandidates(ctx, scan.AfterAttachmentID, beeperMediaScanPageSize)
		if err != nil {
			return result, err
		}
		for _, candidate := range candidates {
			mapping, err := w.reconcileCandidate(ctx, archiveUID, candidate)
			if err != nil {
				return result, err
			}
			result.Examined++
			if mapping.RetentionState == store.BeeperMediaRetentionBlocked {
				result.Blocked++
			} else {
				result.Pending++
			}
			scan.AfterAttachmentID = candidate.AttachmentID
		}
		fullPass := len(candidates) < beeperMediaScanPageSize || scan.AfterAttachmentID >= scan.PassHighWater
		if fullPass {
			scan.AfterAttachmentID, scan.PassHighWater = 0, 0
			// Raw-message updates (such as late transcripts) are not in the attachment journal.
			scan.NextFullScanAt = time.Now().UTC().Add(24 * time.Hour)
		}
		var swapped bool
		err = w.gated(ctx, func() (err error) {
			if fullPass {
				if err := w.store.RevokeStaleBeeperMediaMappings(ctx, w.destination); err != nil {
					return err
				}
			}
			swapped, err = w.store.AdvanceBeeperMediaScan(ctx, w.destination, before, scan)
			if err == nil && swapped && fullPass && !consumer.ReconciliationComplete && scan.BaselineSequence == consumer.BaselineSequence {
				err = w.store.CompleteAttachmentChangeReconciliation(ctx,
					store.BeeperMediaAttachmentConsumerKey, consumer.BaselineSequence)
				consumer.ReconciliationComplete = err == nil
			}
			return err
		})
		if err != nil || !swapped {
			return result, err
		}
	}
	if consumer.ReconciliationComplete {
		result.Journaled, err = w.replayJournal(ctx, archiveUID)
	}
	return result, err
}

// reconcileCandidate reads and parses outside the daemon's write gate.
func (w *MediaSubmitter) reconcileCandidate(
	ctx context.Context, archiveUID string, candidate store.BeeperMediaCandidate,
) (store.BeeperMediaMapping, error) {
	mapping, err := w.mappingForCandidate(ctx, archiveUID, candidate)
	if err != nil {
		return store.BeeperMediaMapping{}, err
	}
	err = w.gated(ctx, func() error { return w.store.ReconcileBeeperMediaMapping(ctx, mapping) })
	return mapping, err
}

// replayJournal resolves journaled row IDs to current attachments, commits
// local discovery and only then acknowledges the journal page.
func (w *MediaSubmitter) replayJournal(ctx context.Context, archiveUID string) (int, error) {
	changes, err := w.store.ListAttachmentChanges(ctx, store.BeeperMediaAttachmentConsumerKey, beeperMediaJournalPageSize)
	if err != nil || len(changes) == 0 {
		return 0, err
	}
	for _, change := range changes {
		for _, attachmentID := range []*int64{change.OldAttachmentID, change.NewAttachmentID} {
			if attachmentID == nil {
				continue
			}
			candidate, err := w.store.GetBeeperMediaCandidate(ctx, *attachmentID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return 0, err
			}
			if _, err := w.reconcileCandidate(ctx, archiveUID, candidate); err != nil {
				return 0, err
			}
		}
	}
	err = w.gated(ctx, func() error {
		if err := w.store.RevokeStaleBeeperMediaMappings(ctx, w.destination); err != nil {
			return err
		}
		return w.store.AdvanceAttachmentChangeConsumer(ctx, store.BeeperMediaAttachmentConsumerKey, changes[len(changes)-1].Sequence)
	})
	return len(changes), err
}

func (w *MediaSubmitter) mappingForCandidate(
	ctx context.Context, archiveUID string, candidate store.BeeperMediaCandidate,
) (store.BeeperMediaMapping, error) {
	raw, err := w.store.GetMessageRawContext(ctx, candidate.MessageID)
	if beeperMediaRawGap(err) {
		return fallbackMediaMapping(w.destination, candidate, archiveUID, errBeeperMediaRawInvalid), nil
	}
	if err != nil {
		return store.BeeperMediaMapping{}, err
	}
	descriptor, _, err := describeMedia(raw, candidate, archiveUID)
	if err != nil {
		return fallbackMediaMapping(w.destination, candidate, archiveUID, err), nil
	}
	return descriptorMapping(w.destination, candidate, descriptor), nil
}

func beeperMediaRawGap(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrInvalidMessageRaw)
}

type beeperMediaEvidence struct {
	candidate  store.BeeperMediaCandidate
	mapping    store.BeeperMediaMapping
	descriptor MediaDescriptor
	transcript string
	rawHash    string
	gapCode    string
	missing    bool
}

func (w *MediaSubmitter) candidateEvidence(
	ctx context.Context, archiveUID string, attachmentID int64,
) (beeperMediaEvidence, error) {
	candidate, err := w.store.GetBeeperMediaCandidate(ctx, attachmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return beeperMediaEvidence{missing: true}, nil
	}
	if err != nil {
		return beeperMediaEvidence{}, err
	}
	evidence := beeperMediaEvidence{candidate: candidate}
	raw, err := w.store.GetMessageRawContext(ctx, candidate.MessageID)
	if beeperMediaRawGap(err) {
		evidence.gapCode = errBeeperMediaRawInvalid.Error()
		return evidence, nil
	}
	if err != nil {
		return beeperMediaEvidence{}, err
	}
	evidence.rawHash = hashBytes(raw)
	descriptor, transcript, err := describeMedia(raw, candidate, archiveUID)
	if err != nil {
		evidence.gapCode = mediaGapCode(err)
		return evidence, nil
	}
	evidence.descriptor, evidence.transcript = descriptor, transcript
	return evidence, nil
}

func (w *MediaSubmitter) mappingEvidence(
	ctx context.Context, archiveUID string, mapping store.BeeperMediaMapping,
) (beeperMediaEvidence, error) {
	evidence := beeperMediaEvidence{candidate: mappingCandidate(mapping), mapping: mapping}
	raw, err := w.store.GetMessageRawContext(ctx, mapping.MessageID)
	if beeperMediaRawGap(err) {
		evidence.gapCode = errBeeperMediaRawInvalid.Error()
		return evidence, nil
	}
	if err != nil {
		return beeperMediaEvidence{}, err
	}
	evidence.rawHash = hashBytes(raw)
	descriptor, transcript, err := describeMedia(raw, evidence.candidate, archiveUID)
	if err != nil {
		evidence.gapCode = mediaGapCode(err)
		return evidence, nil
	}
	evidence.descriptor, evidence.transcript = descriptor, transcript
	return evidence, nil
}

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func sameBeeperMediaCandidate(a, b store.BeeperMediaCandidate) bool {
	return a.AttachmentID == b.AttachmentID && a.MessageID == b.MessageID &&
		a.ConversationID == b.ConversationID && a.SourceID == b.SourceID &&
		a.SourceType == b.SourceType && a.SourceIdentifier == b.SourceIdentifier &&
		a.SourceConversationID == b.SourceConversationID && a.SourceMessageID == b.SourceMessageID &&
		a.SourceAttachmentID == b.SourceAttachmentID && a.SourcePartKey == b.SourcePartKey &&
		a.Filename == b.Filename && a.MIMEType == b.MIMEType && a.MediaType == b.MediaType &&
		a.Role == b.Role && a.ContentHash == b.ContentHash && a.ByteLength == b.ByteLength &&
		a.AttachmentState == b.AttachmentState
}

func sameBeeperMediaMapping(a store.BeeperMediaMapping, b beeperMediaEvidence) bool {
	return a.OccurrenceRef == b.mapping.OccurrenceRef && a.Revision == b.mapping.Revision &&
		a.SourceSHA256 == b.mapping.SourceSHA256 && a.ProcessingKey == b.mapping.ProcessingKey
}

func descriptorMapping(
	destination string, candidate store.BeeperMediaCandidate, descriptor MediaDescriptor,
) store.BeeperMediaMapping {
	return store.BeeperMediaMapping{
		DestinationKey: destination, OccurrenceRef: descriptor.Occurrence.Ref,
		Revision: descriptor.Occurrence.Revision, SourceType: descriptor.SourceType,
		SourceIdentifier: descriptor.SourceIdentifier, SourceConversationID: descriptor.SourceConversationID,
		SourceMessageID: descriptor.SourceMessageID, SourceAttachmentID: descriptor.SourceAttachmentID,
		SourcePartKey: descriptor.SourcePartKey, LocalSourceID: candidate.SourceID,
		MessageID: candidate.MessageID, AttachmentID: candidate.AttachmentID,
		SourceSHA256: descriptor.SourceSHA256, ByteLength: descriptor.ByteLength,
		RawHash: descriptor.RawHash, TranscriptSHA256: descriptor.TranscriptSHA256,
		Language: descriptor.Language, OccurrenceJSON: mustJSON(descriptor.Occurrence),
		Filename: descriptor.Filename, MIMEType: descriptor.MIMEType,
		RetentionState: store.BeeperMediaRetentionPending, ProcessingKey: descriptor.ProcessingKey,
	}
}

// fallbackMediaMapping records a source gap under the stable occurrence
// reference, so the gap stays visible without sending anything.
func fallbackMediaMapping(
	destination string, candidate store.BeeperMediaCandidate, archiveUID string, cause error,
) store.BeeperMediaMapping {
	return fallbackMediaMappingCode(destination, candidate, archiveUID, mediaGapCode(cause))
}

func fallbackMediaMappingCode(
	destination string, candidate store.BeeperMediaCandidate, archiveUID, code string,
) store.BeeperMediaMapping {
	part := candidate.SourcePartKey
	if part == "" {
		part = candidate.SourceAttachmentID
	}
	if part == "" {
		part = "beeper:unknown"
	}
	filename, mediaType := selectedMediaMetadata(candidate.Filename, candidate.MIMEType)
	descriptor := MediaDescriptor{
		Occurrence: docbankmedia.Occurrence{Filename: filename, Ref: mediaOccurrenceRef(archiveUID,
			candidate.SourceType, candidate.SourceIdentifier, candidate.SourceConversationID,
			candidate.SourceMessageID, part)},
		SourceType: candidate.SourceType, SourceIdentifier: candidate.SourceIdentifier,
		SourceConversationID: candidate.SourceConversationID, SourceMessageID: candidate.SourceMessageID,
		SourceAttachmentID: candidate.SourceAttachmentID, SourcePartKey: part,
		SourceSHA256: candidate.ContentHash, ByteLength: candidate.ByteLength,
		Filename: filename, MIMEType: mediaType,
	}
	descriptor.Occurrence.Revision = hashDelimited("gap", mediaRevision(descriptor), code)
	mapping := descriptorMapping(destination, candidate, descriptor)
	mapping.RetentionState, mapping.ErrorCode = store.BeeperMediaRetentionBlocked, code
	return mapping
}

func (w *MediaSubmitter) runOperation(
	ctx context.Context, archiveUID string, operation store.BeeperMediaOperation,
) (bool, error) {
	// Only remote and spool work runs under the step deadline, so a timed-out step can still record its retry.
	timeout := w.actionTimeout
	if operation.Kind == store.BeeperMediaOperationRetain && w.uploadRate > 0 {
		// Copying and uploading scale with size, so large audio gets a proportionally longer bounded deadline.
		timeout += time.Duration(operation.ByteLength/w.uploadRate) * time.Second
	}
	actionCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch operation.Kind {
	case store.BeeperMediaOperationRetain:
		return w.retain(ctx, actionCtx, archiveUID, operation)
	case store.BeeperMediaOperationArtifact:
		return false, w.artifact(ctx, actionCtx, archiveUID, operation)
	case store.BeeperMediaOperationProcess:
		return false, w.process(ctx, actionCtx, archiveUID, operation)
	case store.BeeperMediaOperationStatus:
		return false, w.status(ctx, actionCtx, operation)
	default:
		return false, errors.New("unknown beeper media operation")
	}
}

// retain uploads one verified occurrence. The saved occurrence row, rather
// than a fresh parse, supplies the wire metadata so replay stays identical.
func (w *MediaSubmitter) retain(
	ctx, actionCtx context.Context, archiveUID string, operation store.BeeperMediaOperation,
) (bool, error) {
	unavailable := func(code string) (bool, error) {
		return false, w.finishOperation(ctx, operation, store.BeeperMediaResult{ErrorCode: code, SourceUnavailable: true})
	}
	evidence, err := w.candidateEvidence(ctx, archiveUID, operation.AttachmentID)
	if err != nil {
		return false, err
	}
	if evidence.missing {
		// A hidden or deleted message retires its pending row; discovery reopens it if the message returns.
		return false, w.finishOperation(ctx, operation, store.BeeperMediaResult{
			ErrorCode: errBeeperMediaNoLiveOccurrence.Error(), Revoked: true,
		})
	}
	if evidence.gapCode != "" {
		return unavailable(evidence.gapCode)
	}
	if evidence.descriptor.Occurrence.Ref != operation.OccurrenceRef ||
		evidence.descriptor.Occurrence.Revision != operation.Revision ||
		evidence.descriptor.SourceSHA256 != operation.SourceSHA256 {
		return unavailable(errBeeperMediaSourceChanged.Error())
	}
	file, format, err := prepareMediaUpload(actionCtx, w.blobs, evidence.descriptor, w.spoolDir)
	if err != nil {
		if actionCtx.Err() != nil {
			return false, w.finishClientError(ctx, actionCtx, operation, err)
		}
		if errors.Is(err, errBeeperMediaSourceUnavailable) {
			return unavailable(errBeeperMediaSourceUnavailable.Error())
		}
		return false, w.finishOperation(ctx, operation, store.BeeperMediaResult{ErrorCode: mediaGapCode(err)})
	}
	defer closeAndRemove(file)
	fresh, err := w.candidateEvidence(ctx, archiveUID, operation.AttachmentID)
	if err != nil {
		return false, err
	}
	var prepared store.BeeperMediaOperation
	err = w.gated(ctx, func() error {
		candidate, err := w.store.GetBeeperMediaCandidate(ctx, operation.AttachmentID)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = w.store.FinishBeeperMediaOperation(ctx, operation, store.BeeperMediaResult{
				ErrorCode: errBeeperMediaNoLiveOccurrence.Error(), Revoked: true,
			})
			return err
		}
		if err != nil {
			return err
		}
		raw, err := w.store.GetMessageRawContext(ctx, candidate.MessageID)
		if beeperMediaRawGap(err) {
			_, err = w.store.FinishBeeperMediaOperation(ctx, operation, store.BeeperMediaResult{
				ErrorCode: errBeeperMediaRawInvalid.Error(), SourceUnavailable: true,
			})
			return err
		}
		if err != nil {
			return err
		}
		currentRawHash := hashBytes(raw)
		if fresh.missing || fresh.gapCode != "" ||
			!sameBeeperMediaCandidate(candidate, fresh.candidate) ||
			fresh.rawHash == "" || currentRawHash != fresh.rawHash ||
			fresh.rawHash != evidence.rawHash ||
			fresh.descriptor.Occurrence.Ref != operation.OccurrenceRef ||
			fresh.descriptor.Occurrence.Revision != operation.Revision ||
			fresh.descriptor.SourceSHA256 != operation.SourceSHA256 {
			code := errBeeperMediaSourceChanged.Error()
			if fresh.gapCode != "" && fresh.rawHash != "" && currentRawHash == fresh.rawHash {
				code = fresh.gapCode
			}
			_, err = w.store.FinishBeeperMediaOperation(ctx, operation, store.BeeperMediaResult{
				ErrorCode: code, SourceUnavailable: true,
			})
			return err
		}
		prepared, err = w.store.PrepareBeeperMediaOperation(ctx, operation)
		return err
	})
	if err != nil {
		return false, err
	}
	if prepared.OperationID == "" {
		return false, nil
	}
	var occurrence docbankmedia.Occurrence
	if err := json.Unmarshal([]byte(prepared.FrozenRequestJSON), &occurrence); err != nil {
		return false, fmt.Errorf("decode saved beeper media occurrence: %w", err)
	}
	filename, mediaType := mediaWireIdentity(operation.Filename, format)
	receipt, err := w.client.Submit(actionCtx, docbankmedia.SuppliedMetadata{
		OperationID: prepared.OperationID, Filename: filename, MediaType: mediaType,
		SHA256: operation.SourceSHA256, ByteLength: operation.ByteLength, Occurrence: occurrence,
	}, file)
	if err != nil {
		return false, w.finishClientError(ctx, actionCtx, prepared, err)
	}
	return w.finish(ctx, prepared, store.BeeperMediaResult{
		VaultUID: receipt.VaultUID, DocbankSourceID: receipt.SourceID,
		SourceVersionID: receipt.SourceVersionID, ContentVersionID: receipt.ContentVersionID,
		DocbankOccurrenceID: receipt.OccurrenceID, CoverageState: receipt.CoverageState,
	})
}

// artifact imports the exact provider transcript once for a shared processing
// key, attached to a retained live donor occurrence.
func (w *MediaSubmitter) artifact(
	ctx, actionCtx context.Context, archiveUID string, operation store.BeeperMediaOperation,
) error {
	mappings, err := w.liveMappings(ctx, operation.ProcessingKey, 100)
	if err != nil {
		return err
	}
	evidence := make([]beeperMediaEvidence, 0, len(mappings))
	for _, mapping := range mappings {
		donor, err := w.mappingEvidence(ctx, archiveUID, mapping)
		if err != nil {
			return err
		}
		evidence = append(evidence, donor)
	}
	var donor store.BeeperMediaMapping
	var transcript string
	var prepared store.BeeperMediaOperation
	var retryScheduled bool
	err = w.gated(ctx, func() error {
		current, err := w.store.ListLiveBeeperMediaMappings(ctx, w.destination, operation.ProcessingKey, 100)
		if err != nil {
			return err
		}
		var gaps []store.BeeperMediaMapping
		var surviving *beeperMediaEvidence
		var sourceChanged *beeperMediaEvidence
		for _, mapping := range current {
			var snapshot *beeperMediaEvidence
			for i := range evidence {
				candidate := &evidence[i]
				if !sameBeeperMediaMapping(mapping, *candidate) {
					continue
				}
				if candidate.gapCode == "" &&
					(candidate.descriptor.Occurrence.Ref != mapping.OccurrenceRef ||
						candidate.descriptor.Occurrence.Revision != mapping.Revision) {
					continue
				}
				if candidate.gapCode == "" && mapping.TranscriptSHA256 != candidate.descriptor.TranscriptSHA256 {
					continue
				}
				if candidate.gapCode != "" || (candidate.descriptor.ProcessingKey == operation.ProcessingKey && candidate.transcript != "") {
					snapshot = candidate
					break
				}
			}
			if snapshot == nil {
				continue
			}
			raw, err := w.store.GetMessageRawContext(ctx, mapping.MessageID)
			if beeperMediaRawGap(err) {
				gaps = append(gaps, fallbackMediaMapping(w.destination, mappingCandidate(mapping), archiveUID,
					errBeeperMediaRawInvalid))
				continue
			}
			if err != nil {
				return err
			}
			currentRawHash := hashBytes(raw)
			if snapshot.gapCode != "" {
				if snapshot.rawHash != "" && currentRawHash == snapshot.rawHash {
					gaps = append(gaps, fallbackMediaMappingCode(w.destination,
						mappingCandidate(mapping), archiveUID, snapshot.gapCode))
				}
				continue
			}
			if currentRawHash != snapshot.rawHash {
				if sourceChanged == nil {
					copyOf := *snapshot
					sourceChanged = &copyOf
				}
				continue
			}
			if snapshot.descriptor.ProcessingKey != operation.ProcessingKey || snapshot.transcript == "" {
				continue
			}
			if surviving == nil {
				copyOf := *snapshot
				surviving = &copyOf
			}
		}
		for _, gap := range gaps {
			if err := w.store.ReconcileBeeperMediaMapping(ctx, gap); err != nil {
				return err
			}
		}
		if surviving == nil {
			if sourceChanged != nil {
				transcriptSHA := sourceChanged.descriptor.TranscriptSHA256
				operation.FrozenRequestJSON = mustJSON(docbankmedia.ArtifactMetadata{
					OccurrenceID: sourceChanged.mapping.DocbankOccurrenceID, Kind: "transcript", Origin: "provider",
					Provider: "beeper", Language: operation.Language, Filename: "transcript.txt",
					MediaType: "text/plain", SHA256: transcriptSHA, ByteLength: int64(len(sourceChanged.transcript)),
				})
				operation.DocbankSourceID, operation.SourceVersionID = sourceChanged.mapping.DocbankSourceID, sourceChanged.mapping.SourceVersionID
				operation.ContentVersionID, operation.DocbankOccurrenceID = sourceChanged.mapping.ContentVersionID, sourceChanged.mapping.DocbankOccurrenceID
				prepared, err = w.store.PrepareBeeperMediaOperation(ctx, operation)
				if err != nil {
					return err
				}
				if _, err := w.store.FinishBeeperMediaOperation(ctx, prepared, store.BeeperMediaResult{
					ErrorCode: errBeeperMediaSourceChanged.Error(), Retry: true,
				}); err != nil {
					return err
				}
				retryScheduled = true
			}
			return nil
		}
		donor, transcript = surviving.mapping, surviving.transcript
		refreshed := descriptorMapping(w.destination, surviving.candidate, surviving.descriptor)
		refreshed.RawHash = surviving.rawHash
		if err := w.store.ReconcileBeeperMediaMapping(ctx, refreshed); err != nil {
			return err
		}
		transcriptSHA := surviving.descriptor.TranscriptSHA256
		operation.FrozenRequestJSON = mustJSON(docbankmedia.ArtifactMetadata{
			OccurrenceID: donor.DocbankOccurrenceID, Kind: "transcript", Origin: "provider",
			Provider: "beeper", Language: operation.Language, Filename: "transcript.txt",
			MediaType: "text/plain", SHA256: transcriptSHA, ByteLength: int64(len(transcript)),
		})
		operation.DocbankSourceID, operation.SourceVersionID = donor.DocbankSourceID, donor.SourceVersionID
		operation.ContentVersionID, operation.DocbankOccurrenceID = donor.ContentVersionID, donor.DocbankOccurrenceID
		prepared, err = w.store.PrepareBeeperMediaOperation(ctx, operation)
		return err
	})
	if err != nil {
		return err
	}
	if retryScheduled {
		return nil
	}
	if prepared.OperationID == "" {
		return nil
	}
	var metadata docbankmedia.ArtifactMetadata
	if err := json.Unmarshal([]byte(prepared.FrozenRequestJSON), &metadata); err != nil {
		return fmt.Errorf("decode saved beeper transcript request: %w", err)
	}
	metadata.OperationID = prepared.OperationID
	if metadata.SHA256 != hashBytes([]byte(transcript)) {
		return w.finishOperation(ctx, prepared, store.BeeperMediaResult{ErrorCode: errBeeperMediaSourceChanged.Error()})
	}
	receipt, err := w.client.ImportTranscript(actionCtx, prepared.DocbankSourceID, metadata, strings.NewReader(transcript))
	if err != nil {
		return w.finishClientError(ctx, actionCtx, prepared, err)
	}
	if receipt.VaultUID != donor.VaultUID || receipt.SourceID != prepared.DocbankSourceID {
		return w.finishOperation(ctx, prepared, store.BeeperMediaResult{ErrorCode: "destination_mismatch"})
	}
	return w.finishOperation(ctx, prepared, store.BeeperMediaResult{SuppliedInputID: receipt.SuppliedInputID})
}

// process explicitly queues the supplied-transcript profile once per key.
func (w *MediaSubmitter) process(
	ctx, actionCtx context.Context, archiveUID string, operation store.BeeperMediaOperation,
) error {
	mappings, err := w.liveMappings(ctx, operation.ProcessingKey, 1)
	if err != nil {
		return err
	}
	if len(mappings) == 0 {
		return nil
	}
	evidence, err := w.mappingEvidence(ctx, archiveUID, mappings[0])
	if err != nil {
		return err
	}
	operation.FrozenRequestJSON = mustJSON(docbankmedia.Processing{
		Profile: "supplied-transcript", SuppliedInputID: operation.SuppliedInputID,
	})
	var prepared store.BeeperMediaOperation
	var vaultUID string
	var retryScheduled bool
	err = w.gated(ctx, func() error {
		current, err := w.store.ListLiveBeeperMediaMappings(ctx, w.destination, operation.ProcessingKey, 1)
		if err != nil {
			return err
		}
		var selected *store.BeeperMediaMapping
		for i := range current {
			if sameBeeperMediaMapping(current[i], evidence) {
				selected = &current[i]
				break
			}
		}
		if selected == nil {
			return nil
		}
		vaultUID = selected.VaultUID
		raw, err := w.store.GetMessageRawContext(ctx, selected.MessageID)
		if beeperMediaRawGap(err) {
			gap := fallbackMediaMapping(w.destination, mappingCandidate(*selected), archiveUID,
				errBeeperMediaRawInvalid)
			return w.store.ReconcileBeeperMediaMapping(ctx, gap)
		}
		if err != nil {
			return err
		}
		currentRawHash := hashBytes(raw)
		if currentRawHash != evidence.rawHash {
			prepared, err = w.store.PrepareBeeperMediaOperation(ctx, operation)
			if err != nil {
				return err
			}
			_, err = w.store.FinishBeeperMediaOperation(ctx, prepared, store.BeeperMediaResult{
				ErrorCode: errBeeperMediaSourceChanged.Error(), Retry: true,
			})
			retryScheduled = err == nil
			return err
		}
		if evidence.gapCode != "" {
			gap := fallbackMediaMappingCode(w.destination, mappingCandidate(*selected), archiveUID, evidence.gapCode)
			return w.store.ReconcileBeeperMediaMapping(ctx, gap)
		}
		fresh := descriptorMapping(w.destination, evidence.candidate, evidence.descriptor)
		if !sameBeeperMediaMapping(*selected, beeperMediaEvidence{mapping: fresh}) {
			return w.store.ReconcileBeeperMediaMapping(ctx, fresh)
		}
		if err := w.store.ReconcileBeeperMediaMapping(ctx, fresh); err != nil {
			return err
		}
		prepared, err = w.store.PrepareBeeperMediaOperation(ctx, operation)
		return err
	})
	if err != nil {
		return err
	}
	if retryScheduled || prepared.OperationID == "" {
		return nil
	}
	var processing docbankmedia.Processing
	if err := json.Unmarshal([]byte(prepared.FrozenRequestJSON), &processing); err != nil {
		return fmt.Errorf("decode saved beeper processing request: %w", err)
	}
	receipt, err := w.client.Process(actionCtx, prepared.DocbankSourceID, prepared.OperationID, processing.SuppliedInputID)
	if err != nil {
		return w.finishClientError(ctx, actionCtx, prepared, err)
	}
	if receipt.VaultUID != vaultUID || receipt.SourceID != prepared.DocbankSourceID {
		return w.finishOperation(ctx, prepared, store.BeeperMediaResult{ErrorCode: "destination_mismatch"})
	}
	result := store.BeeperMediaResult{
		JobID: receipt.JobID, OperationState: receipt.OperationState, CoverageState: receipt.CoverageState,
	}
	if receipt.OperationState == "failed" {
		// Docbank saves a failed receipt, usually without a job, when it cannot enqueue this processing.
		result.Terminal, result.ErrorCode = true, "processing_failed"
	}
	return w.finishOperation(ctx, prepared, result)
}

// status reads the exact processing job and this operation's own receipt. It
// settles only when that receipt has failed or has succeeded with final coverage.
func (w *MediaSubmitter) status(ctx, actionCtx context.Context, operation store.BeeperMediaOperation) error {
	job, err := w.client.JobStatus(actionCtx, operation.JobID)
	if err != nil {
		return w.finishClientError(ctx, actionCtx, operation, err)
	}
	if job.State == "operator_required" {
		// Docbank quarantines these jobs; a daemon restart can resume observation after operator repair.
		return w.finishOperation(ctx, operation, store.BeeperMediaResult{
			OperationState: job.State, ErrorCode: "operator_required",
		})
	}
	if job.State == "failed" || job.State == "abandoned" {
		return w.finishOperation(ctx, operation, store.BeeperMediaResult{
			OperationState: "failed", CoverageState: "unavailable", Terminal: true,
			ErrorCode: job.FailureCode,
		})
	}
	source, err := w.client.Status(actionCtx, operation.DocbankSourceID)
	if err != nil {
		return w.finishClientError(ctx, actionCtx, operation, err)
	}
	if source.VaultUID != operation.VaultUID || source.SourceID != operation.DocbankSourceID {
		return w.finishOperation(ctx, operation, store.BeeperMediaResult{ErrorCode: "destination_mismatch"})
	}
	own, err := w.ownProcessingReceipt(actionCtx, operation, source)
	if err != nil {
		return w.finishClientError(ctx, actionCtx, operation, err)
	}
	if own.VaultUID != operation.VaultUID || own.SourceID != operation.DocbankSourceID ||
		own.OperationID != operation.OperationID {
		return w.finishOperation(ctx, operation, store.BeeperMediaResult{ErrorCode: "destination_mismatch"})
	}
	result := store.BeeperMediaResult{OperationState: own.OperationState, CoverageState: own.CoverageState}
	switch {
	case own.OperationState == "failed" || own.OperationState == "cancelled":
		result.OperationState, result.Terminal = "failed", true
	case own.OperationState == "succeeded" && terminalMediaCoverage(own.CoverageState):
		result.Terminal = true
	}
	if result.OperationState == "failed" && !terminalMediaCoverage(result.CoverageState) {
		result.CoverageState = "unavailable"
	}
	return w.finishOperation(ctx, operation, result)
}

// ownProcessingReceipt returns this operation's receipt. Docbank's source
// status names only the newest operation and takes coverage from the newest
// succeeded one, so an older operation replays its saved retry receipt.
func (w *MediaSubmitter) ownProcessingReceipt(
	ctx context.Context, operation store.BeeperMediaOperation, source docbankmedia.Receipt,
) (docbankmedia.Receipt, error) {
	if source.OperationID != operation.OperationID {
		return w.client.Process(ctx, operation.DocbankSourceID, operation.OperationID, operation.SuppliedInputID)
	}
	if source.OperationState != "succeeded" {
		// Until this operation succeeds, source coverage belongs to an earlier one.
		source.CoverageState = ""
	}
	return source, nil
}

func terminalMediaCoverage(state string) bool {
	switch state {
	case "transcribed", "unavailable", "stale":
		return true
	}
	return false
}

// finishClientError records a remote failure. Parent cancellation leaves the
// operation untouched; the step's own deadline is a retryable transport fault.
func (w *MediaSubmitter) finishClientError(
	ctx, actionCtx context.Context, operation store.BeeperMediaOperation, err error,
) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	result := store.BeeperMediaResult{ErrorCode: mediaGapCode(err), Retry: docbankmedia.Retryable(err)}
	if actionCtx.Err() != nil {
		result = store.BeeperMediaResult{ErrorCode: "timeout", Retry: true}
	}
	return w.finishOperation(ctx, operation, result)
}

func (w *MediaSubmitter) finishOperation(
	ctx context.Context, operation store.BeeperMediaOperation, result store.BeeperMediaResult,
) error {
	_, err := w.finish(ctx, operation, result)
	return err
}

func (w *MediaSubmitter) finish(
	ctx context.Context, operation store.BeeperMediaOperation, result store.BeeperMediaResult,
) (bool, error) {
	var retained bool
	err := w.gated(ctx, func() (err error) {
		retained, err = w.store.FinishBeeperMediaOperation(ctx, operation, result)
		return err
	})
	return retained, err
}

func mappingCandidate(mapping store.BeeperMediaMapping) store.BeeperMediaCandidate {
	return store.BeeperMediaCandidate{
		AttachmentID: mapping.AttachmentID, MessageID: mapping.MessageID, SourceID: mapping.LocalSourceID,
		SourceType: mapping.SourceType, SourceIdentifier: mapping.SourceIdentifier,
		SourceConversationID: mapping.SourceConversationID, SourceMessageID: mapping.SourceMessageID,
		SourceAttachmentID: mapping.SourceAttachmentID, SourcePartKey: mapping.SourcePartKey,
		ContentHash: mapping.SourceSHA256, ByteLength: mapping.ByteLength,
	}
}

func describeMedia(
	raw []byte, candidate store.BeeperMediaCandidate, archiveUID string,
) (MediaDescriptor, string, error) {
	if !utf8.Valid(raw) {
		return MediaDescriptor{}, "", errBeeperMediaRawInvalid
	}
	var envelope struct {
		ID          string         `json:"id"`
		Timestamp   jsontext.Value `json:"timestamp"`
		Attachments []Attachment   `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return MediaDescriptor{}, "", errBeeperMediaRawInvalid
	}
	if candidate.SourceType != "beeper" {
		return MediaDescriptor{}, "", errBeeperMediaUnsupported
	}
	if candidate.SourceMessageID != "" && envelope.ID != candidate.SourceMessageID {
		return MediaDescriptor{}, "", errBeeperMediaSourceChanged
	}
	part := candidate.SourcePartKey
	if part == "" {
		part = candidate.SourceAttachmentID
	}
	if part == "" {
		return MediaDescriptor{}, "", errBeeperMediaPartMissing
	}
	var attachment *Attachment
	for i := range envelope.Attachments {
		value := &envelope.Attachments[i]
		ref := beeperAttachmentID(assetRef(value))
		if ref != part && ref != candidate.SourceAttachmentID {
			continue
		}
		if attachment != nil {
			return MediaDescriptor{}, "", errBeeperMediaPartAmbiguous
		}
		attachment = value
	}
	if attachment == nil {
		return MediaDescriptor{}, "", errBeeperMediaPartMissing
	}
	mediaType := mediaTypeOf(attachment)
	if (mediaType != "audio" && mediaType != "voice_note") || attachment.IsSticker || attachment.IsGif {
		return MediaDescriptor{}, "", errBeeperMediaUnsupported
	}
	transcript := sourceTranscript(attachment)
	if !utf8.ValidString(transcript) {
		return MediaDescriptor{}, "", errBeeperMediaTranscriptInvalid
	}
	if len(transcript) > beeperMediaTranscriptLimit {
		return MediaDescriptor{}, "", errBeeperMediaTranscriptTooLarge
	}
	filename, requestMIME := selectedMediaMetadata(attachment.FileName, attachment.MimeType)
	rawHash := sha256.Sum256(raw)
	transcriptHash := ""
	if transcript != "" {
		digest := sha256.Sum256([]byte(transcript))
		transcriptHash = hex.EncodeToString(digest[:])
	}
	language := ""
	if attachment.Transcription != nil {
		language = strings.TrimSpace(attachment.Transcription.Language)
	}
	timestamp := mediaTimestamp(envelope.Timestamp)
	occurrence := docbankmedia.Occurrence{
		Ref: mediaOccurrenceRef(archiveUID, candidate.SourceType, candidate.SourceIdentifier,
			candidate.SourceConversationID, candidate.SourceMessageID, part),
		Filename: filename, Message: timestamp,
	}
	descriptor := MediaDescriptor{
		Occurrence: occurrence, SourceType: candidate.SourceType,
		SourceIdentifier: candidate.SourceIdentifier, SourceConversationID: candidate.SourceConversationID,
		SourceMessageID: candidate.SourceMessageID, SourceAttachmentID: candidate.SourceAttachmentID,
		SourcePartKey: part, SourceSHA256: candidate.ContentHash, ByteLength: candidate.ByteLength,
		RawHash: hex.EncodeToString(rawHash[:]), TranscriptSHA256: transcriptHash,
		Language: language, Filename: filename, MIMEType: requestMIME,
	}
	descriptor.Occurrence.Revision = mediaRevision(descriptor)
	descriptor.ProcessingKey = mediaProcessingKey(descriptor)
	return descriptor, transcript, nil
}

func mediaRevision(descriptor MediaDescriptor) string {
	value := struct {
		SourceSHA256, Filename, MIMEType, TranscriptSHA256, Language string
		ByteLength                                                   int64
		Message                                                      docbankmedia.Timestamp
	}{
		SourceSHA256: descriptor.SourceSHA256, Filename: descriptor.Filename,
		MIMEType: descriptor.MIMEType, TranscriptSHA256: descriptor.TranscriptSHA256,
		Language: descriptor.Language, ByteLength: descriptor.ByteLength,
		Message: descriptor.Occurrence.Message,
	}
	return hashJSON(value)
}

func mediaProcessingKey(descriptor MediaDescriptor) string {
	if descriptor.TranscriptSHA256 == "" {
		return ""
	}
	return hashDelimited("beeper", "supplied-transcript", descriptor.SourceSHA256,
		descriptor.TranscriptSHA256, descriptor.Language)
}

func mediaOccurrenceRef(
	archiveUID, sourceType, sourceIdentifier, conversationID, messageID, part string,
) string {
	return "msgvault:" + hashDelimited(archiveUID, sourceType, sourceIdentifier,
		conversationID, messageID, part)
}

func hashDelimited(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = fmt.Fprintf(hash, "%d:%s;", len(value), value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func hashJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func selectedMediaMetadata(filename, mediaType string) (string, string) {
	filename = strings.TrimSpace(filename)
	baseType, _, err := mime.ParseMediaType(strings.TrimSpace(mediaType))
	if err != nil {
		baseType = ""
	}
	if filename == "" {
		if baseType == "audio/mpeg" {
			filename = "audio.mp3"
		} else {
			filename = "audio.wav"
		}
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if baseType == "" {
		switch ext {
		case ".mp3":
			baseType = "audio/mpeg"
		case ".wav":
			baseType = "audio/wav"
		}
	}
	if ext == "" {
		switch baseType {
		case "audio/mpeg":
			filename += ".mp3"
		case "audio/wav", "audio/x-wav":
			filename += ".wav"
		}
	}
	return filename, baseType
}

func mediaTimestamp(raw jsontext.Value) docbankmedia.Timestamp {
	var rawTimestamp string
	if err := json.Unmarshal(raw, &rawTimestamp); err != nil {
		return docbankmedia.Timestamp{}
	}
	result := docbankmedia.Timestamp{Raw: rawTimestamp}
	instant, err := time.Parse(time.RFC3339Nano, rawTimestamp)
	if err != nil {
		return result
	}
	result.Normalized = instant.UTC().Format(time.RFC3339Nano)
	result.Precision = "instant"
	_, offset := instant.Zone()
	result.OffsetSeconds = &offset
	zoneText := rawTimestamp
	if index := strings.LastIndexAny(zoneText, "Zz+-"); index >= 10 {
		result.ZoneText = zoneText[index:]
	}
	if result.ZoneText == "Z" || result.ZoneText == "z" {
		result.ZoneText = "Z"
		result.Timezone = "UTC"
	}
	if dot := strings.IndexByte(rawTimestamp, '.'); dot >= 0 {
		end := len(rawTimestamp)
		for index := dot + 1; index < len(rawTimestamp); index++ {
			if rawTimestamp[index] < '0' || rawTimestamp[index] > '9' {
				end = index
				break
			}
		}
		result.FractionDigits = end - dot - 1
	}
	return result
}

// mediaWireIdentity uses the only labels Docbank accepts: .wav with audio/wav, .mp3 with audio/mpeg.
func mediaWireIdentity(filename, format string) (string, string) {
	mediaType := "audio/wav"
	if format == "mp3" {
		mediaType = "audio/mpeg"
	}
	return strings.TrimSuffix(filename, filepath.Ext(filename)) + "." + format, mediaType
}

// prepareMediaUpload returns the verified spool and its inspected format; local spool I/O failures retry as source gaps.
func prepareMediaUpload(
	ctx context.Context, blobs *attachmentstore.Store, descriptor MediaDescriptor, spoolDir string,
) (*os.File, string, error) {
	if blobs == nil || descriptor.SourceSHA256 == "" || descriptor.ByteLength < 1 ||
		descriptor.ByteLength > beeperMediaSourceLimit {
		return nil, "", errBeeperMediaUnsupported
	}
	if spoolDir == "" {
		return nil, "", fmt.Errorf("%w: media spool directory is not configured", errBeeperMediaSourceUnavailable)
	}
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("%w: create media spool directory: %w", errBeeperMediaSourceUnavailable, err)
	}
	file, err := os.CreateTemp(spoolDir, "msgvault-docbank-media-*")
	if err != nil {
		return nil, "", fmt.Errorf("%w: create media spool: %w", errBeeperMediaSourceUnavailable, err)
	}
	remove := true
	defer func() {
		if remove {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()
	// The verified CAS reader is consumed through EOF and its Close error is
	// checked, so corrupt or truncated bytes never reach the upload spool.
	reader, declaredSize, err := blobs.OpenStream(ctx, descriptor.SourceSHA256)
	if err != nil {
		return nil, "", fmt.Errorf("%w: open media source", errBeeperMediaSourceUnavailable)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash),
		io.LimitReader(reader, beeperMediaSourceLimit+1))
	closeErr := reader.Close()
	if copyErr != nil || closeErr != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", fmt.Errorf("%w: verify media source", errBeeperMediaSourceUnavailable)
	}
	if declaredSize != descriptor.ByteLength || written != descriptor.ByteLength ||
		hex.EncodeToString(hash.Sum(nil)) != descriptor.SourceSHA256 {
		return nil, "", fmt.Errorf("%w: media source identity changed", errBeeperMediaSourceUnavailable)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", fmt.Errorf("%w: rewind media spool: %w", errBeeperMediaSourceUnavailable, err)
	}
	// Provider aliases such as audio/mp3 fail the inspector's identity check, so inspect under canonical labels.
	header := make([]byte, 12)
	if _, err := io.ReadFull(file, header); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, "", fmt.Errorf("%w: read media spool: %w", errBeeperMediaSourceUnavailable, err)
	}
	format := "mp3"
	if string(header[:4]) == "RIFF" && string(header[8:]) == "WAVE" {
		format = "wav"
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", fmt.Errorf("%w: rewind media spool: %w", errBeeperMediaSourceUnavailable, err)
	}
	inspectName, inspectType := mediaWireIdentity("audio", format)
	policy := media.InspectionPolicy{
		Filename: inspectName, DeclaredMediaType: inspectType,
		ExpectedBytes: descriptor.ByteLength, ExpectedSHA256: descriptor.SourceSHA256,
		DescriptorFingerprint: descriptor.SourceSHA256, ProfileFingerprint: descriptor.SourceSHA256,
		DisclosureFingerprint: descriptor.SourceSHA256, InputKind: document.RenditionInputOriginalFile,
		MaxSourceBytes: beeperMediaSourceLimit, MaxExpandedBytes: beeperMediaSourceLimit,
		MaxEntryBytes: beeperMediaSourceLimit, MaxEntries: 100, MaxNestingDepth: 1,
		MaxTextLines: 1_000_000, MaxCharacters: beeperMediaSourceLimit,
		MaxRecords: 1_000_000, MaxPages: 100_000, MaxSlides: 100_000,
		MaxSheets: 100_000, MaxCells: 10_000_000, MaxSpineItems: 100_000,
		MaxResources: 1_000_000, MaxDurationMS: 24 * 60 * 60 * 1000,
	}
	record, err := media.InspectCapability(file, policy)
	if err != nil {
		return nil, "", fmt.Errorf("%w: inspect media spool: %w", errBeeperMediaSourceUnavailable, err)
	}
	if !record.Eligible || record.Format != format {
		return nil, "", errBeeperMediaUnsupported
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", fmt.Errorf("%w: rewind media spool: %w", errBeeperMediaSourceUnavailable, err)
	}
	remove = false
	return file, record.Format, nil
}

func closeAndRemove(file *os.File) {
	if file == nil {
		return
	}
	name := file.Name()
	_ = file.Close()
	_ = os.Remove(name)
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func mediaGapCode(err error) string {
	switch {
	case errors.Is(err, errBeeperMediaRawInvalid):
		return errBeeperMediaRawInvalid.Error()
	case errors.Is(err, errBeeperMediaPartMissing):
		return errBeeperMediaPartMissing.Error()
	case errors.Is(err, errBeeperMediaPartAmbiguous):
		return errBeeperMediaPartAmbiguous.Error()
	case errors.Is(err, errBeeperMediaTranscriptInvalid):
		return errBeeperMediaTranscriptInvalid.Error()
	case errors.Is(err, errBeeperMediaTranscriptTooLarge):
		return errBeeperMediaTranscriptTooLarge.Error()
	case errors.Is(err, errBeeperMediaSourceChanged):
		return errBeeperMediaSourceChanged.Error()
	case errors.Is(err, errBeeperMediaUnsupported):
		return errBeeperMediaUnsupported.Error()
	case errors.Is(err, errBeeperMediaNoLiveOccurrence):
		return errBeeperMediaNoLiveOccurrence.Error()
	case errors.Is(err, errBeeperMediaSourceUnavailable):
		return errBeeperMediaSourceUnavailable.Error()
	default:
		return docbankmedia.ErrorCode(err)
	}
}
