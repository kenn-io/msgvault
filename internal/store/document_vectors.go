package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	docembedding "go.kenn.io/docbank/document/embedding"
)

const maxDocumentVectorCandidateLimit = docembedding.MaxCandidateLimit

var documentVectorFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

var (
	ErrDocumentVectorClaimLost              = errors.New("document vector chunk claim lost")
	ErrDocumentVectorSourceChanged          = errors.New("document vector chunk source changed")
	ErrDocumentVectorInvalidGenerationState = errors.New("document vector generation state is invalid for this operation")
	ErrDocumentVectorCoverageIncomplete     = errors.New("document vector generation coverage is incomplete")
	ErrDocumentVectorGenerationBlocked      = errors.New("document vector generation is blocked by terminal failures")
	ErrDocumentVectorCleanupIncomplete      = errors.New("document vector generation backend cleanup is incomplete")
)

type DocumentVectorGenerationState string

const (
	DocumentVectorGenerationBuilding DocumentVectorGenerationState = "building"
	DocumentVectorGenerationActive   DocumentVectorGenerationState = "active"
	DocumentVectorGenerationRetired  DocumentVectorGenerationState = "retired"
)

type DocumentVectorGenerationSpec struct {
	Fingerprint               string `json:"fingerprint"`
	TargetExtractionProfileID string `json:"target_extraction_profile_id"`
	EmbeddingProfile          string `json:"embedding_profile"`
	Model                     string `json:"model"`
	Dimension                 int    `json:"dimension"`
}

type DocumentVectorGeneration struct {
	DocumentVectorGenerationSpec

	ID          int64                         `json:"id"`
	State       DocumentVectorGenerationState `json:"state"`
	CreatedAt   time.Time                     `json:"created_at"`
	ActivatedAt *time.Time                    `json:"activated_at,omitempty"`
	RetiredAt   *time.Time                    `json:"retired_at,omitempty"`
}

type DocumentVectorChunkCandidate struct {
	GenerationID, ChunkID                                int64
	ExtractionID, ExtractionProfileID, CanonicalBlobHash string
	ExtractionInputKey, ChunkKey, ChunkChecksum, Text    string
	ChunkOrdinal                                         int
	SourceSequence                                       int64
}

type DocumentVectorChunkClaim struct {
	DocumentVectorChunkCandidate

	Token        string
	LeaseOwner   string
	LeaseFence   int64
	LeaseUntil   time.Time
	AttemptCount int
}

// DocumentVectorCoverage compares the current live corpus with ready publications.
type DocumentVectorCoverage struct {
	Required int64 `json:"required"`
	Ready    int64 `json:"ready"`
}

// Complete reports whether every currently served chunk has a ready publication.
func (c DocumentVectorCoverage) Complete() bool { return c.Required == c.Ready }

// DocumentVectorLivePublication is a ready token resolved to its current chunk snapshot.
type DocumentVectorLivePublication struct {
	DocumentVectorChunkCandidate

	Token string
}

// DocumentVectorCleanupToken identifies one opaque backend row to delete.
type DocumentVectorCleanupToken struct {
	GenerationID int64
	Token        string
}

// DocumentVectorCleanupPage is one durable cleanup-parking page. Exhausted
// pages atomically reset the restorable generation/token cursor pair.
type DocumentVectorCleanupPage struct {
	Tokens            []DocumentVectorCleanupToken
	AfterGenerationID int64
	AfterToken        string
	Exhausted         bool
}

// DocumentVectorFailureDiagnostic is the bounded, non-PII failure surface.
type DocumentVectorFailureDiagnostic struct {
	Token        string     `json:"token"`
	AttemptCount int        `json:"attempt_count"`
	NextRetryAt  *time.Time `json:"next_retry_at,omitempty"`
	Terminal     bool       `json:"terminal"`
	ErrorCode    string     `json:"error_code"`
}

// DocumentVectorGenerationStatus counts mutually exclusive current
// publication states. Obsolete counts retired or noncurrent snapshots;
// CleanupPending is the uncleaned subset of Obsolete. Failures may include
// obsolete rows so operators can inspect durable failure history.
type DocumentVectorGenerationStatus struct {
	GenerationID             int64                             `json:"generation_id"`
	State                    DocumentVectorGenerationState     `json:"state"`
	Blocked                  bool                              `json:"blocked"`
	Pending                  int64                             `json:"pending"`
	Retryable                int64                             `json:"retryable"`
	Terminal                 int64                             `json:"terminal"`
	ReadyLive                int64                             `json:"ready_live"`
	Obsolete                 int64                             `json:"stale_obsolete"`
	CleanupPending           int64                             `json:"cleanup_pending"`
	Failures                 []DocumentVectorFailureDiagnostic `json:"failures"`
	FailureAfterGenerationID int64                             `json:"failure_after_generation_id,omitempty"`
	FailureAfterToken        string                            `json:"failure_after_token,omitempty"`
	FailuresExhausted        bool                              `json:"failures_exhausted"`
}

// DocumentVectorFailureResetResult reports one bounded stable-token scan.
type DocumentVectorFailureResetResult struct {
	Scanned           int    `json:"scanned"`
	Reset             int    `json:"reset"`
	AfterGenerationID int64  `json:"after_generation_id,omitempty"`
	AfterToken        string `json:"after_token,omitempty"`
	Exhausted         bool   `json:"exhausted"`
}

func (s *Store) EnsureDocumentVectorGeneration(ctx context.Context, spec DocumentVectorGenerationSpec) (DocumentVectorGeneration, bool, error) {
	if err := validateDocumentVectorGenerationSpec(spec); err != nil {
		return DocumentVectorGeneration{}, false, err
	}
	var result DocumentVectorGeneration
	created := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		// Serialize target rotation before consulting or creating a generation.
		// This follows the same index-state -> generation lock order as claim and
		// activation, and keeps the target check in the creation transaction.
		if _, err := q.Exec(`UPDATE document_index_state SET revision = revision WHERE singleton = 1`); err != nil {
			return fmt.Errorf("lock document vector index state: %w", err)
		}
		var currentTarget sql.NullString
		if err := q.QueryRow(`SELECT target_profile_id FROM document_index_state WHERE singleton = 1`).Scan(&currentTarget); err != nil {
			return fmt.Errorf("read document vector target profile: %w", err)
		}
		if !currentTarget.Valid || currentTarget.String != spec.TargetExtractionProfileID {
			return ErrDocumentVectorInvalidGenerationState
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT id, fingerprint, target_extraction_profile_id, embedding_profile, model, dimension,
			       state, created_at, activated_at, retired_at
			FROM document_vector_generations WHERE fingerprint = ? ORDER BY id DESC`, spec.Fingerprint)
		if err != nil {
			return fmt.Errorf("find document vector generation fingerprint %q: %w", spec.Fingerprint, err)
		}
		defer func() { _ = rows.Close() }()
		var matching *DocumentVectorGeneration
		for rows.Next() {
			existing, _, scanErr := scanDocumentVectorGeneration(rows)
			if scanErr != nil {
				return scanErr
			}
			if existing.DocumentVectorGenerationSpec != spec {
				return fmt.Errorf("document vector generation fingerprint %q collides with a different immutable specification", spec.Fingerprint)
			}
			if matching == nil && existing.State != DocumentVectorGenerationRetired {
				matching = &existing
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate document vector generations: %w", err)
		}
		if matching != nil {
			result = *matching
			return nil
		}
		var building int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_vector_generations WHERE state = ?`, string(DocumentVectorGenerationBuilding)).Scan(&building); err != nil {
			return fmt.Errorf("check building document vector generation: %w", err)
		}
		if building != 0 {
			return errors.New("a document vector generation is already building")
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO document_vector_generations
			(fingerprint, target_extraction_profile_id, embedding_profile, model, dimension, state)
			VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, spec.Fingerprint, spec.TargetExtractionProfileID, spec.EmbeddingProfile, spec.Model, spec.Dimension, string(DocumentVectorGenerationBuilding)).Scan(&id); err != nil {
			return fmt.Errorf("create document vector generation: %w", err)
		}
		generation, found, err := scanDocumentVectorGeneration(tx.QueryRowContext(ctx, `
			SELECT id, fingerprint, target_extraction_profile_id, embedding_profile, model, dimension,
			       state, created_at, activated_at, retired_at
			FROM document_vector_generations WHERE id = ?`, id))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("created document vector generation %d was not found", id)
		}
		result, created = generation, true
		return nil
	})
	return result, created, err
}

func validateDocumentVectorGenerationSpec(spec DocumentVectorGenerationSpec) error {
	if !documentVectorFingerprintPattern.MatchString(spec.Fingerprint) || strings.TrimSpace(spec.TargetExtractionProfileID) == "" || strings.TrimSpace(spec.Model) == "" {
		return errors.New("document vector generation immutable fields are required")
	}
	if spec.EmbeddingProfile != "vector.embeddings" {
		return errors.New("document vector generation embedding profile must be vector.embeddings")
	}
	if spec.Dimension <= 0 {
		return errors.New("document vector generation dimension must be positive")
	}
	return nil
}

func (s *Store) GetDocumentVectorGeneration(ctx context.Context, id int64) (DocumentVectorGeneration, error) {
	if id <= 0 {
		return DocumentVectorGeneration{}, errors.New("document vector generation id must be positive")
	}
	g, found, err := scanDocumentVectorGeneration(s.db.QueryRowContext(ctx, s.Rebind(`SELECT id, fingerprint, target_extraction_profile_id, embedding_profile, model, dimension, state, created_at, activated_at, retired_at FROM document_vector_generations WHERE id = ?`), id))
	if err != nil {
		return DocumentVectorGeneration{}, err
	}
	if !found {
		return DocumentVectorGeneration{}, fmt.Errorf("document vector generation %d not found", id)
	}
	return g, nil
}

func (s *Store) GetBuildingDocumentVectorGeneration(ctx context.Context) (*DocumentVectorGeneration, error) {
	return s.getDocumentVectorGenerationByState(ctx, DocumentVectorGenerationBuilding)
}
func (s *Store) GetActiveDocumentVectorGeneration(ctx context.Context) (*DocumentVectorGeneration, error) {
	return s.getDocumentVectorGenerationByState(ctx, DocumentVectorGenerationActive)
}
func (s *Store) getDocumentVectorGenerationByState(ctx context.Context, state DocumentVectorGenerationState) (*DocumentVectorGeneration, error) {
	g, found, err := scanDocumentVectorGeneration(s.db.QueryRowContext(ctx, s.Rebind(`SELECT id, fingerprint, target_extraction_profile_id, embedding_profile, model, dimension, state, created_at, activated_at, retired_at FROM document_vector_generations WHERE state = ?`), string(state)))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil //nolint:nilnil // Absence is a valid optional state lookup result.
	}
	return &g, nil
}

func (s *Store) ListDocumentVectorChunkCandidates(ctx context.Context, generationID, afterChunkID int64, limit int) ([]DocumentVectorChunkCandidate, error) {
	if generationID <= 0 || afterChunkID < 0 {
		return nil, errors.New("document vector candidate generation and after chunk id have invalid bounds")
	}
	if limit < 1 || limit > maxDocumentVectorCandidateLimit {
		return nil, fmt.Errorf("document vector candidate limit must be between 1 and %d", maxDocumentVectorCandidateLimit)
	}
	query := `SELECT dc.id, h.extraction_id, h.profile_id, h.canonical_blob_hash, h.extraction_input_key,
		dc.chunk_key, dc.checksum, dc.ordinal, dc.text, e.source_sequence
		FROM (SELECT id, target_extraction_profile_id AS target_profile_id, state
		        FROM document_vector_generations WHERE id = ?) g
		JOIN document_index_state ds ON ds.singleton = 1
		JOIN document_extraction_heads h ON h.profile_id = g.target_profile_id
		JOIN document_extractions e ON e.id = h.extraction_id
		JOIN document_extraction_profiles p ON p.id = h.profile_id
		JOIN document_provider_consents c ON c.profile_id = p.id
		JOIN document_chunks dc ON dc.extraction_id = h.extraction_id
		WHERE g.state <> ? AND ds.target_profile_id = g.target_profile_id AND dc.id > ?
		  AND ` + documentVectorLiveAuthoritySQL() + `
		ORDER BY dc.id LIMIT ?`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), generationID, string(DocumentVectorGenerationRetired), afterChunkID, limit)
	if err != nil {
		return nil, fmt.Errorf("list document vector candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DocumentVectorChunkCandidate
	for rows.Next() {
		var c DocumentVectorChunkCandidate
		c.GenerationID = generationID
		if err := rows.Scan(&c.ChunkID, &c.ExtractionID, &c.ExtractionProfileID, &c.CanonicalBlobHash, &c.ExtractionInputKey, &c.ChunkKey, &c.ChunkChecksum, &c.ChunkOrdinal, &c.Text, &c.SourceSequence); err != nil {
			return nil, fmt.Errorf("scan document vector candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate document vector candidates: %w", err)
	}
	return out, nil
}

func (s *Store) ClaimDocumentVectorChunk(
	ctx context.Context,
	generationID, afterChunkID int64,
	scanLimit int,
	owner string,
	now time.Time,
	leaseDuration time.Duration,
) (*DocumentVectorChunkClaim, error) {
	if err := validateDocumentVectorClaimRequest(generationID, afterChunkID, scanLimit, owner, now, leaseDuration); err != nil {
		return nil, err
	}
	leaseUntil := normalizeDocumentVectorTime(now.Add(leaseDuration))
	now = normalizeDocumentVectorTime(now)
	if !leaseUntil.After(now) {
		return nil, errors.New("document vector lease duration has no effective database precision")
	}
	var claimed *DocumentVectorChunkClaim
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, currentTarget, err := s.lockDocumentVectorGeneration(q, generationID)
		if err != nil {
			return err
		}
		if state != DocumentVectorGenerationBuilding || !currentTarget {
			return ErrDocumentVectorInvalidGenerationState
		}
		candidates, err := s.listDocumentVectorChunkCandidatesTx(ctx, tx, generationID, afterChunkID, scanLimit)
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			token := documentVectorToken(candidate)
			result, err := q.Exec(`
				INSERT INTO document_vector_publications
					(generation_id, extraction_id, extraction_profile_id, canonical_blob_hash,
					 extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence,
					 token, state, lease_owner, lease_fence, lease_until, attempt_count,
					 created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, 1, ?, 1, ?, ?)
				ON CONFLICT (generation_id, extraction_id, chunk_id) DO NOTHING`,
				candidate.GenerationID, candidate.ExtractionID, candidate.ExtractionProfileID,
				candidate.CanonicalBlobHash, candidate.ExtractionInputKey, candidate.ChunkID,
				candidate.ChunkKey, candidate.ChunkChecksum, candidate.SourceSequence, token,
				owner, s.dialect.TimestampParam(leaseUntil), s.dialect.TimestampParam(now),
				s.dialect.TimestampParam(now))
			if err != nil {
				return fmt.Errorf("create document vector chunk claim: %w", err)
			}
			inserted, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("read document vector chunk claim result: %w", err)
			}
			if inserted == 1 {
				claimed = documentVectorChunkClaim(candidate, token, owner, 1, leaseUntil, 1)
				return nil
			}

			publication, found, err := s.getDocumentVectorPublicationForClaim(q, candidate)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			if publication.token != token {
				return errors.New("document vector publication token does not match its immutable identity")
			}
			switch publication.state {
			case "ready":
				continue
			case "pending":
				if publication.leaseUntil.Valid && publication.leaseUntil.Time.After(now) {
					if publication.leaseOwner.String == owner {
						claimed = documentVectorChunkClaim(candidate, token, owner, publication.leaseFence, publication.leaseUntil.Time, publication.attemptCount)
						return nil
					}
					continue
				}
			case "failed":
				if !publication.nextRetryAt.Valid || publication.nextRetryAt.Time.After(now) {
					continue
				}
			default:
				return fmt.Errorf("document vector publication %q has invalid state %q", token, publication.state)
			}

			result, err = q.Exec(`
				UPDATE document_vector_publications
				SET state = 'pending', lease_owner = ?, lease_fence = lease_fence + 1,
				    lease_until = ?, attempt_count = attempt_count + 1,
				    next_retry_at = NULL, error_code = NULL, updated_at = ?
				WHERE generation_id = ? AND extraction_id = ? AND chunk_id = ?
				  AND lease_fence = ? AND (
				      (state = 'pending' AND (lease_until IS NULL OR lease_until <= ?))
				      OR (state = 'failed' AND next_retry_at IS NOT NULL AND next_retry_at <= ?)
				  )`, owner, s.dialect.TimestampParam(leaseUntil), s.dialect.TimestampParam(now),
				generationID, candidate.ExtractionID, candidate.ChunkID, publication.leaseFence,
				s.dialect.TimestampParam(now), s.dialect.TimestampParam(now))
			if err != nil {
				return fmt.Errorf("take over document vector chunk claim: %w", err)
			}
			updated, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("read document vector chunk takeover result: %w", err)
			}
			if updated == 1 {
				claimed = documentVectorChunkClaim(candidate, token, owner, publication.leaseFence+1, leaseUntil, publication.attemptCount+1)
				return nil
			}
		}
		return nil
	})
	return claimed, err
}

func (s *Store) RenewDocumentVectorChunkClaim(
	ctx context.Context,
	generationID int64,
	token, owner string,
	fence int64,
	now time.Time,
	leaseDuration time.Duration,
) (time.Time, error) {
	if err := validateDocumentVectorClaimReference(generationID, token, owner, fence, now); err != nil {
		return time.Time{}, err
	}
	if leaseDuration <= 0 {
		return time.Time{}, errors.New("document vector lease duration must be positive")
	}
	leaseUntil := normalizeDocumentVectorTime(now.Add(leaseDuration))
	now = normalizeDocumentVectorTime(now)
	if !leaseUntil.After(now) {
		return time.Time{}, errors.New("document vector lease duration has no effective database precision")
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, currentTarget, err := s.lockDocumentVectorGeneration(q, generationID)
		if err != nil {
			return err
		}
		if state != DocumentVectorGenerationBuilding || !currentTarget {
			return ErrDocumentVectorInvalidGenerationState
		}
		result, err := q.Exec(`
			UPDATE document_vector_publications SET lease_until = ?, updated_at = ?
			WHERE generation_id = ? AND token = ? AND state = 'pending'
			  AND lease_owner = ? AND lease_fence = ? AND lease_until > ?`,
			s.dialect.TimestampParam(leaseUntil), s.dialect.TimestampParam(now), generationID,
			token, owner, fence, s.dialect.TimestampParam(now))
		if err != nil {
			return fmt.Errorf("renew document vector chunk claim: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document vector chunk renewal result: %w", err)
		}
		if updated != 1 {
			return ErrDocumentVectorClaimLost
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return leaseUntil, nil
}

func (s *Store) CommitDocumentVectorPublication(
	ctx context.Context,
	generationID int64,
	token, owner string,
	fence int64,
	now time.Time,
) error {
	if err := validateDocumentVectorClaimReference(generationID, token, owner, fence, now); err != nil {
		return err
	}
	now = normalizeDocumentVectorTime(now)
	sourceChanged := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, currentTarget, err := s.lockDocumentVectorGeneration(q, generationID)
		if err != nil {
			return err
		}
		publication, found, err := s.getDocumentVectorPublicationByToken(q, generationID, token)
		if err != nil {
			return err
		}
		if !found || publication.leaseOwner.String != owner || publication.leaseFence != fence {
			return ErrDocumentVectorClaimLost
		}
		if publication.state == "ready" {
			return nil
		}
		if publication.state != "pending" || !publication.leaseUntil.Valid || !publication.leaseUntil.Time.After(now) {
			return ErrDocumentVectorClaimLost
		}
		if state != DocumentVectorGenerationBuilding || !currentTarget {
			return ErrDocumentVectorInvalidGenerationState
		}
		current, err := s.isDocumentVectorPublicationCurrent(q, publication)
		if err != nil {
			return err
		}
		if !current {
			result, err := q.Exec(`
				UPDATE document_vector_publications
				SET state = 'failed', lease_owner = NULL, lease_until = NULL,
				    next_retry_at = NULL, error_code = 'source_changed',
				    backend_cleaned_at = NULL, updated_at = ?
				WHERE generation_id = ? AND token = ? AND state = 'pending'
				  AND lease_owner = ? AND lease_fence = ?`,
				s.dialect.TimestampParam(now), generationID, token, owner, fence)
			if err != nil {
				return fmt.Errorf("record changed document vector source: %w", err)
			}
			updated, err := result.RowsAffected()
			if err != nil || updated != 1 {
				return ErrDocumentVectorClaimLost
			}
			sourceChanged = true
			return nil
		}
		result, err := q.Exec(`
			UPDATE document_vector_publications
			SET state = 'ready', lease_until = NULL, next_retry_at = NULL,
			    error_code = NULL, updated_at = ?
			WHERE generation_id = ? AND token = ? AND state = 'pending'
			  AND lease_owner = ? AND lease_fence = ?`,
			s.dialect.TimestampParam(now), generationID, token, owner, fence)
		if err != nil {
			return fmt.Errorf("commit document vector publication: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil || updated != 1 {
			return ErrDocumentVectorClaimLost
		}
		return nil
	})
	if err != nil {
		return err
	}
	if sourceChanged {
		return ErrDocumentVectorSourceChanged
	}
	return nil
}

func (s *Store) FailDocumentVectorChunk(
	ctx context.Context,
	generationID int64,
	token, owner string,
	fence int64,
	now time.Time,
	nextRetryAt *time.Time,
	terminal bool,
	errorCode string,
) error {
	if err := validateDocumentVectorClaimReference(generationID, token, owner, fence, now); err != nil {
		return err
	}
	now = normalizeDocumentVectorTime(now)
	var normalizedRetryAt *time.Time
	if nextRetryAt != nil {
		retryAt := normalizeDocumentVectorTime(*nextRetryAt)
		normalizedRetryAt = &retryAt
	}
	if err := validateDocumentVectorFailure(now, normalizedRetryAt, terminal, errorCode); err != nil {
		return err
	}
	var retryAt any
	if normalizedRetryAt != nil {
		retryAt = s.dialect.TimestampParam(*normalizedRetryAt)
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, currentTarget, err := s.lockDocumentVectorGeneration(q, generationID)
		if err != nil {
			return err
		}
		if state != DocumentVectorGenerationBuilding || !currentTarget {
			return ErrDocumentVectorInvalidGenerationState
		}
		result, err := q.Exec(`
			UPDATE document_vector_publications
			SET state = 'failed', lease_owner = NULL, lease_until = NULL,
			    next_retry_at = ?, error_code = ?, updated_at = ?
			WHERE generation_id = ? AND token = ? AND state = 'pending'
			  AND lease_owner = ? AND lease_fence = ? AND lease_until > ?`,
			retryAt, errorCode, s.dialect.TimestampParam(now), generationID, token,
			owner, fence, s.dialect.TimestampParam(now))
		if err != nil {
			return fmt.Errorf("record document vector chunk failure: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document vector chunk failure result: %w", err)
		}
		if updated != 1 {
			return ErrDocumentVectorClaimLost
		}
		return nil
	})
}

// GetDocumentVectorGenerationStatus returns exact ledger counts plus one
// bounded page of stable-token failure diagnostics.
func (s *Store) GetDocumentVectorGenerationStatus(
	ctx context.Context, generationID int64, afterToken string, limit int,
) (DocumentVectorGenerationStatus, error) {
	if generationID <= 0 {
		return DocumentVectorGenerationStatus{}, errors.New("document vector generation id must be positive")
	}
	if afterToken != "" && !documentVectorFingerprintPattern.MatchString(afterToken) {
		return DocumentVectorGenerationStatus{}, errors.New("document vector failure cursor must be a lowercase SHA-256 value")
	}
	if limit < 1 || limit > maxDocumentVectorCandidateLimit {
		return DocumentVectorGenerationStatus{}, fmt.Errorf("document vector failure limit must be between 1 and %d", maxDocumentVectorCandidateLimit)
	}
	status := DocumentVectorGenerationStatus{GenerationID: generationID}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		current := documentVectorPublicationSnapshotCurrentSQL("v", "g")
		parked := `(v.state = 'failed' AND v.error_code = 'source_changed')`
		query := `SELECT g.state,
			COALESCE(SUM(CASE WHEN v.token IS NOT NULL AND g.state <> 'retired' AND NOT (` + parked + `) AND ` + current + ` AND v.state = 'pending' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN v.token IS NOT NULL AND g.state <> 'retired' AND NOT (` + parked + `) AND ` + current + ` AND v.state = 'failed' AND v.next_retry_at IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN v.token IS NOT NULL AND g.state <> 'retired' AND NOT (` + parked + `) AND ` + current + ` AND v.state = 'failed' AND v.next_retry_at IS NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN v.token IS NOT NULL AND g.state <> 'retired' AND NOT (` + parked + `) AND ` + current + ` AND v.state = 'ready' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN v.token IS NOT NULL AND (g.state = 'retired' OR ` + parked + ` OR NOT (` + current + `)) THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN v.token IS NOT NULL AND v.backend_cleaned_at IS NULL AND (g.state = 'retired' OR ` + parked + ` OR NOT (` + current + `)) THEN 1 ELSE 0 END), 0)
			FROM document_vector_generations g
			LEFT JOIN document_vector_publications v ON v.generation_id = g.id
			WHERE g.id = ? GROUP BY g.state`
		if err := tx.QueryRowContext(ctx, query, generationID).Scan(
			&status.State, &status.Pending, &status.Retryable, &status.Terminal,
			&status.ReadyLive, &status.Obsolete, &status.CleanupPending,
		); errors.Is(err, sql.ErrNoRows) {
			return ErrDocumentVectorInvalidGenerationState
		} else if err != nil {
			return fmt.Errorf("count document vector generation status: %w", err)
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT token, attempt_count, next_retry_at, error_code
			FROM document_vector_publications
			WHERE generation_id = ? AND state = 'failed' AND token > ?
			ORDER BY token LIMIT ?`, generationID, afterToken, limit)
		if err != nil {
			return fmt.Errorf("list document vector failure diagnostics: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var diagnostic DocumentVectorFailureDiagnostic
			var retryAt sql.NullTime
			var errorCode sql.NullString
			if err := rows.Scan(&diagnostic.Token, &diagnostic.AttemptCount, &retryAt, &errorCode); err != nil {
				return fmt.Errorf("scan document vector failure diagnostic: %w", err)
			}
			if retryAt.Valid {
				normalized := normalizeDocumentVectorTime(retryAt.Time)
				diagnostic.NextRetryAt = &normalized
			}
			diagnostic.Terminal = !retryAt.Valid
			diagnostic.ErrorCode = boundedDocumentVectorErrorCode(errorCode.String)
			status.Failures = append(status.Failures, diagnostic)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate document vector failure diagnostics: %w", err)
		}
		if len(status.Failures) < limit {
			status.FailuresExhausted = true
			status.FailureAfterGenerationID = 0
			status.FailureAfterToken = ""
		} else {
			status.FailureAfterGenerationID = generationID
			status.FailureAfterToken = status.Failures[len(status.Failures)-1].Token
		}
		status.Blocked = status.State == DocumentVectorGenerationBuilding && status.Terminal > 0
		return nil
	})
	return status, err
}

// ResetDocumentVectorFailures makes one bounded page of current failed
// snapshots claimable again. Source-changed and noncurrent rows remain parked.
func (s *Store) ResetDocumentVectorFailures(
	ctx context.Context, generationID int64, afterToken string, limit int, now time.Time,
) (DocumentVectorFailureResetResult, error) {
	if generationID <= 0 || now.IsZero() {
		return DocumentVectorFailureResetResult{}, errors.New("document vector failure reset requires a generation and time")
	}
	if afterToken != "" && !documentVectorFingerprintPattern.MatchString(afterToken) {
		return DocumentVectorFailureResetResult{}, errors.New("document vector failure reset cursor must be a lowercase SHA-256 value")
	}
	if limit < 1 || limit > maxDocumentVectorCandidateLimit {
		return DocumentVectorFailureResetResult{}, fmt.Errorf("document vector failure reset limit must be between 1 and %d", maxDocumentVectorCandidateLimit)
	}
	now = normalizeDocumentVectorTime(now)
	var reset DocumentVectorFailureResetResult
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, currentTarget, err := s.lockDocumentVectorGeneration(q, generationID)
		if err != nil {
			return err
		}
		if state != DocumentVectorGenerationBuilding || !currentTarget {
			return ErrDocumentVectorInvalidGenerationState
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT token, error_code FROM document_vector_publications
			WHERE generation_id = ? AND state = 'failed' AND token > ?
			ORDER BY token LIMIT ?`, generationID, afterToken, limit)
		if err != nil {
			return fmt.Errorf("list document vector failures to reset: %w", err)
		}
		type failure struct{ token, errorCode string }
		var failures []failure
		for rows.Next() {
			var item failure
			if err := rows.Scan(&item.token, &item.errorCode); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan document vector failure to reset: %w", err)
			}
			failures = append(failures, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate document vector failures to reset: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close document vector failures to reset: %w", err)
		}
		reset.Scanned = len(failures)
		for _, item := range failures {
			if item.errorCode == "source_changed" {
				continue
			}
			publication, found, err := s.getDocumentVectorPublicationByToken(q, generationID, item.token)
			if err != nil {
				return err
			}
			if !found || publication.state != "failed" {
				continue
			}
			current, err := s.isDocumentVectorPublicationCurrent(q, publication)
			if err != nil {
				return err
			}
			if !current {
				continue
			}
			result, err := q.Exec(`
				UPDATE document_vector_publications
				SET state = 'pending', lease_owner = NULL, lease_until = NULL,
				    attempt_count = 0, next_retry_at = NULL, error_code = NULL,
				    backend_cleaned_at = NULL, updated_at = ?
				WHERE generation_id = ? AND token = ? AND state = 'failed'`,
				s.dialect.TimestampParam(now), generationID, item.token)
			if err != nil {
				return fmt.Errorf("reset document vector failure: %w", err)
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("read document vector failure reset result: %w", err)
			}
			reset.Reset += int(changed)
		}
		if len(failures) < limit {
			reset.Exhausted = true
			reset.AfterGenerationID = 0
			reset.AfterToken = ""
		} else {
			reset.AfterGenerationID = generationID
			reset.AfterToken = failures[len(failures)-1].token
		}
		return nil
	})
	return reset, err
}

// GetDocumentVectorCoverage counts the current live corpus and its exact ready publications.
func (s *Store) GetDocumentVectorCoverage(ctx context.Context, generationID int64) (DocumentVectorCoverage, error) {
	if generationID <= 0 {
		return DocumentVectorCoverage{}, errors.New("document vector generation id must be positive")
	}
	var coverage DocumentVectorCoverage
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, found, err := getDocumentVectorGenerationState(q, generationID)
		if err != nil {
			return err
		}
		if !found || state == DocumentVectorGenerationRetired {
			return ErrDocumentVectorInvalidGenerationState
		}
		coverage, err = s.getDocumentVectorCoverage(q, generationID)
		return err
	})
	return coverage, err
}

// ActivateDocumentVectorGeneration atomically swaps in one completely built generation.
func (s *Store) ActivateDocumentVectorGeneration(ctx context.Context, generationID int64, now time.Time) error {
	if generationID <= 0 || now.IsZero() {
		return errors.New("document vector activation requires a generation and time")
	}
	now = normalizeDocumentVectorTime(now)
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, currentTarget, err := s.lockDocumentVectorGeneration(q, generationID)
		if err != nil {
			return err
		}
		if state != DocumentVectorGenerationBuilding || !currentTarget {
			return ErrDocumentVectorInvalidGenerationState
		}
		coverage, err := s.getDocumentVectorCoverage(q, generationID)
		if err != nil {
			return err
		}
		if !coverage.Complete() {
			return ErrDocumentVectorCoverageIncomplete
		}
		if _, err := q.Exec(`
			UPDATE document_vector_generations
			SET state = 'retired', retired_at = ?
			WHERE state = 'active' AND id <> ?`,
			s.dialect.TimestampParam(now), generationID); err != nil {
			return fmt.Errorf("retire prior active document vector generation: %w", err)
		}
		result, err := q.Exec(`
			UPDATE document_vector_generations
			SET state = 'active', activated_at = ?, retired_at = NULL
			WHERE id = ? AND state = 'building'`,
			s.dialect.TimestampParam(now), generationID)
		if err != nil {
			return fmt.Errorf("activate document vector generation: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document vector activation result: %w", err)
		}
		if updated != 1 {
			return ErrDocumentVectorInvalidGenerationState
		}
		return bumpDocumentIndexRevision(q)
	})
}

// RetireDocumentVectorGeneration removes a building or active generation from service.
func (s *Store) RetireDocumentVectorGeneration(ctx context.Context, generationID int64, now time.Time) (bool, error) {
	if generationID <= 0 || now.IsZero() {
		return false, errors.New("document vector retirement requires a generation and time")
	}
	now = normalizeDocumentVectorTime(now)
	changed := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, _, found, err := s.lockDocumentVectorGenerationIfExists(q, generationID)
		if err != nil {
			return err
		}
		if !found || state == DocumentVectorGenerationRetired {
			return nil
		}
		if state != DocumentVectorGenerationActive && state != DocumentVectorGenerationBuilding {
			return ErrDocumentVectorInvalidGenerationState
		}
		result, err := q.Exec(`
			UPDATE document_vector_generations
			SET state = 'retired', retired_at = ?
			WHERE id = ? AND state = ?`, s.dialect.TimestampParam(now), generationID, string(state))
		if err != nil {
			return fmt.Errorf("retire document vector generation: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document vector retirement result: %w", err)
		}
		if updated != 1 {
			return ErrDocumentVectorInvalidGenerationState
		}
		changed = true
		if state == DocumentVectorGenerationActive {
			return bumpDocumentIndexRevision(q)
		}
		return nil
	})
	return changed, err
}

// ResolveLiveDocumentVectorPublications filters tokens through current archive authority.
func (s *Store) ResolveLiveDocumentVectorPublications(
	ctx context.Context, generationID int64, tokens []string,
) ([]DocumentVectorLivePublication, error) {
	if generationID <= 0 {
		return nil, errors.New("document vector generation id must be positive")
	}
	if len(tokens) > maxDocumentVectorCandidateLimit {
		return nil, fmt.Errorf("document vector token limit must not exceed %d", maxDocumentVectorCandidateLimit)
	}
	if len(tokens) == 0 {
		return []DocumentVectorLivePublication{}, nil
	}
	unique := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if !documentVectorFingerprintPattern.MatchString(token) {
			return nil, errors.New("document vector token must be a lowercase SHA-256 value")
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		unique = append(unique, token)
	}
	args := make([]any, 0, len(unique)+2)
	args = append(args, generationID, string(DocumentVectorGenerationActive))
	for _, token := range unique {
		args = append(args, token)
	}
	query := `SELECT v.token, dc.id, v.extraction_id, v.extraction_profile_id,
		v.canonical_blob_hash, v.extraction_input_key, v.chunk_key, v.chunk_checksum,
		dc.ordinal, dc.text, v.source_sequence
		FROM document_vector_publications v
		JOIN document_vector_generations g ON g.id = v.generation_id
		JOIN document_index_state ds ON ds.singleton = 1
		JOIN document_extraction_heads h
		  ON h.extraction_id = v.extraction_id
		 AND h.profile_id = v.extraction_profile_id
		 AND h.canonical_blob_hash = v.canonical_blob_hash
		 AND h.extraction_input_key = v.extraction_input_key
		JOIN document_extractions e ON e.id = h.extraction_id
		JOIN document_extraction_profiles p ON p.id = h.profile_id
		JOIN document_provider_consents c ON c.profile_id = p.id
		JOIN document_chunks dc
		  ON dc.id = v.chunk_id AND dc.extraction_id = v.extraction_id
		 AND dc.chunk_key = v.chunk_key AND dc.checksum = v.chunk_checksum
		WHERE g.id = ? AND g.state = ?
		  AND ds.target_profile_id = g.target_extraction_profile_id
		  AND v.state = 'ready' AND e.source_sequence = v.source_sequence
		  AND h.source_sequence = v.source_sequence
		  AND v.token IN (` + documentPlaceholders(len(unique)) + `)
		  AND ` + documentVectorLiveAuthoritySQL()
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("resolve live document vector publications: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byToken := make(map[string]DocumentVectorLivePublication, len(unique))
	for rows.Next() {
		var publication DocumentVectorLivePublication
		publication.GenerationID = generationID
		if err := rows.Scan(&publication.Token, &publication.ChunkID, &publication.ExtractionID,
			&publication.ExtractionProfileID, &publication.CanonicalBlobHash,
			&publication.ExtractionInputKey, &publication.ChunkKey, &publication.ChunkChecksum,
			&publication.ChunkOrdinal, &publication.Text, &publication.SourceSequence); err != nil {
			return nil, fmt.Errorf("scan live document vector publication: %w", err)
		}
		byToken[publication.Token] = publication
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate live document vector publications: %w", err)
	}
	result := make([]DocumentVectorLivePublication, 0, len(byToken))
	for _, token := range unique {
		if publication, found := byToken[token]; found {
			result = append(result, publication)
		}
	}
	return result, nil
}

// ListDocumentVectorCleanupTokens pages uncleaned backend tokens from a retired generation.
func (s *Store) ListDocumentVectorCleanupTokens(
	ctx context.Context, generationID int64, afterToken string, limit int,
) ([]DocumentVectorCleanupToken, error) {
	if generationID <= 0 {
		return nil, errors.New("document vector generation id must be positive")
	}
	if afterToken != "" && !documentVectorFingerprintPattern.MatchString(afterToken) {
		return nil, errors.New("document vector cleanup cursor must be a lowercase SHA-256 value")
	}
	if limit < 1 || limit > maxDocumentVectorCandidateLimit {
		return nil, fmt.Errorf("document vector cleanup limit must be between 1 and %d", maxDocumentVectorCandidateLimit)
	}
	q := boundQuerier{ctx: ctx, q: s.db}
	state, found, err := getDocumentVectorGenerationState(q, generationID)
	if err != nil {
		return nil, err
	}
	if !found || state != DocumentVectorGenerationRetired {
		return nil, ErrDocumentVectorInvalidGenerationState
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT generation_id, token
		FROM document_vector_publications
		WHERE generation_id = ? AND backend_cleaned_at IS NULL AND token > ?
		ORDER BY token LIMIT ?`), generationID, afterToken, limit)
	if err != nil {
		return nil, fmt.Errorf("list document vector cleanup tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []DocumentVectorCleanupToken
	for rows.Next() {
		var token DocumentVectorCleanupToken
		if err := rows.Scan(&token.GenerationID, &token.Token); err != nil {
			return nil, fmt.Errorf("scan document vector cleanup token: %w", err)
		}
		result = append(result, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate document vector cleanup tokens: %w", err)
	}
	return result, nil
}

// ParkObsoleteDocumentVectorTokens durably parks nonretired obsolete rows
// before their backend vectors are deleted. Already parked rows remain in the
// page even if source authority later returns, making crash replay safe.
func (s *Store) ParkObsoleteDocumentVectorTokens(
	ctx context.Context, generationID int64, afterToken string, limit int, now time.Time,
) (DocumentVectorCleanupPage, error) {
	if generationID <= 0 || now.IsZero() {
		return DocumentVectorCleanupPage{}, errors.New("document vector cleanup parking requires a generation and time")
	}
	if afterToken != "" && !documentVectorFingerprintPattern.MatchString(afterToken) {
		return DocumentVectorCleanupPage{}, errors.New("document vector cleanup parking cursor must be a lowercase SHA-256 value")
	}
	if limit < 1 || limit > maxDocumentVectorCandidateLimit {
		return DocumentVectorCleanupPage{}, fmt.Errorf("document vector cleanup parking limit must be between 1 and %d", maxDocumentVectorCandidateLimit)
	}
	now = normalizeDocumentVectorTime(now)
	var page DocumentVectorCleanupPage
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, _, found, err := s.lockDocumentVectorGenerationIfExists(q, generationID)
		if err != nil {
			return err
		}
		if !found {
			return ErrDocumentVectorInvalidGenerationState
		}
		current := documentVectorPublicationSnapshotCurrentSQL("v", "g")
		query := `SELECT v.generation_id, v.token
			FROM document_vector_publications v
			JOIN document_vector_generations g ON g.id = v.generation_id
			WHERE v.generation_id = ? AND v.backend_cleaned_at IS NULL AND v.token > ?
			  AND (v.state <> 'pending' OR v.lease_until IS NULL OR v.lease_until <= ?)
			  AND (g.state = 'retired'
			       OR (v.state = 'failed' AND v.error_code = 'source_changed')
			       OR NOT (` + current + `))
			ORDER BY v.token LIMIT ?`
		rows, err := tx.QueryContext(ctx, query, generationID, afterToken, s.dialect.TimestampParam(now), limit)
		if err != nil {
			return fmt.Errorf("list document vector tokens to park: %w", err)
		}
		for rows.Next() {
			var token DocumentVectorCleanupToken
			if err := rows.Scan(&token.GenerationID, &token.Token); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan document vector token to park: %w", err)
			}
			page.Tokens = append(page.Tokens, token)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate document vector tokens to park: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close document vector tokens to park: %w", err)
		}
		if state != DocumentVectorGenerationRetired {
			for _, token := range page.Tokens {
				if _, err := q.Exec(`
					UPDATE document_vector_publications
					SET state = 'failed', lease_owner = NULL, lease_until = NULL,
					    next_retry_at = NULL, error_code = 'source_changed',
					    backend_cleaned_at = NULL, updated_at = ?
					WHERE generation_id = ? AND token = ?
					  AND NOT (state = 'failed' AND error_code = 'source_changed')`,
					s.dialect.TimestampParam(now), generationID, token.Token); err != nil {
					return fmt.Errorf("park obsolete document vector token: %w", err)
				}
			}
		}
		if len(page.Tokens) < limit {
			page.Exhausted = true
		} else {
			page.AfterGenerationID = generationID
			page.AfterToken = page.Tokens[len(page.Tokens)-1].Token
		}
		return nil
	})
	return page, err
}

// FinalizeObsoleteDocumentVectorToken acknowledges one backend deletion.
// Nonretired parked rows are removed so a current candidate can be rebuilt;
// retired rows retain their cleanup marker until generation purge.
func (s *Store) FinalizeObsoleteDocumentVectorToken(
	ctx context.Context, generationID int64, token string, now time.Time,
) (bool, error) {
	if generationID <= 0 || !documentVectorFingerprintPattern.MatchString(token) || now.IsZero() {
		return false, errors.New("document vector cleanup finalization is invalid")
	}
	now = normalizeDocumentVectorTime(now)
	changed := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, _, found, err := s.lockDocumentVectorGenerationIfExists(q, generationID)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		var result sql.Result
		if state == DocumentVectorGenerationRetired {
			result, err = q.Exec(`
				UPDATE document_vector_publications SET backend_cleaned_at = ?, updated_at = ?
				WHERE generation_id = ? AND token = ? AND backend_cleaned_at IS NULL`,
				s.dialect.TimestampParam(now), s.dialect.TimestampParam(now), generationID, token)
		} else {
			result, err = q.Exec(`
				DELETE FROM document_vector_publications
				WHERE generation_id = ? AND token = ?
				  AND state = 'failed' AND error_code = 'source_changed'`, generationID, token)
		}
		if err != nil {
			return fmt.Errorf("finalize obsolete document vector token: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read obsolete document vector finalization result: %w", err)
		}
		changed = updated == 1
		return nil
	})
	return changed, err
}

// MarkDocumentVectorTokenCleaned records one successful backend deletion idempotently.
func (s *Store) MarkDocumentVectorTokenCleaned(
	ctx context.Context, generationID int64, token string, now time.Time,
) (bool, error) {
	if generationID <= 0 || !documentVectorFingerprintPattern.MatchString(token) || now.IsZero() {
		return false, errors.New("document vector cleanup marker is invalid")
	}
	now = normalizeDocumentVectorTime(now)
	changed := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, _, found, err := s.lockDocumentVectorGenerationIfExists(q, generationID)
		if err != nil {
			return err
		}
		if !found || state != DocumentVectorGenerationRetired {
			return ErrDocumentVectorInvalidGenerationState
		}
		result, err := q.Exec(`
			UPDATE document_vector_publications SET backend_cleaned_at = ?, updated_at = ?
			WHERE generation_id = ? AND token = ? AND backend_cleaned_at IS NULL`,
			s.dialect.TimestampParam(now), s.dialect.TimestampParam(now), generationID, token)
		if err != nil {
			return fmt.Errorf("mark document vector token cleaned: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document vector cleanup marker result: %w", err)
		}
		changed = updated == 1
		return nil
	})
	return changed, err
}

// PurgeRetiredDocumentVectorGeneration removes a retired ledger after backend cleanup.
func (s *Store) PurgeRetiredDocumentVectorGeneration(ctx context.Context, generationID int64) (bool, error) {
	if generationID <= 0 {
		return false, errors.New("document vector generation id must be positive")
	}
	purged := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, _, found, err := s.lockDocumentVectorGenerationIfExists(q, generationID)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if state != DocumentVectorGenerationRetired {
			return ErrDocumentVectorInvalidGenerationState
		}
		var uncleaned int64
		if err := q.QueryRow(`
			SELECT COUNT(*) FROM document_vector_publications
			WHERE generation_id = ? AND backend_cleaned_at IS NULL`, generationID).Scan(&uncleaned); err != nil {
			return fmt.Errorf("count uncleaned document vector tokens: %w", err)
		}
		if uncleaned != 0 {
			return ErrDocumentVectorCleanupIncomplete
		}
		if _, err := q.Exec(`DELETE FROM document_vector_publications WHERE generation_id = ?`, generationID); err != nil {
			return fmt.Errorf("delete cleaned document vector publications: %w", err)
		}
		result, err := q.Exec(`DELETE FROM document_vector_generations WHERE id = ? AND state = 'retired'`, generationID)
		if err != nil {
			return fmt.Errorf("purge retired document vector generation: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document vector generation purge result: %w", err)
		}
		if deleted != 1 {
			return ErrDocumentVectorInvalidGenerationState
		}
		purged = true
		return nil
	})
	return purged, err
}

type documentVectorPublication struct {
	DocumentVectorChunkCandidate

	token        string
	state        string
	leaseOwner   sql.NullString
	leaseFence   int64
	leaseUntil   sql.NullTime
	attemptCount int
	nextRetryAt  sql.NullTime
}

func (s *Store) listDocumentVectorChunkCandidatesTx(
	ctx context.Context, tx *loggedTx, generationID, afterChunkID int64, limit int,
) ([]DocumentVectorChunkCandidate, error) {
	query := `SELECT dc.id, h.extraction_id, h.profile_id, h.canonical_blob_hash, h.extraction_input_key,
		dc.chunk_key, dc.checksum, dc.ordinal, dc.text, e.source_sequence
		FROM document_vector_generations g
		JOIN document_index_state ds ON ds.singleton = 1
		JOIN document_extraction_heads h ON h.profile_id = g.target_extraction_profile_id
		JOIN document_extractions e ON e.id = h.extraction_id
		JOIN document_extraction_profiles p ON p.id = h.profile_id
		JOIN document_provider_consents c ON c.profile_id = p.id
		JOIN document_chunks dc ON dc.extraction_id = h.extraction_id
		WHERE g.id = ? AND g.state = ? AND ds.target_profile_id = g.target_extraction_profile_id
		  AND dc.id > ? AND ` + documentVectorLiveAuthoritySQL() + `
		ORDER BY dc.id LIMIT ?`
	rows, err := tx.QueryContext(ctx, query, generationID, string(DocumentVectorGenerationBuilding), afterChunkID, limit)
	if err != nil {
		return nil, fmt.Errorf("list claimable document vector candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []DocumentVectorChunkCandidate
	for rows.Next() {
		var candidate DocumentVectorChunkCandidate
		candidate.GenerationID = generationID
		if err := rows.Scan(&candidate.ChunkID, &candidate.ExtractionID, &candidate.ExtractionProfileID,
			&candidate.CanonicalBlobHash, &candidate.ExtractionInputKey, &candidate.ChunkKey,
			&candidate.ChunkChecksum, &candidate.ChunkOrdinal, &candidate.Text, &candidate.SourceSequence); err != nil {
			return nil, fmt.Errorf("scan claimable document vector candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimable document vector candidates: %w", err)
	}
	return candidates, nil
}

func (s *Store) getDocumentVectorCoverage(q boundQuerier, generationID int64) (DocumentVectorCoverage, error) {
	query := `SELECT COUNT(*), COALESCE(SUM(CASE WHEN EXISTS (
			SELECT 1 FROM document_vector_publications v
			WHERE v.generation_id = g.id AND v.state = 'ready'
			  AND v.extraction_id = h.extraction_id
			  AND v.extraction_profile_id = h.profile_id
			  AND v.canonical_blob_hash = h.canonical_blob_hash
			  AND v.extraction_input_key = h.extraction_input_key
			  AND v.chunk_id = dc.id AND v.chunk_key = dc.chunk_key
			  AND v.chunk_checksum = dc.checksum
			  AND v.source_sequence = e.source_sequence
			  AND v.source_sequence = h.source_sequence
		) THEN 1 ELSE 0 END), 0)
		FROM document_vector_generations g
		JOIN document_index_state ds ON ds.singleton = 1
		JOIN document_extraction_heads h ON h.profile_id = g.target_extraction_profile_id
		JOIN document_extractions e ON e.id = h.extraction_id
		JOIN document_extraction_profiles p ON p.id = h.profile_id
		JOIN document_provider_consents c ON c.profile_id = p.id
		JOIN document_chunks dc ON dc.extraction_id = h.extraction_id
		WHERE g.id = ? AND ds.target_profile_id = g.target_extraction_profile_id
		  AND ` + documentVectorLiveAuthoritySQL()
	var coverage DocumentVectorCoverage
	if err := q.QueryRow(query, generationID).Scan(&coverage.Required, &coverage.Ready); err != nil {
		return DocumentVectorCoverage{}, fmt.Errorf("count document vector coverage: %w", err)
	}
	return coverage, nil
}

func getDocumentVectorGenerationState(
	q boundQuerier, generationID int64,
) (DocumentVectorGenerationState, bool, error) {
	var state DocumentVectorGenerationState
	err := q.QueryRow(`SELECT state FROM document_vector_generations WHERE id = ?`, generationID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read document vector generation state: %w", err)
	}
	return state, true, nil
}

func (s *Store) lockDocumentVectorGeneration(
	q boundQuerier, generationID int64,
) (DocumentVectorGenerationState, bool, error) {
	state, currentTarget, found, err := s.lockDocumentVectorGenerationIfExists(q, generationID)
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, ErrDocumentVectorInvalidGenerationState
	}
	return state, currentTarget, nil
}

func (s *Store) lockDocumentVectorGenerationIfExists(
	q boundQuerier, generationID int64,
) (DocumentVectorGenerationState, bool, bool, error) {
	// The index-state row is the serialization point for source/profile changes.
	// Taking it before the generation/publication row gives claim, commit, and
	// the later activation path one lock order on both database backends.
	if _, err := q.Exec(`UPDATE document_index_state SET revision = revision WHERE singleton = 1`); err != nil {
		return "", false, false, fmt.Errorf("lock document vector index state: %w", err)
	}
	result, err := q.Exec(`UPDATE document_vector_generations SET state = state WHERE id = ?`, generationID)
	if err != nil {
		return "", false, false, fmt.Errorf("lock document vector generation: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return "", false, false, fmt.Errorf("read document vector generation lock result: %w", err)
	}
	if updated != 1 {
		return "", false, false, nil
	}
	var state DocumentVectorGenerationState
	var currentTarget bool
	err = q.QueryRow(`
		SELECT g.state, COALESCE(ds.target_profile_id = g.target_extraction_profile_id, FALSE)
		FROM document_vector_generations g
		JOIN document_index_state ds ON ds.singleton = 1
		WHERE g.id = ?`, generationID).Scan(&state, &currentTarget)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, fmt.Errorf("check document vector generation state: %w", err)
	}
	return state, currentTarget, true, nil
}

func (s *Store) getDocumentVectorPublicationForClaim(
	q boundQuerier, candidate DocumentVectorChunkCandidate,
) (documentVectorPublication, bool, error) {
	query := `SELECT token, state, lease_owner, lease_fence, lease_until, attempt_count, next_retry_at
		FROM document_vector_publications
		WHERE generation_id = ? AND extraction_id = ? AND chunk_id = ?`
	if s.dialect.DriverName() == postgresDriverName {
		query += ` FOR UPDATE`
	}
	var publication documentVectorPublication
	publication.DocumentVectorChunkCandidate = candidate
	err := q.QueryRow(query, candidate.GenerationID, candidate.ExtractionID, candidate.ChunkID).Scan(
		&publication.token, &publication.state, &publication.leaseOwner, &publication.leaseFence,
		&publication.leaseUntil, &publication.attemptCount, &publication.nextRetryAt)
	if errors.Is(err, sql.ErrNoRows) {
		return documentVectorPublication{}, false, nil
	}
	if err != nil {
		return documentVectorPublication{}, false, fmt.Errorf("read document vector publication claim: %w", err)
	}
	return publication, true, nil
}

func (s *Store) getDocumentVectorPublicationByToken(
	q boundQuerier, generationID int64, token string,
) (documentVectorPublication, bool, error) {
	query := `SELECT extraction_id, extraction_profile_id, canonical_blob_hash, extraction_input_key,
		chunk_id, chunk_key, chunk_checksum, source_sequence, token, state, lease_owner,
		lease_fence, lease_until, attempt_count, next_retry_at
		FROM document_vector_publications WHERE generation_id = ? AND token = ?`
	if s.dialect.DriverName() == postgresDriverName {
		query += ` FOR UPDATE`
	}
	var publication documentVectorPublication
	publication.GenerationID = generationID
	err := q.QueryRow(query, generationID, token).Scan(
		&publication.ExtractionID, &publication.ExtractionProfileID, &publication.CanonicalBlobHash,
		&publication.ExtractionInputKey, &publication.ChunkID, &publication.ChunkKey,
		&publication.ChunkChecksum, &publication.SourceSequence, &publication.token,
		&publication.state, &publication.leaseOwner, &publication.leaseFence,
		&publication.leaseUntil, &publication.attemptCount, &publication.nextRetryAt)
	if errors.Is(err, sql.ErrNoRows) {
		return documentVectorPublication{}, false, nil
	}
	if err != nil {
		return documentVectorPublication{}, false, fmt.Errorf("read document vector publication: %w", err)
	}
	return publication, true, nil
}

func (s *Store) isDocumentVectorPublicationCurrent(q boundQuerier, publication documentVectorPublication) (bool, error) {
	query := `SELECT EXISTS (
		SELECT 1
		FROM document_vector_generations g
		JOIN document_index_state ds ON ds.singleton = 1
		JOIN document_extraction_heads h ON h.extraction_id = ?
		JOIN document_extractions e ON e.id = h.extraction_id
		JOIN document_extraction_profiles p ON p.id = h.profile_id
		JOIN document_provider_consents c ON c.profile_id = p.id
		JOIN document_chunks dc ON dc.extraction_id = h.extraction_id
		WHERE g.id = ? AND ds.target_profile_id = g.target_extraction_profile_id
		  AND h.profile_id = ? AND h.canonical_blob_hash = ? AND h.extraction_input_key = ?
		  AND e.source_sequence = ? AND dc.id = ? AND dc.chunk_key = ? AND dc.checksum = ?
		  AND ` + documentVectorLiveAuthoritySQL() + `
	)`
	var current bool
	err := q.QueryRow(query, publication.ExtractionID, publication.GenerationID,
		publication.ExtractionProfileID,
		publication.CanonicalBlobHash, publication.ExtractionInputKey, publication.SourceSequence,
		publication.ChunkID, publication.ChunkKey, publication.ChunkChecksum).Scan(&current)
	if err != nil {
		return false, fmt.Errorf("recheck document vector publication source: %w", err)
	}
	return current, nil
}

func documentVectorPublicationSnapshotCurrentSQL(publication, generation string) string {
	return `EXISTS (
		SELECT 1
		FROM document_index_state ds
		JOIN document_extraction_heads h ON h.extraction_id = ` + publication + `.extraction_id
		JOIN document_extractions e ON e.id = h.extraction_id
		JOIN document_extraction_profiles p ON p.id = h.profile_id
		JOIN document_provider_consents c ON c.profile_id = p.id
		JOIN document_chunks dc ON dc.id = ` + publication + `.chunk_id AND dc.extraction_id = h.extraction_id
		WHERE ds.singleton = 1
		  AND ds.target_profile_id = ` + generation + `.target_extraction_profile_id
		  AND h.profile_id = ` + publication + `.extraction_profile_id
		  AND h.canonical_blob_hash = ` + publication + `.canonical_blob_hash
		  AND h.extraction_input_key = ` + publication + `.extraction_input_key
		  AND e.source_sequence = ` + publication + `.source_sequence
		  AND h.source_sequence = ` + publication + `.source_sequence
		  AND dc.chunk_key = ` + publication + `.chunk_key
		  AND dc.checksum = ` + publication + `.chunk_checksum
		  AND ` + documentVectorLiveAuthoritySQL() + `
	)`
}

func documentVectorLiveAuthoritySQL() string {
	const head = "h"

	return `EXISTS (
		SELECT 1 FROM document_occurrences o
		JOIN attachments a ON a.id = o.attachment_id
		JOIN messages m ON m.id = o.message_id
		WHERE o.canonical_blob_hash = ` + head + `.canonical_blob_hash
		  AND ` + documentSearchValidity() + `
	)`
}

func boundedDocumentVectorErrorCode(value string) string {
	if len(value) == 0 || len(value) > 64 {
		return "unknown"
	}
	for _, character := range value {
		if character != '_' && character != '-' && (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') {
			return "unknown"
		}
	}
	return value
}

func documentVectorToken(candidate DocumentVectorChunkCandidate) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte("msgvault-document-vector-token-v1"))
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(candidate.GenerationID)) //nolint:gosec // Generation IDs are positive database keys.
	_, _ = hasher.Write(size[:])
	for _, field := range []string{candidate.ExtractionID, candidate.ChunkKey, candidate.ChunkChecksum} {
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = hasher.Write(size[:])
		_, _ = hasher.Write([]byte(field))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func documentVectorChunkClaim(
	candidate DocumentVectorChunkCandidate,
	token, owner string,
	fence int64,
	leaseUntil time.Time,
	attemptCount int,
) *DocumentVectorChunkClaim {
	return &DocumentVectorChunkClaim{
		DocumentVectorChunkCandidate: candidate,
		Token:                        token, LeaseOwner: owner, LeaseFence: fence,
		LeaseUntil: normalizeDocumentVectorTime(leaseUntil), AttemptCount: attemptCount,
	}
}

func normalizeDocumentVectorTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Millisecond)
}

func validateDocumentVectorClaimRequest(
	generationID, afterChunkID int64,
	scanLimit int,
	owner string,
	now time.Time,
	leaseDuration time.Duration,
) error {
	if generationID <= 0 || afterChunkID < 0 {
		return errors.New("document vector claim generation and scan position have invalid bounds")
	}
	if scanLimit < 1 || scanLimit > maxDocumentVectorCandidateLimit {
		return fmt.Errorf("document vector claim scan limit must be between 1 and %d", maxDocumentVectorCandidateLimit)
	}
	if strings.TrimSpace(owner) == "" || now.IsZero() || leaseDuration <= 0 {
		return errors.New("document vector claim owner, time, and lease duration are required")
	}
	return nil
}

func validateDocumentVectorClaimReference(generationID int64, token, owner string, fence int64, now time.Time) error {
	if generationID <= 0 || !documentVectorFingerprintPattern.MatchString(token) ||
		strings.TrimSpace(owner) == "" || fence <= 0 || now.IsZero() {
		return errors.New("document vector claim reference is invalid")
	}
	return nil
}

func validateDocumentVectorFailure(now time.Time, nextRetryAt *time.Time, terminal bool, errorCode string) error {
	if len(errorCode) == 0 || len(errorCode) > 64 {
		return errors.New("document vector failure error code is invalid")
	}
	for _, character := range errorCode {
		if character != '_' && character != '-' && (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') {
			return errors.New("document vector failure error code is invalid")
		}
	}
	if terminal {
		if nextRetryAt != nil {
			return errors.New("terminal document vector failure cannot retry")
		}
		return nil
	}
	if nextRetryAt == nil || !nextRetryAt.After(now) {
		return errors.New("retryable document vector failure requires a future retry time")
	}
	return nil
}

func scanDocumentVectorGeneration(row scanner) (DocumentVectorGeneration, bool, error) {
	var g DocumentVectorGeneration
	var activated, retired sql.NullTime
	err := row.Scan(&g.ID, &g.Fingerprint, &g.TargetExtractionProfileID, &g.EmbeddingProfile, &g.Model, &g.Dimension, &g.State, &g.CreatedAt, &activated, &retired)
	if errors.Is(err, sql.ErrNoRows) {
		return DocumentVectorGeneration{}, false, nil
	}
	if err != nil {
		return DocumentVectorGeneration{}, false, fmt.Errorf("scan document vector generation: %w", err)
	}
	g.CreatedAt = normalizeDocumentVectorTime(g.CreatedAt)
	if activated.Valid {
		normalized := normalizeDocumentVectorTime(activated.Time)
		g.ActivatedAt = &normalized
	}
	if retired.Valid {
		normalized := normalizeDocumentVectorTime(retired.Time)
		g.RetiredAt = &normalized
	}
	return g, true, nil
}
