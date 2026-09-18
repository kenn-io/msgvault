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
	actionTimeout time.Duration
}

// NewMediaSubmitter returns a worker for one destination. A nil client keeps
// discovery local and sends nothing.
func NewMediaSubmitter(
	st *store.Store, blobs *attachmentstore.Store, client *docbankmedia.Client, destination string,
) *MediaSubmitter {
	return &MediaSubmitter{
		store: st, blobs: blobs, client: client, destination: destination, actionTimeout: beeperMediaActionTimeout,
	}
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
		return result, err
	}
	operation, ok, err := w.store.NextBeeperMediaOperation(ctx, w.destination, time.Now().UTC())
	if err != nil || !ok {
		return result, err
	}
	// ponytail: one remote action per pass (~1,440/day); batch once a measured backlog outgrows it.
	retained, err := w.runOperation(ctx, archiveUID, operation)
	if retained {
		result.Retained++
	}
	return result, err
}

func (w *MediaSubmitter) discover(ctx context.Context, archiveUID string) (MediaBatchResult, error) {
	var result MediaBatchResult
	consumer, created, err := w.store.RegisterAttachmentChangeConsumer(ctx, store.BeeperMediaAttachmentConsumerKey)
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
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Examined++
		mapping, gap := w.mappingForCandidate(ctx, archiveUID, candidate)
		if err := w.store.ReconcileBeeperMediaMapping(ctx, mapping); err != nil {
			return result, err
		}
		if gap != nil {
			result.Blocked++
		} else {
			result.Pending++
		}
		scan.AfterAttachmentID = candidate.AttachmentID
	}
	fullPass := len(candidates) < beeperMediaScanPageSize || scan.AfterAttachmentID >= scan.PassHighWater
	if fullPass {
		scan.AfterAttachmentID, scan.PassHighWater = 0, 0
	}
	swapped, err := w.store.AdvanceBeeperMediaScan(ctx, w.destination, before, scan)
	if err != nil || !swapped {
		return result, err
	}
	if fullPass && !consumer.ReconciliationComplete && scan.BaselineSequence == consumer.BaselineSequence {
		if err := w.store.CompleteAttachmentChangeReconciliation(
			ctx, store.BeeperMediaAttachmentConsumerKey, consumer.BaselineSequence); err != nil {
			return result, err
		}
		consumer.ReconciliationComplete = true
	}
	if consumer.ReconciliationComplete {
		result.Journaled, err = w.replayJournal(ctx, archiveUID)
	}
	return result, err
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
			mapping, _ := w.mappingForCandidate(ctx, archiveUID, candidate)
			if err := w.store.ReconcileBeeperMediaMapping(ctx, mapping); err != nil {
				return 0, err
			}
		}
	}
	if err := w.store.RevokeStaleBeeperMediaMappings(ctx, w.destination); err != nil {
		return 0, err
	}
	last := changes[len(changes)-1].Sequence
	if err := w.store.AdvanceAttachmentChangeConsumer(ctx, store.BeeperMediaAttachmentConsumerKey, last); err != nil {
		return 0, err
	}
	return len(changes), nil
}

func (w *MediaSubmitter) mappingForCandidate(
	ctx context.Context, archiveUID string, candidate store.BeeperMediaCandidate,
) (store.BeeperMediaMapping, error) {
	raw, err := w.store.GetMessageRawContext(ctx, candidate.MessageID)
	if err != nil {
		return fallbackMediaMapping(w.destination, candidate, archiveUID, errBeeperMediaRawInvalid), errBeeperMediaRawInvalid
	}
	descriptor, _, err := describeMedia(raw, candidate, archiveUID)
	if err != nil {
		return fallbackMediaMapping(w.destination, candidate, archiveUID, err), err
	}
	return descriptorMapping(w.destination, candidate, descriptor), nil
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
	descriptor.Occurrence.Revision = hashDelimited("gap", mediaRevision(descriptor), mediaGapCode(cause))
	mapping := descriptorMapping(destination, candidate, descriptor)
	mapping.RetentionState, mapping.ErrorCode = store.BeeperMediaRetentionBlocked, mediaGapCode(cause)
	return mapping
}

func (w *MediaSubmitter) runOperation(
	ctx context.Context, archiveUID string, operation store.BeeperMediaOperation,
) (bool, error) {
	// Only remote and spool work runs under the step deadline, so a timed-out step can still record its retry.
	actionCtx, cancel := context.WithTimeout(ctx, w.actionTimeout)
	defer cancel()
	switch operation.Kind {
	case store.BeeperMediaOperationRetain:
		return w.retain(ctx, actionCtx, archiveUID, operation)
	case store.BeeperMediaOperationArtifact:
		return false, w.artifact(ctx, actionCtx, archiveUID, operation)
	case store.BeeperMediaOperationProcess:
		return false, w.process(ctx, actionCtx, operation)
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
	candidate, err := w.store.GetBeeperMediaCandidate(ctx, operation.AttachmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return unavailable(errBeeperMediaNoLiveOccurrence.Error())
	}
	if err != nil {
		return false, err
	}
	raw, err := w.store.GetMessageRawContext(ctx, candidate.MessageID)
	if err != nil {
		return unavailable(errBeeperMediaRawInvalid.Error())
	}
	descriptor, _, err := describeMedia(raw, candidate, archiveUID)
	if err != nil || descriptor.Occurrence.Revision != operation.Revision ||
		descriptor.SourceSHA256 != operation.SourceSHA256 {
		return unavailable(errBeeperMediaSourceChanged.Error())
	}
	file, err := prepareMediaUpload(actionCtx, w.blobs, descriptor)
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
	prepared, err := w.store.PrepareBeeperMediaOperation(ctx, operation)
	if err != nil {
		return false, err
	}
	var occurrence docbankmedia.Occurrence
	if err := json.Unmarshal([]byte(prepared.FrozenRequestJSON), &occurrence); err != nil {
		return false, fmt.Errorf("decode saved beeper media occurrence: %w", err)
	}
	receipt, err := w.client.Submit(actionCtx, docbankmedia.SuppliedMetadata{
		OperationID: prepared.OperationID, Filename: operation.Filename, MediaType: operation.MIMEType,
		SHA256: operation.SourceSHA256, ByteLength: operation.ByteLength, Occurrence: occurrence,
	}, file)
	if err != nil {
		return false, w.finishClientError(ctx, actionCtx, prepared, err)
	}
	return w.store.FinishBeeperMediaOperation(ctx, prepared, store.BeeperMediaResult{
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
	mappings, err := w.store.ListLiveBeeperMediaMappings(ctx, w.destination, operation.ProcessingKey, 100)
	if err != nil {
		return err
	}
	var donor store.BeeperMediaMapping
	var transcript string
	for _, mapping := range mappings {
		raw, err := w.store.GetMessageRawContext(ctx, mapping.MessageID)
		if err != nil {
			continue
		}
		descriptor, text, err := describeMedia(raw, mappingCandidate(mapping), archiveUID)
		if err == nil && text != "" && descriptor.ProcessingKey == operation.ProcessingKey {
			donor, transcript = mapping, text
			break
		}
	}
	if donor.DocbankOccurrenceID == "" {
		return w.finishOperation(ctx, operation, store.BeeperMediaResult{
			ErrorCode: errBeeperMediaNoLiveOccurrence.Error(), Retry: true,
		})
	}
	digest := sha256.Sum256([]byte(transcript))
	transcriptSHA := hex.EncodeToString(digest[:])
	operation.FrozenRequestJSON = mustJSON(docbankmedia.ArtifactMetadata{
		OccurrenceID: donor.DocbankOccurrenceID, Kind: "transcript", Origin: "provider",
		Provider: "beeper", Language: operation.Language, Filename: "transcript.txt",
		MediaType: "text/plain", SHA256: transcriptSHA, ByteLength: int64(len(transcript)),
	})
	operation.DocbankSourceID, operation.SourceVersionID = donor.DocbankSourceID, donor.SourceVersionID
	operation.ContentVersionID, operation.DocbankOccurrenceID = donor.ContentVersionID, donor.DocbankOccurrenceID
	prepared, err := w.store.PrepareBeeperMediaOperation(ctx, operation)
	if err != nil {
		return err
	}
	var metadata docbankmedia.ArtifactMetadata
	if err := json.Unmarshal([]byte(prepared.FrozenRequestJSON), &metadata); err != nil {
		return fmt.Errorf("decode saved beeper transcript request: %w", err)
	}
	metadata.OperationID = prepared.OperationID
	if metadata.SHA256 != transcriptSHA {
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
func (w *MediaSubmitter) process(ctx, actionCtx context.Context, operation store.BeeperMediaOperation) error {
	mappings, err := w.store.ListLiveBeeperMediaMappings(ctx, w.destination, operation.ProcessingKey, 1)
	if err != nil {
		return err
	}
	if len(mappings) == 0 {
		return w.finishOperation(ctx, operation, store.BeeperMediaResult{
			ErrorCode: errBeeperMediaNoLiveOccurrence.Error(), Retry: true,
		})
	}
	operation.FrozenRequestJSON = mustJSON(docbankmedia.Processing{
		Profile: "supplied-transcript", SuppliedInputID: operation.SuppliedInputID,
	})
	prepared, err := w.store.PrepareBeeperMediaOperation(ctx, operation)
	if err != nil {
		return err
	}
	var processing docbankmedia.Processing
	if err := json.Unmarshal([]byte(prepared.FrozenRequestJSON), &processing); err != nil {
		return fmt.Errorf("decode saved beeper processing request: %w", err)
	}
	receipt, err := w.client.Process(actionCtx, prepared.DocbankSourceID, prepared.OperationID, processing.SuppliedInputID)
	if err != nil {
		return w.finishClientError(ctx, actionCtx, prepared, err)
	}
	if receipt.VaultUID != mappings[0].VaultUID || receipt.SourceID != prepared.DocbankSourceID {
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

// status reads the exact processing job. Source coverage counts only when
// Docbank reports it for this variant's processing operation.
func (w *MediaSubmitter) status(ctx, actionCtx context.Context, operation store.BeeperMediaOperation) error {
	mappings, err := w.store.ListLiveBeeperMediaMappings(ctx, w.destination, operation.ProcessingKey, 1)
	if err != nil {
		return err
	}
	if len(mappings) == 0 {
		return w.finishOperation(ctx, operation, store.BeeperMediaResult{
			ErrorCode: errBeeperMediaNoLiveOccurrence.Error(), Retry: true,
		})
	}
	job, err := w.client.JobStatus(actionCtx, operation.JobID)
	if err != nil {
		return w.finishClientError(ctx, actionCtx, operation, err)
	}
	source, err := w.client.Status(actionCtx, operation.DocbankSourceID)
	if err != nil {
		return w.finishClientError(ctx, actionCtx, operation, err)
	}
	if source.VaultUID != mappings[0].VaultUID || source.SourceID != operation.DocbankSourceID {
		return w.finishOperation(ctx, operation, store.BeeperMediaResult{ErrorCode: "destination_mismatch"})
	}
	ownCoverage := source.OperationID == operation.OperationID
	result := store.BeeperMediaResult{OperationState: job.State}
	switch job.State {
	case "completed", "partial":
		result.OperationState, result.Terminal = "succeeded", true
		if ownCoverage && source.OperationState == "failed" {
			result.OperationState = "failed"
		} else if ownCoverage && !terminalMediaCoverage(source.CoverageState) {
			// Docbank publishes coverage after the job completes; keep polling until it settles.
			result.OperationState, result.Terminal = source.OperationState, false
		}
	case "failed", "abandoned":
		result.OperationState, result.Terminal, result.ErrorCode = "failed", true, job.FailureCode
	}
	if ownCoverage {
		result.CoverageState = source.CoverageState
	}
	return w.finishOperation(ctx, operation, result)
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
	_, err := w.store.FinishBeeperMediaOperation(ctx, operation, result)
	return err
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
	var parsedTimestamp time.Time
	_ = json.Unmarshal(envelope.Timestamp, &parsedTimestamp)
	timestamp := mediaTimestamp(raw, parsedTimestamp)
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

func mediaTimestamp(raw []byte, parsed time.Time) docbankmedia.Timestamp {
	var fields struct {
		Timestamp jsontext.Value `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields.Timestamp) == 0 {
		return docbankmedia.Timestamp{}
	}
	var rawTimestamp string
	if err := json.Unmarshal(fields.Timestamp, &rawTimestamp); err != nil {
		return docbankmedia.Timestamp{}
	}
	result := docbankmedia.Timestamp{Raw: rawTimestamp}
	if parsed.IsZero() {
		return result
	}
	instant, err := time.Parse(time.RFC3339Nano, rawTimestamp)
	if err != nil {
		return result
	}
	result.Normalized = instant.UTC().Format(time.RFC3339Nano)
	result.Precision = "instant"
	_, offset := instant.Zone()
	result.OffsetSeconds = &offset
	result.Timezone = "UTC"
	zoneText := rawTimestamp
	if index := strings.LastIndexAny(zoneText, "Zz+-"); index >= 10 {
		result.ZoneText = zoneText[index:]
	}
	if result.ZoneText == "Z" || result.ZoneText == "z" {
		result.ZoneText = "Z"
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

func prepareMediaUpload(
	ctx context.Context, blobs *attachmentstore.Store, descriptor MediaDescriptor,
) (*os.File, error) {
	if blobs == nil || descriptor.SourceSHA256 == "" || descriptor.ByteLength < 1 ||
		descriptor.ByteLength > beeperMediaSourceLimit {
		return nil, errBeeperMediaUnsupported
	}
	file, err := os.CreateTemp("", "msgvault-docbank-media-*")
	if err != nil {
		return nil, fmt.Errorf("create media spool: %w", err)
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
		return nil, fmt.Errorf("%w: open media source", errBeeperMediaSourceUnavailable)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash),
		io.LimitReader(reader, beeperMediaSourceLimit+1))
	closeErr := reader.Close()
	if copyErr != nil || closeErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: verify media source", errBeeperMediaSourceUnavailable)
	}
	if declaredSize != descriptor.ByteLength || written != descriptor.ByteLength ||
		hex.EncodeToString(hash.Sum(nil)) != descriptor.SourceSHA256 {
		return nil, fmt.Errorf("%w: media source identity changed", errBeeperMediaSourceUnavailable)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	policy := media.InspectionPolicy{
		Filename: descriptor.Filename, DeclaredMediaType: descriptor.MIMEType,
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
		return nil, errBeeperMediaUnsupported
	}
	if !record.Eligible || (record.Format != "wav" && record.Format != "mp3") {
		return nil, errBeeperMediaUnsupported
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	remove = false
	return file, nil
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
