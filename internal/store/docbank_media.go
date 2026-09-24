package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// beeperMediaLocalGapCodes are source gaps that a daemon restart cannot fix.
var beeperMediaLocalGapCodes = []any{"source_raw_invalid", "source_part_missing", "source_part_ambiguous",
	"unsupported_media", "source_transcript_invalid", "source_transcript_too_large", "source_changed",
	"source_unavailable", "no_live_occurrence"}

// beeperMediaResumedPhase is the step a reopened delivery resumes from.
const beeperMediaResumedPhase = `CASE WHEN job_id <> '' THEN 'observing'
	WHEN supplied_input_id = '' THEN 'pending-artifact' ELSE 'pending-process' END`

// beeperMediaLocalGapFilter matches error_code against beeperMediaLocalGapCodes.
func beeperMediaLocalGapFilter() string {
	return `error_code IN (?` + strings.Repeat(", ?", len(beeperMediaLocalGapCodes)-1) + `)`
}

const (
	BeeperMediaAttachmentConsumerKey = "beeper-media/v1"

	BeeperMediaRetentionPending           = "pending"
	BeeperMediaRetentionRetained          = "retained"
	BeeperMediaRetentionBlocked           = "blocked"
	BeeperMediaRetentionSourceUnavailable = "source_unavailable"
	BeeperMediaRetentionRevoked           = "revoked"

	BeeperMediaOperationRetain   = "retain"
	BeeperMediaOperationArtifact = "artifact"
	BeeperMediaOperationProcess  = "process"
	BeeperMediaOperationStatus   = "status"

	beeperMediaPhasePendingArtifact = "pending-artifact"
	beeperMediaPhasePendingProcess  = "pending-process"
	beeperMediaPhaseObserving       = "observing"
	beeperMediaPhaseDone            = "done"
	beeperMediaPhaseBlocked         = "blocked"

	beeperMediaProcessingProfile = "supplied-transcript"
	beeperMediaRetryDelay        = 5 * time.Minute
	beeperMediaPollDelay         = time.Minute
)

// beeperMediaEligible is the shared provider, capture and role predicate. It
// assumes the aliases a (attachments), m (messages) and src (sources).
const beeperMediaEligible = `src.source_type = 'beeper'
	  AND length(COALESCE(a.content_hash, '')) = 64
	  AND COALESCE(a.storage_path, '') <> ''
	  AND COALESCE(a.attachment_state, '') = 'stored'
	  AND COALESCE(a.media_type, '') IN ('audio', 'voice_note')
	  AND COALESCE(a.attachment_role, 'unknown') = 'standalone'`

// beeperMediaCurrentJoin follows occurrence row o to its live source
// attachment by stable source tuple, never by attachment row ID.
var beeperMediaCurrentJoin = `
	JOIN sources src ON src.source_type = o.source_type AND src.identifier = o.source_identifier
	JOIN messages m ON m.source_id = src.id AND m.source_message_id = o.source_message_id
	JOIN conversations c ON c.id = m.conversation_id
	JOIN attachments a ON a.message_id = m.id
	WHERE c.source_conversation_id = o.source_conversation_id
	  AND COALESCE(a.source_attachment_id, '') = o.source_attachment_id
	  AND COALESCE(NULLIF(a.source_part_key, ''), a.source_attachment_id, '') = o.source_part_key
	  AND a.content_hash = o.source_sha256
	  AND ` + beeperMediaEligible + `
	  AND ` + LiveMessagesWhere("m", true)

// BeeperMediaCandidate is the current archive evidence for one stored Beeper
// audio occurrence. The source tuple, rather than AttachmentID, is stable.
type BeeperMediaCandidate struct {
	AttachmentID         int64
	MessageID            int64
	ConversationID       int64
	SourceID             int64
	SourceType           string
	SourceIdentifier     string
	SourceConversationID string
	SourceMessageID      string
	SourceAttachmentID   string
	SourcePartKey        string
	Filename             string
	MIMEType             string
	MediaType            string
	Role                 string
	ContentHash          string
	ByteLength           int64
	AttachmentState      string
}

// BeeperMediaMapping is one occurrence row together with the optional shared
// processing row. Text and bytes stay in their existing archive owners.
type BeeperMediaMapping struct {
	DestinationKey       string
	OccurrenceRef        string
	Revision             string
	SourceType           string
	SourceIdentifier     string
	SourceConversationID string
	SourceMessageID      string
	SourceAttachmentID   string
	SourcePartKey        string
	LocalSourceID        int64
	MessageID            int64
	AttachmentID         int64
	SourceSHA256         string
	ByteLength           int64
	RawHash              string
	TranscriptSHA256     string
	Language             string
	OccurrenceJSON       string
	Filename             string
	MIMEType             string
	RetentionOperationID string
	RetentionState       string
	NextActionAt         time.Time
	ErrorCode            string

	VaultUID            string
	DocbankSourceID     string
	SourceVersionID     string
	ContentVersionID    string
	DocbankOccurrenceID string
	CoverageState       string
	ProcessingKey       string

	ProcessingPhase       string
	ProcessingOperationID string
	DonorOccurrenceID     string
	SuppliedInputID       string
	JobID                 string
	OperationState        string
	ProcessingCoverage    string
}

// BeeperMediaOperation is the one remote action selected by a bounded pass.
// FrozenRequestJSON holds the saved nonsecret request for replay.
type BeeperMediaOperation struct {
	Kind                string
	DestinationKey      string
	OccurrenceRef       string
	Revision            string
	ProcessingKey       string
	OperationID         string
	FrozenRequestJSON   string
	SourceSHA256        string
	ByteLength          int64
	Filename            string
	MIMEType            string
	TranscriptSHA256    string
	Language            string
	MessageID           int64
	AttachmentID        int64
	VaultUID            string // expected receipt destination, carried only in memory
	DocbankSourceID     string
	SourceVersionID     string
	ContentVersionID    string
	DocbankOccurrenceID string
	SuppliedInputID     string
	JobID               string
	NextActionAt        time.Time
}

// BeeperMediaResult contains only receipt identity and stable error state.
// Raw response bodies never cross this boundary.
type BeeperMediaResult struct {
	VaultUID            string
	DocbankSourceID     string
	SourceVersionID     string
	ContentVersionID    string
	DocbankOccurrenceID string
	SuppliedInputID     string
	JobID               string
	OperationState      string
	CoverageState       string
	Terminal            bool
	ErrorCode           string
	Retry               bool
	SourceUnavailable   bool
	Revoked             bool
}

// BeeperMediaScan is a namespaced rolling scan checkpoint. BaselineSequence
// ties a pass to the journal registration it can complete.
type BeeperMediaScan struct {
	DestinationKey    string    `json:"destination_key"`
	AfterAttachmentID int64     `json:"after_attachment_id"`
	PassHighWater     int64     `json:"pass_high_water"`
	BaselineSequence  int64     `json:"baseline_sequence"`
	NextFullScanAt    time.Time `json:"next_full_scan_at,omitzero"`
}

const beeperMediaCandidateColumns = `
	SELECT a.id, m.id, c.id, src.id, src.source_type, src.identifier,
	       COALESCE(c.source_conversation_id, ''), COALESCE(m.source_message_id, ''),
	       COALESCE(a.source_attachment_id, ''),
	       COALESCE(NULLIF(a.source_part_key, ''), a.source_attachment_id, ''),
	       COALESCE(a.filename, ''), COALESCE(a.mime_type, ''),
	       COALESCE(a.media_type, ''), COALESCE(a.attachment_role, 'unknown'),
	       COALESCE(a.content_hash, ''), COALESCE(a.size, 0),
	       COALESCE(a.attachment_state, '')
	FROM attachments a
	JOIN messages m ON m.id = a.message_id
	JOIN conversations c ON c.id = m.conversation_id
	JOIN sources src ON src.id = m.source_id`

// ListBeeperMediaCandidates returns a bounded page of stored, standalone
// Beeper audio on live messages. Provider ownership is an explicit filter.
func (s *Store) ListBeeperMediaCandidates(
	ctx context.Context, afterID int64, limit int,
) ([]BeeperMediaCandidate, error) {
	if afterID < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("beeper media candidate page is invalid")
	}
	return s.queryBeeperMediaCandidates(ctx, `a.id > ?`, afterID, limit)
}

// GetBeeperMediaCandidate resolves one journaled attachment row through the
// current source, capture and visibility predicates.
func (s *Store) GetBeeperMediaCandidate(ctx context.Context, attachmentID int64) (BeeperMediaCandidate, error) {
	rows, err := s.queryBeeperMediaCandidates(ctx, `a.id = ?`, attachmentID, 1)
	if err != nil {
		return BeeperMediaCandidate{}, err
	}
	if len(rows) == 0 {
		return BeeperMediaCandidate{}, sql.ErrNoRows
	}
	return rows[0], nil
}

func (s *Store) queryBeeperMediaCandidates(
	ctx context.Context, idFilter string, id int64, limit int,
) ([]BeeperMediaCandidate, error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(beeperMediaCandidateColumns+`
		WHERE `+idFilter+` AND `+beeperMediaEligible+` AND `+LiveMessagesWhere("m", true)+`
		ORDER BY a.id
		LIMIT ?`), id, limit)
	if err != nil {
		return nil, fmt.Errorf("list beeper media candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]BeeperMediaCandidate, 0, limit)
	for rows.Next() {
		var candidate BeeperMediaCandidate
		if err := rows.Scan(
			&candidate.AttachmentID, &candidate.MessageID, &candidate.ConversationID,
			&candidate.SourceID, &candidate.SourceType, &candidate.SourceIdentifier,
			&candidate.SourceConversationID, &candidate.SourceMessageID,
			&candidate.SourceAttachmentID, &candidate.SourcePartKey,
			&candidate.Filename, &candidate.MIMEType, &candidate.MediaType,
			&candidate.Role, &candidate.ContentHash, &candidate.ByteLength,
			&candidate.AttachmentState,
		); err != nil {
			return nil, fmt.Errorf("scan beeper media candidate: %w", err)
		}
		result = append(result, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate beeper media candidates: %w", err)
	}
	return result, nil
}

// BeeperMediaAttachmentHighWater returns the current highest attachment ID.
func (s *Store) BeeperMediaAttachmentHighWater(ctx context.Context) (int64, error) {
	var highWater int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM attachments`).Scan(&highWater); err != nil {
		return 0, fmt.Errorf("read beeper media high-water mark: %w", err)
	}
	return highWater, nil
}

// ReconcileBeeperMediaMapping records the current revision of one occurrence.
// Other revisions of that occurrence become revoked. Receipt identity and a
// saved operation ID survive; an unchanged row is left untouched.
func (s *Store) ReconcileBeeperMediaMapping(ctx context.Context, mapping BeeperMediaMapping) error {
	if err := validateBeeperMediaMapping(mapping); err != nil {
		return err
	}
	existing, err := s.readBeeperMediaOccurrence(boundQuerier{ctx: ctx, q: s.db},
		mapping.DestinationKey, mapping.OccurrenceRef, mapping.Revision)
	if err == nil && existing.RetentionState != BeeperMediaRetentionRevoked &&
		sameBeeperMediaOccurrence(existing, reconciledBeeperMediaOccurrence(existing, mapping)) {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read beeper media occurrence: %w", err)
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if _, err := q.Exec(`
			UPDATE beeper_media_occurrences
			SET retention_state = 'revoked', next_action_at = NULL, updated_at = `+s.dialect.Now()+`
			WHERE destination_key = ? AND occurrence_ref = ? AND revision <> ?
			  AND retention_state <> 'revoked'`,
			mapping.DestinationKey, mapping.OccurrenceRef, mapping.Revision); err != nil {
			return fmt.Errorf("revoke old beeper media revisions: %w", err)
		}

		existing, err := s.readBeeperMediaOccurrence(q, mapping.DestinationKey, mapping.OccurrenceRef, mapping.Revision)
		reopened := true
		if errors.Is(err, sql.ErrNoRows) {
			if mapping.RetentionState != BeeperMediaRetentionBlocked {
				mapping.RetentionState = BeeperMediaRetentionPending
				mapping.RetentionOperationID = newBeeperMediaOperationID()
				mapping.NextActionAt = time.Now().UTC()
				mapping.ErrorCode = ""
			} else {
				mapping.RetentionOperationID = ""
				mapping.NextActionAt = time.Time{}
			}
			if err := s.insertBeeperMediaOccurrence(q, mapping); err != nil {
				return err
			}
		} else if err != nil {
			return fmt.Errorf("read beeper media occurrence: %w", err)
		} else {
			next := reconciledBeeperMediaOccurrence(existing, mapping)
			if !sameBeeperMediaOccurrence(existing, next) {
				if err := s.updateBeeperMediaOccurrence(q, next); err != nil {
					return err
				}
			}
			reopened = existing.RetentionState != next.RetentionState
			mapping = next
		}

		if mapping.ProcessingKey != "" {
			if _, err := q.Exec(`
			INSERT INTO beeper_media_deliveries
				(destination_key, processing_key, source_sha256, byte_length, transcript_sha256,
				language, provider, profile, phase, next_action_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 'beeper', ?, 'pending-artifact', ?, `+s.dialect.Now()+`, `+s.dialect.Now()+`)
			ON CONFLICT (destination_key, processing_key) DO NOTHING`,
				mapping.DestinationKey, mapping.ProcessingKey, mapping.SourceSHA256, mapping.ByteLength,
				mapping.TranscriptSHA256, mapping.Language, beeperMediaProcessingProfile,
				s.timestampValue(time.Now().UTC())); err != nil {
				return fmt.Errorf("create beeper media processing state: %w", err)
			}
			if reopened {
				// A new or restored occurrence can supply audio again, so a delivery retired by a source gap resumes.
				if _, err := q.Exec(`
			UPDATE beeper_media_deliveries
			SET phase = `+beeperMediaResumedPhase+`, next_action_at = ?, error_code = '', updated_at = `+s.dialect.Now()+`
			WHERE destination_key = ? AND processing_key = ? AND phase = 'blocked' AND `+beeperMediaLocalGapFilter(),
					append([]any{s.timestampValue(time.Now().UTC()), mapping.DestinationKey, mapping.ProcessingKey},
						beeperMediaLocalGapCodes...)...); err != nil {
					return fmt.Errorf("reopen beeper media processing state: %w", err)
				}
			}
		}
		retireCode := errBeeperMediaNoLiveOccurrenceCode
		if mapping.RetentionState == BeeperMediaRetentionBlocked {
			retireCode = mapping.ErrorCode
		}
		if err := s.retireBeeperMediaDeliveries(q, mapping.DestinationKey, mapping.OccurrenceRef, retireCode); err != nil {
			return err
		}
		return nil
	})
}

const errBeeperMediaNoLiveOccurrenceCode = "no_live_occurrence"

// retireBeeperMediaDeliveries blocks an unstarted transcript delivery of this
// occurrence once no pending or retained occurrence can supply its audio.
func (s *Store) retireBeeperMediaDeliveries(q boundQuerier, destination, occurrenceRef, code string) error {
	if !slices.Contains(beeperMediaLocalGapCodes, any(code)) {
		return nil
	}
	if _, err := q.Exec(`
		UPDATE beeper_media_deliveries
		SET phase = 'blocked', next_action_at = NULL, error_code = ?, updated_at = `+s.dialect.Now()+`
		WHERE destination_key = ?
		  AND (phase = 'pending-artifact' OR (phase = 'pending-process' AND COALESCE(job_id, '') = ''))
		  AND processing_key IN (
			SELECT processing_key FROM beeper_media_occurrences
			WHERE destination_key = ? AND occurrence_ref = ? AND processing_key <> '')
		  AND NOT EXISTS (
			SELECT 1 FROM beeper_media_occurrences o
			WHERE o.destination_key = beeper_media_deliveries.destination_key
			  AND o.processing_key = beeper_media_deliveries.processing_key
			  AND o.retention_state IN ('pending', 'source_unavailable', 'retained'))`,
		code, destination, destination, occurrenceRef); err != nil {
		return fmt.Errorf("retire beeper media processing state: %w", err)
	}
	return nil
}

// reconciledBeeperMediaOccurrence merges current local evidence into a saved
// row. Receipts restore a revoked revision; a gap never discards a receipt.
func reconciledBeeperMediaOccurrence(existing, current BeeperMediaMapping) BeeperMediaMapping {
	next := existing
	next.SourceIdentifier, next.SourceConversationID = current.SourceIdentifier, current.SourceConversationID
	next.SourceMessageID, next.SourceAttachmentID = current.SourceMessageID, current.SourceAttachmentID
	next.SourcePartKey, next.LocalSourceID = current.SourcePartKey, current.LocalSourceID
	next.MessageID, next.AttachmentID, next.RawHash = current.MessageID, current.AttachmentID, current.RawHash
	switch {
	case existing.DocbankSourceID != "":
		next.RetentionState = BeeperMediaRetentionRetained
	case current.RetentionState == BeeperMediaRetentionBlocked:
		next.RetentionState, next.ErrorCode, next.NextActionAt = BeeperMediaRetentionBlocked, current.ErrorCode, time.Time{}
	case existing.RetentionState == BeeperMediaRetentionRevoked,
		existing.RetentionState == BeeperMediaRetentionBlocked && existing.RetentionOperationID == "":
		next.RetentionState, next.ErrorCode, next.NextActionAt = BeeperMediaRetentionPending, "", time.Now().UTC()
		if next.RetentionOperationID == "" {
			next.RetentionOperationID = newBeeperMediaOperationID()
		}
	}
	return next
}

func sameBeeperMediaOccurrence(a, b BeeperMediaMapping) bool {
	return a.SourceIdentifier == b.SourceIdentifier && a.SourceConversationID == b.SourceConversationID &&
		a.SourceMessageID == b.SourceMessageID && a.SourceAttachmentID == b.SourceAttachmentID &&
		a.SourcePartKey == b.SourcePartKey && a.LocalSourceID == b.LocalSourceID &&
		a.MessageID == b.MessageID && a.AttachmentID == b.AttachmentID && a.RawHash == b.RawHash &&
		a.RetentionState == b.RetentionState && a.RetentionOperationID == b.RetentionOperationID &&
		a.ErrorCode == b.ErrorCode && a.NextActionAt.Equal(b.NextActionAt)
}

// ListLiveBeeperMediaMappings returns retained mappings whose current source,
// message, part and bytes still agree with the saved occurrence.
// Mappings that no longer agree enter the revoked state.
func (s *Store) ListLiveBeeperMediaMappings(
	ctx context.Context, destination, processingKey string, limit int,
) ([]BeeperMediaMapping, error) {
	if destination == "" || limit < 1 || limit > 1000 {
		return nil, errors.New("beeper media mapping query is invalid")
	}
	if err := s.RevokeStaleBeeperMediaMappings(ctx, destination); err != nil {
		return nil, err
	}
	args := []any{destination}
	processingFilter := ""
	if processingKey != "" {
		processingFilter = ` AND o.processing_key = ?`
		args = append(args, processingKey)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT `+beeperMediaOccurrenceColumns("o")+`,
		       COALESCE(d.phase, ''), COALESCE(d.processing_operation_id, ''),
		       COALESCE(d.donor_occurrence_id, ''), COALESCE(d.supplied_input_id, ''),
		       COALESCE(d.job_id, ''), COALESCE(d.operation_state, ''), COALESCE(d.coverage_state, '')
		FROM beeper_media_occurrences o
		LEFT JOIN beeper_media_deliveries d
		  ON d.destination_key = o.destination_key AND d.processing_key = o.processing_key
		WHERE o.destination_key = ? AND o.retention_state = 'retained'`+processingFilter+`
		ORDER BY o.occurrence_ref, o.revision
		LIMIT ?`), args...)
	if err != nil {
		return nil, fmt.Errorf("list live beeper media mappings: %w", err)
	}
	var candidates []BeeperMediaMapping
	for rows.Next() {
		var mapping BeeperMediaMapping
		dest := beeperMediaOccurrenceScanTargets(&mapping)
		dest = append(dest, &mapping.ProcessingPhase, &mapping.ProcessingOperationID,
			&mapping.DonorOccurrenceID, &mapping.SuppliedInputID, &mapping.JobID,
			&mapping.OperationState, &mapping.ProcessingCoverage)
		var next sql.NullTime
		dest[beeperMediaNextActionIndex] = &next
		if err := rows.Scan(dest...); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan beeper media mapping: %w", err)
		}
		mapping.NextActionAt = nullTimeValue(next)
		candidates = append(candidates, mapping)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate live beeper media mappings: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close live beeper media mappings: %w", err)
	}
	mappings := make([]BeeperMediaMapping, 0, len(candidates))
	for _, mapping := range candidates {
		live, err := s.currentBeeperMediaMessage(ctx, mapping)
		if err != nil {
			return nil, err
		}
		if !live {
			if err := s.markBeeperMediaMappingRevoked(ctx, mapping); err != nil {
				return nil, err
			}
			continue
		}
		mappings = append(mappings, mapping)
	}
	return mappings, nil
}

// currentBeeperMediaMessage resolves the saved source tuple to its current
// live message and attachment. Unrelated raw-message changes do not revoke it.
func (s *Store) currentBeeperMediaMessage(ctx context.Context, mapping BeeperMediaMapping) (bool, error) {
	var messageID int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT m.id FROM beeper_media_occurrences o`+beeperMediaCurrentJoin+`
		  AND o.destination_key = ? AND o.occurrence_ref = ? AND o.revision = ?
		LIMIT 1`), mapping.DestinationKey, mapping.OccurrenceRef, mapping.Revision).Scan(&messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve beeper media occurrence: %w", err)
	}
	return true, nil
}

// RevokeStaleBeeperMediaMappings moves non-revoked mappings without a current
// live source attachment into the revoked state.
func (s *Store) RevokeStaleBeeperMediaMappings(ctx context.Context, destination string) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		rows, err := tx.QueryContext(ctx, `
			SELECT occurrence_ref
			FROM beeper_media_occurrences
			WHERE destination_key = ? AND retention_state <> 'revoked'
			  AND NOT EXISTS (SELECT 1 FROM beeper_media_occurrences o`+beeperMediaCurrentJoin+`
				AND o.destination_key = beeper_media_occurrences.destination_key
				AND o.occurrence_ref = beeper_media_occurrences.occurrence_ref
				AND o.revision = beeper_media_occurrences.revision)`, destination)
		if err != nil {
			return fmt.Errorf("list stale beeper media mappings: %w", err)
		}
		var stale []string
		for rows.Next() {
			var occurrenceRef string
			if err := rows.Scan(&occurrenceRef); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan stale beeper media mapping: %w", err)
			}
			stale = append(stale, occurrenceRef)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate stale beeper media mappings: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close stale beeper media mappings: %w", err)
		}
		if _, err := q.Exec(`
			UPDATE beeper_media_occurrences
			SET retention_state = 'revoked', error_code = 'no_live_occurrence', next_action_at = NULL, updated_at = `+s.dialect.Now()+`
			WHERE destination_key = ? AND retention_state <> 'revoked'
			  AND NOT EXISTS (SELECT 1 FROM beeper_media_occurrences o`+beeperMediaCurrentJoin+`
				AND o.destination_key = beeper_media_occurrences.destination_key
				AND o.occurrence_ref = beeper_media_occurrences.occurrence_ref
				AND o.revision = beeper_media_occurrences.revision)`, destination); err != nil {
			return fmt.Errorf("revoke stale beeper media mappings: %w", err)
		}
		for _, occurrenceRef := range stale {
			if err := s.retireBeeperMediaDeliveries(q, destination, occurrenceRef, errBeeperMediaNoLiveOccurrenceCode); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) markBeeperMediaMappingRevoked(ctx context.Context, mapping BeeperMediaMapping) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if _, err := q.Exec(`
			UPDATE beeper_media_occurrences SET retention_state = 'revoked', next_action_at = NULL, updated_at = `+s.dialect.Now()+`
			WHERE destination_key = ? AND occurrence_ref = ? AND revision = ? AND retention_state = 'retained'`,
			mapping.DestinationKey, mapping.OccurrenceRef, mapping.Revision); err != nil {
			return fmt.Errorf("revoke beeper media mapping: %w", err)
		}
		return s.retireBeeperMediaDeliveries(q, mapping.DestinationKey, mapping.OccurrenceRef,
			errBeeperMediaNoLiveOccurrenceCode)
	})
}

// NextBeeperMediaOperation chooses the earliest ready retention or shared
// processing step. Unstarted processing steps need a retained live member.
func (s *Store) NextBeeperMediaOperation(
	ctx context.Context, destination string, now time.Time,
) (BeeperMediaOperation, bool, error) {
	if destination == "" {
		return BeeperMediaOperation{}, false, errors.New("beeper media destination is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var retain BeeperMediaOperation
	var retainNext sql.NullTime
	retainErr := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT retention_operation_id, destination_key, occurrence_ref, revision,
		       source_sha256, byte_length, request_filename, request_mime_type,
		       transcript_sha256, language, message_id, attachment_id, occurrence_json,
		       processing_key, next_action_at
		FROM beeper_media_occurrences
		WHERE destination_key = ? AND retention_state IN ('pending', 'source_unavailable')
		  AND retention_operation_id <> '' AND next_action_at IS NOT NULL AND next_action_at <= ?
		ORDER BY next_action_at, occurrence_ref, revision
		LIMIT 1`), destination, s.dialect.TimestampParam(now)).Scan(
		&retain.OperationID, &retain.DestinationKey, &retain.OccurrenceRef, &retain.Revision,
		&retain.SourceSHA256, &retain.ByteLength, &retain.Filename, &retain.MIMEType,
		&retain.TranscriptSHA256, &retain.Language, &retain.MessageID, &retain.AttachmentID,
		&retain.FrozenRequestJSON, &retain.ProcessingKey, &retainNext)
	if retainErr != nil && !errors.Is(retainErr, sql.ErrNoRows) {
		return BeeperMediaOperation{}, false, fmt.Errorf("select beeper media retention operation: %w", retainErr)
	}
	retain.Kind = BeeperMediaOperationRetain
	retain.NextActionAt = nullTimeValue(retainNext)

	var delivery BeeperMediaOperation
	var phase, pendingID, processingID, frozen string
	var deliveryNext sql.NullTime
	deliveryErr := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT d.phase, d.destination_key, d.processing_key, COALESCE(d.pending_operation_id, ''),
		       d.processing_operation_id, d.source_sha256, d.byte_length, d.transcript_sha256,
		       d.language, d.source_id, d.source_version_id, d.content_version_id,
		       d.donor_occurrence_id, d.supplied_input_id, d.job_id,
		       COALESCE(d.frozen_request_json, ''),
		       COALESCE((SELECT o.vault_uid FROM beeper_media_occurrences o
			WHERE o.destination_key = d.destination_key AND o.processing_key = d.processing_key
			  AND o.vault_uid <> '' LIMIT 1), ''), d.next_action_at
		FROM beeper_media_deliveries d
		WHERE d.destination_key = ?
		  AND d.phase IN ('pending-artifact', 'pending-process', 'observing')
		  AND d.next_action_at IS NOT NULL AND d.next_action_at <= ?
		  AND (
			(d.phase IN ('pending-artifact', 'pending-process') AND EXISTS (
				SELECT 1 FROM beeper_media_occurrences o`+beeperMediaCurrentJoin+`
				  AND o.destination_key = d.destination_key AND o.processing_key = d.processing_key
				  AND o.retention_state = 'retained'))
			OR (d.phase = 'observing' AND d.job_id <> '' AND EXISTS (
				SELECT 1 FROM beeper_media_occurrences o
				WHERE o.destination_key = d.destination_key AND o.processing_key = d.processing_key
				  AND o.vault_uid <> ''))
		  )
		ORDER BY d.next_action_at, d.processing_key
		LIMIT 1`), destination, s.dialect.TimestampParam(now)).Scan(
		&phase, &delivery.DestinationKey, &delivery.ProcessingKey, &pendingID, &processingID,
		&delivery.SourceSHA256, &delivery.ByteLength, &delivery.TranscriptSHA256, &delivery.Language,
		&delivery.DocbankSourceID, &delivery.SourceVersionID, &delivery.ContentVersionID,
		&delivery.DocbankOccurrenceID, &delivery.SuppliedInputID, &delivery.JobID, &frozen, &delivery.VaultUID, &deliveryNext)
	if deliveryErr != nil && !errors.Is(deliveryErr, sql.ErrNoRows) {
		return BeeperMediaOperation{}, false, fmt.Errorf("select beeper media processing operation: %w", deliveryErr)
	}
	delivery.NextActionAt = nullTimeValue(deliveryNext)
	delivery.FrozenRequestJSON = frozen
	switch phase {
	case beeperMediaPhasePendingArtifact:
		delivery.Kind, delivery.OperationID = BeeperMediaOperationArtifact, pendingID
	case beeperMediaPhasePendingProcess:
		delivery.Kind, delivery.OperationID = BeeperMediaOperationProcess, pendingID
	case beeperMediaPhaseObserving:
		delivery.Kind, delivery.OperationID = BeeperMediaOperationStatus, processingID
	}

	switch {
	case retainErr == nil && deliveryErr == nil:
		if delivery.NextActionAt.Before(retain.NextActionAt) {
			return delivery, true, nil
		}
		return retain, true, nil
	case retainErr == nil:
		return retain, true, nil
	case deliveryErr == nil:
		return delivery, true, nil
	default:
		return BeeperMediaOperation{}, false, nil
	}
}

// PrepareBeeperMediaOperation saves an operation ID and its frozen nonsecret
// request before a remote call. A saved request wins over the caller's copy,
// so every replay sends the same UUIDv4 and metadata.
func (s *Store) PrepareBeeperMediaOperation(
	ctx context.Context, operation BeeperMediaOperation,
) (BeeperMediaOperation, error) {
	if err := validateBeeperMediaOperation(operation); err != nil {
		return BeeperMediaOperation{}, err
	}
	if operation.Kind == BeeperMediaOperationStatus {
		return operation, nil
	}
	if len(operation.FrozenRequestJSON) > 64<<10 {
		return BeeperMediaOperation{}, errors.New("beeper media frozen request is too large")
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if operation.Kind == BeeperMediaOperationRetain {
			var storedID, frozen, state string
			if err := q.QueryRow(`
				SELECT retention_operation_id, occurrence_json, retention_state
				FROM beeper_media_occurrences
				WHERE destination_key = ? AND occurrence_ref = ? AND revision = ?`,
				operation.DestinationKey, operation.OccurrenceRef, operation.Revision).Scan(&storedID, &frozen, &state); err != nil {
				return fmt.Errorf("read beeper media retention operation: %w", err)
			}
			if state != BeeperMediaRetentionPending && state != BeeperMediaRetentionSourceUnavailable {
				return errors.New("beeper media retention is no longer pending")
			}
			if storedID == "" {
				storedID = newBeeperMediaOperationID()
				if _, err := q.Exec(`
					UPDATE beeper_media_occurrences SET retention_operation_id = ?, updated_at = `+s.dialect.Now()+`
					WHERE destination_key = ? AND occurrence_ref = ? AND revision = ?`,
					storedID, operation.DestinationKey, operation.OccurrenceRef, operation.Revision); err != nil {
					return fmt.Errorf("save beeper media retention operation: %w", err)
				}
			}
			operation.OperationID, operation.FrozenRequestJSON = storedID, frozen
			return nil
		}
		var storedID, frozen, sourceID, sourceVersion, contentVersion, donor, supplied, phase string
		if err := q.QueryRow(`
			SELECT COALESCE(pending_operation_id, ''), COALESCE(frozen_request_json, ''),
			       source_id, source_version_id, content_version_id, donor_occurrence_id,
			       supplied_input_id, phase
			FROM beeper_media_deliveries
			WHERE destination_key = ? AND processing_key = ?`,
			operation.DestinationKey, operation.ProcessingKey).Scan(
			&storedID, &frozen, &sourceID, &sourceVersion, &contentVersion, &donor, &supplied, &phase); err != nil {
			return fmt.Errorf("read beeper media delivery operation: %w", err)
		}
		wantPhase := beeperMediaPhasePendingArtifact
		if operation.Kind == BeeperMediaOperationProcess {
			wantPhase = beeperMediaPhasePendingProcess
		}
		if phase != wantPhase {
			return errors.New("beeper media processing step is no longer pending")
		}
		if storedID != "" && frozen != "" {
			operation.OperationID, operation.FrozenRequestJSON = storedID, frozen
			operation.DocbankSourceID, operation.SourceVersionID = sourceID, sourceVersion
			operation.ContentVersionID, operation.DocbankOccurrenceID = contentVersion, donor
			operation.SuppliedInputID = supplied
			return nil
		}
		if storedID == "" {
			storedID = newBeeperMediaOperationID()
		}
		if operation.FrozenRequestJSON == "" {
			return errors.New("beeper media request metadata is required")
		}
		operation.OperationID = storedID
		if operation.Kind == BeeperMediaOperationProcess {
			operation.DocbankSourceID, operation.SourceVersionID = sourceID, sourceVersion
			operation.ContentVersionID, operation.DocbankOccurrenceID = contentVersion, donor
			operation.SuppliedInputID = supplied
		}
		if _, err := q.Exec(`
			UPDATE beeper_media_deliveries
			SET pending_operation_id = ?, frozen_request_json = ?, source_id = ?,
			    source_version_id = ?, content_version_id = ?, donor_occurrence_id = ?,
			    error_code = '', updated_at = `+s.dialect.Now()+`
			WHERE destination_key = ? AND processing_key = ?`,
			operation.OperationID, operation.FrozenRequestJSON, operation.DocbankSourceID,
			operation.SourceVersionID, operation.ContentVersionID, operation.DocbankOccurrenceID,
			operation.DestinationKey, operation.ProcessingKey); err != nil {
			return fmt.Errorf("save beeper media delivery operation: %w", err)
		}
		return nil
	})
	if err != nil {
		return BeeperMediaOperation{}, err
	}
	return operation, nil
}

// FinishBeeperMediaOperation applies a result only when its pending identity
// still matches. The bool reports whether this completion won. Retention
// receipts own audio identity; later receipts never replace it.
func (s *Store) FinishBeeperMediaOperation(
	ctx context.Context, operation BeeperMediaOperation, result BeeperMediaResult,
) (bool, error) {
	if err := validateBeeperMediaOperation(operation); err != nil {
		return false, err
	}
	if operation.OperationID == "" && operation.Kind != BeeperMediaOperationStatus {
		return false, errors.New("beeper media operation ID is required")
	}
	if len(result.ErrorCode) > 128 {
		return false, errors.New("beeper media error code is too long")
	}
	now := time.Now().UTC()
	retryAt := s.timestampValue(now.Add(beeperMediaRetryDelay))
	var applied bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		var res sql.Result
		var err error
		var retireCode string
		switch operation.Kind {
		case BeeperMediaOperationRetain:
			where := `WHERE destination_key = ? AND occurrence_ref = ? AND revision = ?
				  AND retention_operation_id = ? AND retention_state IN ('pending', 'source_unavailable')`
			key := []any{operation.DestinationKey, operation.OccurrenceRef, operation.Revision, operation.OperationID}
			if result.ErrorCode == "" && result.VaultUID != "" {
				var boundVault string
				if err := q.QueryRow(`
					SELECT vault_uid FROM beeper_media_occurrences
					WHERE destination_key = ? AND vault_uid <> '' LIMIT 1`,
					operation.DestinationKey).Scan(&boundVault); err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("read beeper media vault binding: %w", err)
				}
				if boundVault != "" && boundVault != result.VaultUID {
					result = BeeperMediaResult{ErrorCode: "destination_mismatch"}
				}
			}
			if result.ErrorCode != "" {
				state, next := BeeperMediaRetentionBlocked, any(nil)
				if result.Revoked {
					state = BeeperMediaRetentionRevoked
				} else if result.Retry {
					state, next = BeeperMediaRetentionPending, retryAt
				} else if result.SourceUnavailable {
					state, next = BeeperMediaRetentionSourceUnavailable, retryAt
				}
				res, err = q.Exec(`
					UPDATE beeper_media_occurrences
					SET retention_state = ?, next_action_at = ?, error_code = ?, updated_at = `+s.dialect.Now()+`
					`+where, append([]any{state, next, result.ErrorCode}, key...)...)
				if state == BeeperMediaRetentionBlocked || state == BeeperMediaRetentionRevoked {
					retireCode = result.ErrorCode
				}
				break
			}
			res, err = q.Exec(`
				UPDATE beeper_media_occurrences
				SET vault_uid = ?, source_id = ?, source_version_id = ?, content_version_id = ?,
				    occurrence_id = ?, coverage_state = ?, retention_state = 'retained',
				    next_action_at = NULL, error_code = '', updated_at = `+s.dialect.Now()+`
				`+where, append([]any{result.VaultUID, result.DocbankSourceID, result.SourceVersionID,
				result.ContentVersionID, result.DocbankOccurrenceID, result.CoverageState}, key...)...)

		case BeeperMediaOperationArtifact, BeeperMediaOperationProcess:
			phase := beeperMediaPhasePendingArtifact
			if operation.Kind == BeeperMediaOperationProcess {
				phase = beeperMediaPhasePendingProcess
			}
			key := []any{operation.DestinationKey, operation.ProcessingKey, operation.OperationID, phase}
			where := `WHERE destination_key = ? AND processing_key = ? AND pending_operation_id = ? AND phase = ?`
			switch {
			case result.ErrorCode != "" && result.Retry:
				res, err = q.Exec(`
					UPDATE beeper_media_deliveries
					SET next_action_at = ?, error_code = ?, updated_at = `+s.dialect.Now()+`
					`+where, append([]any{retryAt, result.ErrorCode}, key...)...)
			case result.Terminal && operation.Kind == BeeperMediaOperationProcess:
				res, err = q.Exec(`
					UPDATE beeper_media_deliveries
					SET processing_operation_id = pending_operation_id, job_id = ?, phase = 'done',
					    operation_state = ?, coverage_state = ?, pending_operation_id = NULL,
					    frozen_request_json = NULL, next_action_at = NULL, error_code = ?,
					    updated_at = `+s.dialect.Now()+`
					`+where, append([]any{result.JobID, result.OperationState, result.CoverageState,
					result.ErrorCode}, key...)...)
			case result.ErrorCode != "":
				res, err = q.Exec(`
					UPDATE beeper_media_deliveries
					SET phase = 'blocked', next_action_at = NULL, error_code = ?, updated_at = `+s.dialect.Now()+`
					`+where, append([]any{result.ErrorCode}, key...)...)
			case operation.Kind == BeeperMediaOperationArtifact:
				res, err = q.Exec(`
					UPDATE beeper_media_deliveries
					SET supplied_input_id = ?, phase = 'pending-process', pending_operation_id = ?,
					    frozen_request_json = NULL, next_action_at = ?, error_code = '',
					    updated_at = `+s.dialect.Now()+`
					`+where, append([]any{result.SuppliedInputID, newBeeperMediaOperationID(),
					s.timestampValue(now)}, key...)...)
			default:
				res, err = q.Exec(`
					UPDATE beeper_media_deliveries
					SET processing_operation_id = pending_operation_id, job_id = ?, phase = 'observing',
					    operation_state = ?, coverage_state = ?, pending_operation_id = NULL,
					    frozen_request_json = NULL, next_action_at = ?, error_code = '',
					    updated_at = `+s.dialect.Now()+`
					`+where, append([]any{result.JobID, result.OperationState, result.CoverageState,
					s.timestampValue(now)}, key...)...)
			}

		case BeeperMediaOperationStatus:
			phase, next := beeperMediaPhaseObserving, s.timestampValue(now.Add(beeperMediaPollDelay))
			switch {
			case result.Retry:
				next = retryAt
			case result.Terminal:
				phase, next = beeperMediaPhaseDone, nil
			case result.ErrorCode != "":
				// Keep the job ID so a restart resumes observing after a rejected poll.
				phase, next = beeperMediaPhaseBlocked, nil
			}
			res, err = q.Exec(`
				UPDATE beeper_media_deliveries
				SET phase = ?, operation_state = COALESCE(NULLIF(?, ''), operation_state),
				    coverage_state = COALESCE(NULLIF(?, ''), coverage_state), error_code = ?,
				    next_action_at = ?, updated_at = `+s.dialect.Now()+`
				WHERE destination_key = ? AND processing_key = ? AND job_id = ? AND phase = 'observing'`,
				phase, result.OperationState, result.CoverageState, result.ErrorCode, next,
				operation.DestinationKey, operation.ProcessingKey, operation.JobID)
		}
		if err != nil {
			return fmt.Errorf("finish beeper media %s operation: %w", operation.Kind, err)
		}
		count, err := res.RowsAffected()
		applied = count == 1
		if err != nil || !applied || retireCode == "" {
			return err
		}
		return s.retireBeeperMediaDeliveries(q, operation.DestinationKey, operation.OccurrenceRef, retireCode)
	})
	return applied, err
}

// ReconsiderBlockedBeeperMediaOperations lets a daemon start retry remote
// steps that were blocked by configuration or service capability. Local
// source gaps stay blocked until the source changes.
func (s *Store) ReconsiderBlockedBeeperMediaOperations(ctx context.Context, destination string) error {
	if destination == "" {
		return errors.New("beeper media destination is required")
	}
	now := s.timestampValue(time.Now().UTC())
	localGaps := `NOT ` + beeperMediaLocalGapFilter()
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if _, err := q.Exec(`
			UPDATE beeper_media_occurrences
			SET retention_state = 'pending', next_action_at = ?, updated_at = `+s.dialect.Now()+`
			WHERE destination_key = ? AND retention_state = 'blocked' AND retention_operation_id <> ''
			  AND `+localGaps, append([]any{now, destination}, beeperMediaLocalGapCodes...)...); err != nil {
			return fmt.Errorf("reconsider blocked beeper media retention: %w", err)
		}
		if _, err := q.Exec(`
			UPDATE beeper_media_deliveries
			SET phase = `+beeperMediaResumedPhase+`,
			    next_action_at = ?, updated_at = `+s.dialect.Now()+`
			WHERE destination_key = ? AND phase = 'blocked' AND `+localGaps,
			append([]any{now, destination}, beeperMediaLocalGapCodes...)...); err != nil {
			return fmt.Errorf("reconsider blocked beeper media processing: %w", err)
		}
		return nil
	})
}

// LoadBeeperMediaScan loads the destination-namespaced rolling checkpoint.
func (s *Store) LoadBeeperMediaScan(ctx context.Context, destination string) (BeeperMediaScan, error) {
	if destination == "" {
		return BeeperMediaScan{}, errors.New("beeper media destination is required")
	}
	var encoded string
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key = ?`),
		beeperMediaScanKey(destination)).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return BeeperMediaScan{DestinationKey: destination}, nil
	}
	if err != nil {
		return BeeperMediaScan{}, fmt.Errorf("read beeper media scan: %w", err)
	}
	var scan BeeperMediaScan
	if err := json.Unmarshal([]byte(encoded), &scan); err != nil {
		return BeeperMediaScan{}, fmt.Errorf("decode beeper media scan: %w", err)
	}
	if err := validateBeeperMediaScan(destination, scan); err != nil {
		return BeeperMediaScan{}, err
	}
	return scan, nil
}

// AdvanceBeeperMediaScan compares and swaps the rolling scan checkpoint.
func (s *Store) AdvanceBeeperMediaScan(
	ctx context.Context, destination string, before, after BeeperMediaScan,
) (bool, error) {
	if err := errors.Join(validateBeeperMediaScan(destination, before),
		validateBeeperMediaScan(destination, after)); err != nil {
		return false, err
	}
	oldValue, err := json.Marshal(before)
	if err != nil {
		return false, err
	}
	newValue, err := json.Marshal(after)
	if err != nil {
		return false, err
	}
	var swapped bool
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if _, err := q.Exec(`
			INSERT INTO archive_metadata (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO NOTHING`, beeperMediaScanKey(destination), string(oldValue)); err != nil {
			return fmt.Errorf("initialize beeper media scan: %w", err)
		}
		result, err := q.Exec(`UPDATE archive_metadata SET value = ? WHERE key = ? AND value = ?`,
			string(newValue), beeperMediaScanKey(destination), string(oldValue))
		if err != nil {
			return fmt.Errorf("advance beeper media scan: %w", err)
		}
		count, err := result.RowsAffected()
		swapped = count == 1
		return err
	})
	return swapped, err
}

func validateBeeperMediaScan(destination string, scan BeeperMediaScan) error {
	if destination == "" || scan.DestinationKey != destination || scan.AfterAttachmentID < 0 ||
		scan.PassHighWater < 0 || scan.BaselineSequence < 0 {
		return errors.New("invalid beeper media scan checkpoint")
	}
	return nil
}

func validateBeeperMediaMapping(mapping BeeperMediaMapping) error {
	for _, field := range []struct{ name, value string }{
		{"destination", mapping.DestinationKey}, {"occurrence reference", mapping.OccurrenceRef},
		{"revision", mapping.Revision}, {"source identifier", mapping.SourceIdentifier},
		{"message", mapping.SourceMessageID}, {"part", mapping.SourcePartKey},
		{"occurrence JSON", mapping.OccurrenceJSON}, {"filename", mapping.Filename},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("beeper media %s is required", field.name)
		}
	}
	if mapping.SourceType != "beeper" {
		return errors.New("beeper media source type is invalid")
	}
	if mapping.ByteLength < 1 || !isBeeperMediaSHA256(mapping.SourceSHA256) {
		return errors.New("beeper media source identity is invalid")
	}
	if err := validateBeeperMediaJSON(mapping.OccurrenceJSON); err != nil {
		return err
	}
	switch mapping.RetentionState {
	case "", BeeperMediaRetentionPending, BeeperMediaRetentionBlocked:
	default:
		return errors.New("beeper media reconciliation state is invalid")
	}
	return nil
}

func validateBeeperMediaOperation(operation BeeperMediaOperation) error {
	if operation.DestinationKey == "" {
		return errors.New("beeper media operation destination is required")
	}
	switch operation.Kind {
	case BeeperMediaOperationRetain:
		if operation.OccurrenceRef == "" || operation.Revision == "" {
			return errors.New("beeper media retention operation identity is required")
		}
	case BeeperMediaOperationArtifact, BeeperMediaOperationProcess, BeeperMediaOperationStatus:
		if operation.ProcessingKey == "" {
			return errors.New("beeper media processing key is required")
		}
	default:
		return errors.New("beeper media operation kind is invalid")
	}
	if operation.OperationID != "" {
		id, err := uuid.Parse(operation.OperationID)
		if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 {
			return errors.New("beeper media operation ID must be UUIDv4")
		}
	}
	return nil
}

func beeperMediaOccurrenceColumns(alias string) string {
	columns := []string{"destination_key", "occurrence_ref", "revision", "source_type",
		"source_identifier", "source_conversation_id", "source_message_id", "source_attachment_id",
		"source_part_key", "source_row_id", "message_id", "attachment_id", "source_sha256",
		"byte_length", "raw_hash", "transcript_sha256", "language", "occurrence_json",
		"request_filename", "request_mime_type", "retention_operation_id", "retention_state",
		"next_action_at", "error_code", "vault_uid", "source_id", "source_version_id",
		"content_version_id", "occurrence_id", "coverage_state", "processing_key"}
	for i := range columns {
		columns[i] = alias + "." + columns[i]
	}
	return strings.Join(columns, ", ")
}

// beeperMediaNextActionIndex is next_action_at's position in the column list.
const beeperMediaNextActionIndex = 22

func beeperMediaOccurrenceScanTargets(m *BeeperMediaMapping) []any {
	return []any{&m.DestinationKey, &m.OccurrenceRef, &m.Revision, &m.SourceType,
		&m.SourceIdentifier, &m.SourceConversationID, &m.SourceMessageID, &m.SourceAttachmentID,
		&m.SourcePartKey, &m.LocalSourceID, &m.MessageID, &m.AttachmentID, &m.SourceSHA256,
		&m.ByteLength, &m.RawHash, &m.TranscriptSHA256, &m.Language, &m.OccurrenceJSON,
		&m.Filename, &m.MIMEType, &m.RetentionOperationID, &m.RetentionState,
		nil, &m.ErrorCode, &m.VaultUID, &m.DocbankSourceID, &m.SourceVersionID,
		&m.ContentVersionID, &m.DocbankOccurrenceID, &m.CoverageState, &m.ProcessingKey}
}

func (s *Store) readBeeperMediaOccurrence(
	q boundQuerier, destination, occurrenceRef, revision string,
) (BeeperMediaMapping, error) {
	var mapping BeeperMediaMapping
	var next sql.NullTime
	dest := beeperMediaOccurrenceScanTargets(&mapping)
	dest[beeperMediaNextActionIndex] = &next
	err := q.QueryRow(`SELECT `+beeperMediaOccurrenceColumns("o")+`
		FROM beeper_media_occurrences o
		WHERE o.destination_key = ? AND o.occurrence_ref = ? AND o.revision = ?`,
		destination, occurrenceRef, revision).Scan(dest...)
	mapping.NextActionAt = nullTimeValue(next)
	return mapping, err
}

func (s *Store) insertBeeperMediaOccurrence(q boundQuerier, m BeeperMediaMapping) error {
	_, err := q.Exec(`
		INSERT INTO beeper_media_occurrences
			(destination_key, occurrence_ref, revision, source_type, source_identifier,
			 source_conversation_id, source_message_id, source_attachment_id, source_part_key,
			 source_row_id, message_id, attachment_id, source_sha256, byte_length, raw_hash,
			 transcript_sha256, language, occurrence_json, request_filename, request_mime_type,
			 retention_operation_id, retention_state, next_action_at, error_code,
			 processing_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		        `+s.dialect.Now()+`, `+s.dialect.Now()+`)`,
		m.DestinationKey, m.OccurrenceRef, m.Revision, m.SourceType, m.SourceIdentifier,
		m.SourceConversationID, m.SourceMessageID, m.SourceAttachmentID, m.SourcePartKey,
		m.LocalSourceID, m.MessageID, m.AttachmentID, m.SourceSHA256, m.ByteLength, m.RawHash,
		m.TranscriptSHA256, m.Language, m.OccurrenceJSON, m.Filename, m.MIMEType,
		m.RetentionOperationID, m.RetentionState, s.timestampValue(m.NextActionAt), m.ErrorCode,
		m.ProcessingKey)
	if err != nil {
		return fmt.Errorf("insert beeper media occurrence: %w", err)
	}
	return nil
}

func (s *Store) updateBeeperMediaOccurrence(q boundQuerier, m BeeperMediaMapping) error {
	_, err := q.Exec(`
		UPDATE beeper_media_occurrences
		SET source_identifier = ?, source_conversation_id = ?, source_message_id = ?,
		    source_attachment_id = ?, source_part_key = ?, source_row_id = ?, message_id = ?,
		    attachment_id = ?, raw_hash = ?, retention_operation_id = ?, retention_state = ?,
		    next_action_at = ?, error_code = ?, updated_at = `+s.dialect.Now()+`
		WHERE destination_key = ? AND occurrence_ref = ? AND revision = ?`,
		m.SourceIdentifier, m.SourceConversationID, m.SourceMessageID, m.SourceAttachmentID,
		m.SourcePartKey, m.LocalSourceID, m.MessageID, m.AttachmentID, m.RawHash,
		m.RetentionOperationID, m.RetentionState, s.timestampValue(m.NextActionAt), m.ErrorCode,
		m.DestinationKey, m.OccurrenceRef, m.Revision)
	if err != nil {
		return fmt.Errorf("update beeper media occurrence: %w", err)
	}
	return nil
}

func (s *Store) timestampValue(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return s.dialect.TimestampParam(value)
}

func nullTimeValue(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time.UTC()
}

func beeperMediaScanKey(destination string) string {
	return "beeper_media_scan:" + destination
}

func newBeeperMediaOperationID() string {
	return uuid.New().String()
}

func isBeeperMediaSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateBeeperMediaJSON(value string) error {
	var raw map[string]any
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return errors.New("beeper media occurrence JSON is invalid")
	}
	return nil
}
