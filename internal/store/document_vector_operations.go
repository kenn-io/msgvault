package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DocumentVectorConsentSpec binds hosted-processing consent to both the
// reusable corpus policy and the exact canonical egress destination.
type DocumentVectorConsentSpec struct {
	DocumentVectorGenerationSpec

	EgressFingerprint string `json:"egress_fingerprint"`
}

// DocumentVectorConsent records operator consent for one exact egress policy.
// It never contains credentials or raw provider endpoint data.
type DocumentVectorConsent struct {
	DocumentVectorConsentSpec

	ConsentedAt time.Time `json:"consented_at"`
}

// DocumentVectorUsageDelta is locally observed provider work. It intentionally
// excludes token usage because the provider contract does not report it.
type DocumentVectorUsageDelta struct {
	ProviderCalls      int64 `json:"provider_calls"`
	ProviderDocuments  int64 `json:"provider_documents"`
	ProviderChunks     int64 `json:"provider_chunks"`
	ProviderInputChars int64 `json:"provider_input_chars"`
}

// DocumentVectorProviderUsage is cumulative observed provider work for an
// immutable generation fingerprint.
type DocumentVectorProviderUsage struct {
	DocumentVectorUsageDelta

	Fingerprint string    `json:"fingerprint"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
}

// DocumentVectorOperationsStatus is the bounded operator view. When no
// generation is requested, failures deterministically come from the building
// generation first, then the active generation.
type DocumentVectorOperationsStatus struct {
	ConfiguredSpec              DocumentVectorGenerationSpec    `json:"configured_spec"`
	ConfiguredEgressFingerprint string                          `json:"configured_egress_fingerprint"`
	Consent                     *DocumentVectorConsent          `json:"consent,omitempty"`
	Usage                       DocumentVectorProviderUsage     `json:"usage"`
	Active                      *DocumentVectorGeneration       `json:"active,omitempty"`
	Building                    *DocumentVectorGeneration       `json:"building,omitempty"`
	Selected                    *DocumentVectorGenerationStatus `json:"selected,omitempty"`
	Coverage                    *DocumentVectorCoverage         `json:"coverage,omitempty"`
}

func (s *Store) ensureDocumentVectorRebuildSchema(ctx context.Context) error {
	return s.runOnceMigration(ctx, migrationDocumentVectorRebuildIndex, false, func(ctx context.Context) error {
		return s.withTxContext(ctx, func(tx *loggedTx) error {
			if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_document_vector_generations_live_fingerprint`); err != nil {
				return fmt.Errorf("drop unique document vector fingerprint index: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_document_vector_generations_live_fingerprint ON document_vector_generations(fingerprint) WHERE state <> 'retired'`); err != nil {
				return fmt.Errorf("create document vector fingerprint lookup index: %w", err)
			}
			return nil
		})
	})
}

// GetDocumentVectorTargetProfileID returns the configured extraction target
// without resolving any provider credentials.
func (s *Store) GetDocumentVectorTargetProfileID(ctx context.Context) (string, error) {
	var target sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT target_profile_id FROM document_index_state WHERE singleton = 1`).Scan(&target); err != nil {
		return "", fmt.Errorf("read document vector target profile: %w", err)
	}
	if !target.Valid || target.String == "" {
		return "", ErrDocumentVectorInvalidGenerationState
	}
	return target.String, nil
}

func (s *Store) RecordDocumentVectorConsent(ctx context.Context, spec DocumentVectorConsentSpec, now time.Time) (DocumentVectorConsent, bool, error) {
	if err := validateDocumentVectorGenerationSpec(spec.DocumentVectorGenerationSpec); err != nil {
		return DocumentVectorConsent{}, false, err
	}
	if !documentVectorFingerprintPattern.MatchString(spec.EgressFingerprint) {
		return DocumentVectorConsent{}, false, errors.New("document vector consent egress fingerprint is invalid")
	}
	now = normalizeDocumentVectorTime(now)
	if now.IsZero() {
		return DocumentVectorConsent{}, false, errors.New("document vector consent time is required")
	}
	var consent DocumentVectorConsent
	var created bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, s.Rebind(`
			INSERT INTO document_vector_consents
				(egress_fingerprint, generation_fingerprint, target_extraction_profile_id, embedding_profile, model, dimension, consented_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (egress_fingerprint) DO NOTHING`),
			spec.EgressFingerprint, spec.Fingerprint, spec.TargetExtractionProfileID, spec.EmbeddingProfile, spec.Model,
			spec.Dimension, s.dialect.TimestampParam(now))
		if err != nil {
			return fmt.Errorf("record document vector consent: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document vector consent result: %w", err)
		}
		created = rows == 1
		return tx.QueryRowContext(ctx, s.Rebind(`
			SELECT egress_fingerprint, generation_fingerprint, target_extraction_profile_id, embedding_profile, model, dimension, consented_at
			FROM document_vector_consents WHERE egress_fingerprint = ?`), spec.EgressFingerprint).Scan(
			&consent.EgressFingerprint, &consent.Fingerprint, &consent.TargetExtractionProfileID, &consent.EmbeddingProfile,
			&consent.Model, &consent.Dimension, &consent.ConsentedAt)
	})
	if err != nil {
		return DocumentVectorConsent{}, false, err
	}
	consent.ConsentedAt = normalizeDocumentVectorTime(consent.ConsentedAt)
	if consent.DocumentVectorConsentSpec != spec {
		return DocumentVectorConsent{}, false, fmt.Errorf("document vector consent egress fingerprint %q collides with a different immutable specification", spec.EgressFingerprint)
	}
	return consent, created, nil
}

func (s *Store) GetDocumentVectorConsent(ctx context.Context, egressFingerprint string) (*DocumentVectorConsent, error) {
	if !documentVectorFingerprintPattern.MatchString(egressFingerprint) {
		return nil, errors.New("document vector consent egress fingerprint is invalid")
	}
	var consent DocumentVectorConsent
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT egress_fingerprint, generation_fingerprint, target_extraction_profile_id, embedding_profile, model, dimension, consented_at
		FROM document_vector_consents WHERE egress_fingerprint = ?`), egressFingerprint).Scan(
		&consent.EgressFingerprint, &consent.Fingerprint, &consent.TargetExtractionProfileID, &consent.EmbeddingProfile,
		&consent.Model, &consent.Dimension, &consent.ConsentedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // Absence is a valid optional consent lookup result.
	}
	if err != nil {
		return nil, fmt.Errorf("read document vector consent: %w", err)
	}
	consent.ConsentedAt = normalizeDocumentVectorTime(consent.ConsentedAt)
	return &consent, nil
}

// CheckpointDocumentVectorBuild atomically advances the durable scan cursor
// and accumulates the observed provider work returned by one completed worker
// invocation. It is not provider billing or an exactly-once crash counter.
func (s *Store) CheckpointDocumentVectorBuild(ctx context.Context, generationID, afterChunkID int64, exhausted bool, delta DocumentVectorUsageDelta, now time.Time) error {
	return s.checkpointDocumentVectorBuild(ctx, generationID, "", afterChunkID, exhausted, delta, now)
}

// CheckpointDocumentVectorBuildForFingerprint preserves observed usage even
// if a concurrently retired generation was purged after Worker.Run returned.
// The cursor remains generation-scoped and is never written after lifecycle loss.
func (s *Store) CheckpointDocumentVectorBuildForFingerprint(ctx context.Context, generationID int64, fingerprint string, afterChunkID int64, exhausted bool, delta DocumentVectorUsageDelta, now time.Time) error {
	if !documentVectorFingerprintPattern.MatchString(fingerprint) {
		return errors.New("document vector build checkpoint fingerprint is invalid")
	}
	return s.checkpointDocumentVectorBuild(ctx, generationID, fingerprint, afterChunkID, exhausted, delta, now)
}

func (s *Store) checkpointDocumentVectorBuild(ctx context.Context, generationID int64, expectedFingerprint string, afterChunkID int64, exhausted bool, delta DocumentVectorUsageDelta, now time.Time) error {
	if generationID <= 0 || afterChunkID < 0 || exhausted != (afterChunkID == 0) {
		return errors.New("document vector build checkpoint is invalid")
	}
	if delta.ProviderCalls < 0 || delta.ProviderDocuments < 0 || delta.ProviderChunks < 0 || delta.ProviderInputChars < 0 {
		return errors.New("document vector provider usage must be nonnegative")
	}
	now = normalizeDocumentVectorTime(now)
	if now.IsZero() {
		return errors.New("document vector build checkpoint time is required")
	}
	var lifecycleErr error
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, currentTarget, found, err := s.lockDocumentVectorGenerationIfExists(q, generationID)
		if err != nil {
			return err
		}
		fingerprint := expectedFingerprint
		if found {
			if err := q.QueryRow(`SELECT fingerprint FROM document_vector_generations WHERE id = ?`, generationID).Scan(&fingerprint); err != nil {
				return fmt.Errorf("read document vector build fingerprint: %w", err)
			}
			if expectedFingerprint != "" && fingerprint != expectedFingerprint {
				return errors.New("document vector build checkpoint fingerprint does not match generation")
			}
		} else if fingerprint == "" {
			return ErrDocumentVectorInvalidGenerationState
		}
		if _, err := q.Exec(`
			INSERT INTO document_vector_provider_usage
				(fingerprint, provider_calls, provider_documents, provider_chunks, provider_input_chars, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (fingerprint) DO UPDATE SET
				provider_calls = document_vector_provider_usage.provider_calls + excluded.provider_calls,
				provider_documents = document_vector_provider_usage.provider_documents + excluded.provider_documents,
				provider_chunks = document_vector_provider_usage.provider_chunks + excluded.provider_chunks,
				provider_input_chars = document_vector_provider_usage.provider_input_chars + excluded.provider_input_chars,
				updated_at = excluded.updated_at`, fingerprint, delta.ProviderCalls, delta.ProviderDocuments,
			delta.ProviderChunks, delta.ProviderInputChars, s.dialect.TimestampParam(now)); err != nil {
			return fmt.Errorf("checkpoint document vector provider usage: %w", err)
		}
		if !found || state != DocumentVectorGenerationBuilding || !currentTarget {
			// The provider work already happened. Commit its observed usage even
			// when activation, retirement, or target rotation won the lifecycle
			// race; only the resumable cursor remains building/current scoped.
			lifecycleErr = ErrDocumentVectorInvalidGenerationState
			return nil
		}
		if exhausted {
			if _, err := q.Exec(`DELETE FROM document_vector_build_progress WHERE generation_id = ?`, generationID); err != nil {
				return fmt.Errorf("reset document vector build cursor: %w", err)
			}
			return nil
		}
		if _, err := q.Exec(`
			INSERT INTO document_vector_build_progress(generation_id, after_chunk_id, updated_at)
			VALUES (?, ?, ?)
			ON CONFLICT (generation_id) DO UPDATE SET after_chunk_id = excluded.after_chunk_id, updated_at = excluded.updated_at`,
			generationID, afterChunkID, s.dialect.TimestampParam(now)); err != nil {
			return fmt.Errorf("checkpoint document vector build cursor: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return lifecycleErr
}

func (s *Store) GetDocumentVectorBuildCursor(ctx context.Context, generationID int64) (int64, error) {
	if generationID <= 0 {
		return 0, errors.New("document vector generation id must be positive")
	}
	var after int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT after_chunk_id FROM document_vector_build_progress WHERE generation_id = ?`), generationID).Scan(&after)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read document vector build cursor: %w", err)
	}
	return after, nil
}

func (s *Store) GetDocumentVectorProviderUsage(ctx context.Context, fingerprint string) (DocumentVectorProviderUsage, error) {
	if !documentVectorFingerprintPattern.MatchString(fingerprint) {
		return DocumentVectorProviderUsage{}, errors.New("document vector usage fingerprint is invalid")
	}
	usage := DocumentVectorProviderUsage{Fingerprint: fingerprint}
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT provider_calls, provider_documents, provider_chunks, provider_input_chars, updated_at
		FROM document_vector_provider_usage WHERE fingerprint = ?`), fingerprint).Scan(
		&usage.ProviderCalls, &usage.ProviderDocuments, &usage.ProviderChunks, &usage.ProviderInputChars, &usage.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return usage, nil
	}
	if err != nil {
		return DocumentVectorProviderUsage{}, fmt.Errorf("read document vector provider usage: %w", err)
	}
	usage.UpdatedAt = normalizeDocumentVectorTime(usage.UpdatedAt)
	return usage, nil
}

func (s *Store) GetDocumentVectorOperationsStatus(ctx context.Context, configured DocumentVectorGenerationSpec, egressFingerprint string, generationID int64, afterToken string, limit int) (DocumentVectorOperationsStatus, error) {
	if err := validateDocumentVectorGenerationSpec(configured); err != nil {
		return DocumentVectorOperationsStatus{}, err
	}
	if generationID < 0 {
		return DocumentVectorOperationsStatus{}, errors.New("document vector status generation id is invalid")
	}
	if !documentVectorFingerprintPattern.MatchString(egressFingerprint) {
		return DocumentVectorOperationsStatus{}, errors.New("document vector status egress fingerprint is invalid")
	}
	result := DocumentVectorOperationsStatus{
		ConfiguredSpec: configured, ConfiguredEgressFingerprint: egressFingerprint,
	}
	var err error
	result.Consent, err = s.GetDocumentVectorConsent(ctx, egressFingerprint)
	if err != nil {
		return DocumentVectorOperationsStatus{}, err
	}
	result.Usage, err = s.GetDocumentVectorProviderUsage(ctx, configured.Fingerprint)
	if err != nil {
		return DocumentVectorOperationsStatus{}, err
	}
	result.Active, err = s.GetActiveDocumentVectorGeneration(ctx)
	if err != nil {
		return DocumentVectorOperationsStatus{}, err
	}
	result.Building, err = s.GetBuildingDocumentVectorGeneration(ctx)
	if err != nil {
		return DocumentVectorOperationsStatus{}, err
	}
	selectedID := generationID
	if selectedID == 0 && result.Building != nil {
		selectedID = result.Building.ID
	} else if selectedID == 0 && result.Active != nil {
		selectedID = result.Active.ID
	}
	if selectedID != 0 {
		status, err := s.GetDocumentVectorGenerationStatus(ctx, selectedID, afterToken, limit)
		if err != nil {
			return DocumentVectorOperationsStatus{}, err
		}
		result.Selected = &status
		selectedUsesConfiguredTarget := result.Active != nil && result.Active.ID == selectedID &&
			result.Active.TargetExtractionProfileID == configured.TargetExtractionProfileID
		selectedUsesConfiguredTarget = selectedUsesConfiguredTarget || result.Building != nil && result.Building.ID == selectedID &&
			result.Building.TargetExtractionProfileID == configured.TargetExtractionProfileID
		if status.State != DocumentVectorGenerationRetired && selectedUsesConfiguredTarget {
			coverage, err := s.GetDocumentVectorCoverage(ctx, selectedID)
			if err != nil {
				return DocumentVectorOperationsStatus{}, err
			}
			result.Coverage = &coverage
		}
	}
	return result, nil
}

// GetOldestRetiredDocumentVectorGeneration returns the next durable cleanup
// ledger. Scheduled convergence handles at most one bounded page per tick.
func (s *Store) GetOldestRetiredDocumentVectorGeneration(ctx context.Context) (*DocumentVectorGeneration, error) {
	generation, found, err := scanDocumentVectorGeneration(s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT id, fingerprint, target_extraction_profile_id, embedding_profile, model, dimension,
		       state, created_at, activated_at, retired_at
		FROM document_vector_generations WHERE state = ? ORDER BY id LIMIT 1`), string(DocumentVectorGenerationRetired)))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil //nolint:nilnil // Absence means there is no retired generation awaiting cleanup.
	}
	return &generation, nil
}

// StartDocumentVectorRebuild creates a fresh building generation for the
// currently configured exact policy while leaving the active generation live.
func (s *Store) StartDocumentVectorRebuild(ctx context.Context, activeGenerationID int64, desired DocumentVectorGenerationSpec, now time.Time) (DocumentVectorGeneration, error) {
	if activeGenerationID <= 0 {
		return DocumentVectorGeneration{}, errors.New("active document vector generation id must be positive")
	}
	if err := validateDocumentVectorGenerationSpec(desired); err != nil {
		return DocumentVectorGeneration{}, err
	}
	now = normalizeDocumentVectorTime(now)
	if now.IsZero() {
		return DocumentVectorGeneration{}, errors.New("document vector rebuild time is required")
	}
	var result DocumentVectorGeneration
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state, _, err := s.lockDocumentVectorGeneration(q, activeGenerationID)
		if err != nil {
			return err
		}
		if state != DocumentVectorGenerationActive {
			return ErrDocumentVectorInvalidGenerationState
		}
		var target string
		if err := q.QueryRow(`SELECT target_profile_id FROM document_index_state WHERE singleton = 1`).Scan(&target); err != nil {
			return fmt.Errorf("read document vector target profile: %w", err)
		}
		if desired.TargetExtractionProfileID != target {
			return ErrDocumentVectorInvalidGenerationState
		}
		var building int
		if err := q.QueryRow(`SELECT COUNT(*) FROM document_vector_generations WHERE state = ?`, string(DocumentVectorGenerationBuilding)).Scan(&building); err != nil {
			return fmt.Errorf("check building document vector generation: %w", err)
		}
		if building != 0 {
			return errors.New("a document vector generation is already building")
		}
		rows, err := tx.QueryContext(ctx, s.Rebind(`
			SELECT target_extraction_profile_id, embedding_profile, model, dimension
			FROM document_vector_generations WHERE fingerprint = ?`), desired.Fingerprint)
		if err != nil {
			return fmt.Errorf("check document vector rebuild fingerprint: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var existing DocumentVectorGenerationSpec
			existing.Fingerprint = desired.Fingerprint
			if err := rows.Scan(&existing.TargetExtractionProfileID, &existing.EmbeddingProfile, &existing.Model, &existing.Dimension); err != nil {
				return fmt.Errorf("scan document vector rebuild fingerprint: %w", err)
			}
			if existing != desired {
				return fmt.Errorf("document vector generation fingerprint %q collides with a different immutable specification", desired.Fingerprint)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		var id int64
		if err := q.QueryRow(`INSERT INTO document_vector_generations
			(fingerprint, target_extraction_profile_id, embedding_profile, model, dimension, state, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`, desired.Fingerprint, desired.TargetExtractionProfileID,
			desired.EmbeddingProfile, desired.Model, desired.Dimension, string(DocumentVectorGenerationBuilding),
			s.dialect.TimestampParam(now)).Scan(&id); err != nil {
			return fmt.Errorf("start document vector rebuild: %w", err)
		}
		generation, found, err := scanDocumentVectorGeneration(q.QueryRow(`
			SELECT id, fingerprint, target_extraction_profile_id, embedding_profile, model, dimension,
			       state, created_at, activated_at, retired_at
			FROM document_vector_generations WHERE id = ?`, id))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("created document vector rebuild %d was not found", id)
		}
		result = generation
		return nil
	})
	return result, err
}
