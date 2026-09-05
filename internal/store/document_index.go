package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

var (
	ErrDocumentExtractionRebuildActive     = errors.New("document extraction rebuild is already active")
	ErrDocumentExtractionRebuildMissing    = errors.New("document extraction rebuild is not active")
	ErrDocumentIndexStatusScopeUnavailable = errors.New("current document index status scope is unavailable")
)

const authoritativeDocumentRoleSourcesSQL = "('mime_disposition','provider_explicit','importer_semantics','raw_mime_repair')"

func authoritativeDocumentRoleSourceSQL(alias string) string {
	return alias + ".role_source IN " + authoritativeDocumentRoleSourcesSQL
}

// DocumentExtractionProfile is an immutable hosted-extraction policy. The
// caller supplies its full policy fingerprint as both durable identity and
// rebuild boundary; provider consent is recorded separately.
type DocumentExtractionProfile struct {
	ID                string
	Fingerprint       string
	Provider          string
	Endpoint          string
	Region            string
	Model             string
	RetentionPosture  string
	TrainingPosture   string
	AllowedMediaTypes []string
	PolicyJSON        json.RawMessage
}

type DocumentProviderConsent struct {
	ProfileID          string
	ProfileFingerprint string
	RetentionPosture   string
	TrainingPosture    string
}

type DocumentOccurrence struct {
	OccurrenceKey     string
	AttachmentID      int64
	MessageID         int64
	SourceID          int64
	SourcePartKey     string
	StableSourcePart  bool
	CanonicalBlobHash string
	Filename          string
	MIMEType          string
	AttachmentRole    AttachmentRole
	RoleSource        AttachmentRoleSource
	SourceSequence    int64
}

// DocumentExtractionCandidate is one canonical blob owner that has at least
// one current eligible occurrence but no published extraction for the exact
// profile and input key.
type DocumentExtractionCandidate struct {
	AttachmentID      int64
	CanonicalBlobHash string
	MIMEType          string
	Size              int64
	MessageType       string
	SourceSequence    int64
}

type DocumentExtractionRebuild struct {
	ID                      string
	ProfileID               string
	ExtractionInputKey      string
	State                   string
	CreatedAt               time.Time
	CompletedAt             *time.Time
	SnapshotOwners          int64
	IncompleteCurrentOwners int64
}

type DocumentIndexStatus struct {
	ProfileExists              bool    `json:"profile_exists"`
	ProfileEnabled             bool    `json:"profile_enabled"`
	ExactConsent               bool    `json:"exact_consent"`
	ExtractionAttempts         int64   `json:"extraction_attempts"`
	SuccessfulAttempts         int64   `json:"successful_attempts"`
	FailedAttempts             int64   `json:"failed_attempts"`
	ProviderRequests           int64   `json:"provider_requests"`
	ProviderRetries            int64   `json:"provider_retries"`
	ProviderLatencyMillis      int64   `json:"provider_latency_millis"`
	AverageProviderLatencyMS   float64 `json:"average_provider_latency_millis"`
	VerifiedUploadBytes        int64   `json:"verified_upload_bytes"`
	ProcessedProviderUnits     int64   `json:"processed_provider_units"`
	ReportedProviderBytes      int64   `json:"reported_provider_bytes"`
	MissingProviderByteReports int64   `json:"missing_provider_byte_reports"`
	EligibleOccurrences        int64   `json:"eligible_occurrences"`
	EligibleOwners             int64   `json:"eligible_owners"`
	EligibleBytes              int64   `json:"eligible_bytes"`
	UnknownRoleOccurrences     int64   `json:"unknown_role_occurrences"`
	IneligibleRoleOccurrences  int64   `json:"ineligible_role_occurrences"`
	ReadyOwners                int64   `json:"ready_owners"`
	StagingOwners              int64   `json:"staging_owners"`
	RetryOwners                int64   `json:"retry_owners"`
	TerminalOwners             int64   `json:"terminal_owners"`
	MissingOwners              int64   `json:"missing_owners"`
	StoredPlaintextChunks      int64   `json:"stored_plaintext_chunks"`
}

// DocumentIndexStatusRequest identifies one exact profile and configured scope.
type DocumentIndexStatusRequest struct {
	ProfileID           string
	ExtractionInputKey  string
	AllowedMediaTypes   []string
	AllowedMessageTypes []string
}

// DocumentIndexRebuildStatus reports progress for the exact active rebuild.
type DocumentIndexRebuildStatus struct {
	SnapshotOwners  int64 `json:"snapshot_owners"`
	RemainingOwners int64 `json:"remaining_owners"`
}

// DocumentIndexStatusResponse combines scoped coverage and rebuild progress.
type DocumentIndexStatusResponse struct {
	Status        DocumentIndexStatus         `json:"status"`
	ActiveRebuild *DocumentIndexRebuildStatus `json:"active_rebuild,omitempty"`
}

type DocumentDerivativeGCResult struct {
	ExtractionsRemoved  int `json:"extractions_removed"`
	CurrentHeadsRemoved int `json:"current_heads_removed"`
}

type DocumentDerivedPurgeResult struct {
	ExtractionsRemoved int `json:"extractions_removed"`
	HeadsRemoved       int `json:"heads_removed"`
}

// EnsureDocumentExtractionProfile inserts one immutable profile or verifies
// that an existing row has exactly the same policy. It never records consent
// and never enables hosted processing.
func (s *Store) EnsureDocumentExtractionProfile(
	ctx context.Context,
	profile DocumentExtractionProfile,
) (bool, error) {
	allowedJSON, policyJSON, err := validateDocumentProfile(profile)
	if err != nil {
		return false, err
	}
	var created bool
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		result, execErr := tx.Exec(`
			INSERT INTO document_extraction_profiles
				(id, fingerprint, provider, endpoint, region, model,
				 retention_posture, training_posture, allowed_media_types,
				 policy_json, enabled)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`, `+s.dialect.JSONBindExpr()+`, FALSE)
			ON CONFLICT (id) DO NOTHING`,
			profile.ID, profile.Fingerprint, profile.Provider, profile.Endpoint,
			profile.Region, profile.Model, profile.RetentionPosture,
			profile.TrainingPosture, string(allowedJSON), string(policyJSON),
		)
		if execErr != nil {
			return fmt.Errorf("insert document extraction profile: %w", execErr)
		}
		rows, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("read document extraction profile insert result: %w", rowsErr)
		}
		created = rows == 1

		var stored DocumentExtractionProfile
		var storedAllowed, storedPolicy string
		if scanErr := tx.QueryRow(`
			SELECT id, fingerprint, provider, endpoint, region, model,
			       retention_posture, training_posture,
			       CAST(allowed_media_types AS TEXT), CAST(policy_json AS TEXT)
			FROM document_extraction_profiles WHERE id = ?`, profile.ID).Scan(
			&stored.ID, &stored.Fingerprint, &stored.Provider, &stored.Endpoint,
			&stored.Region, &stored.Model, &stored.RetentionPosture,
			&stored.TrainingPosture, &storedAllowed, &storedPolicy,
		); scanErr != nil {
			return fmt.Errorf("read document extraction profile: %w", scanErr)
		}
		if stored.ID != profile.ID || stored.Fingerprint != profile.Fingerprint ||
			stored.Provider != profile.Provider || stored.Endpoint != profile.Endpoint ||
			stored.Region != profile.Region || stored.Model != profile.Model ||
			stored.RetentionPosture != profile.RetentionPosture ||
			stored.TrainingPosture != profile.TrainingPosture ||
			!equalJSON([]byte(storedAllowed), allowedJSON) ||
			!equalJSON([]byte(storedPolicy), policyJSON) {
			return errors.New("document extraction profile ID already has different immutable policy")
		}
		return nil
	})
	return created, err
}

// GetCurrentDocumentIndexStatusScope resolves the exact durable target profile
// selected by document_index_state and its immutable media allowlist. The
// profile identifier is used only as an internal query coordinate; API status
// responses do not publish it.
func (s *Store) GetCurrentDocumentIndexStatusScope(
	ctx context.Context,
) (string, []string, error) {
	var profileID string
	var allowedJSON string
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT p.id, CAST(p.allowed_media_types AS TEXT)
		FROM document_index_state state
		JOIN document_extraction_profiles p ON p.id = state.target_profile_id
		WHERE state.singleton = 1
		  AND p.enabled = TRUE
		  AND p.retired_at IS NULL`)).Scan(&profileID, &allowedJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrDocumentIndexStatusScopeUnavailable
	}
	if err != nil {
		return "", nil, fmt.Errorf("read current document index status scope: %w", err)
	}
	var mediaTypes []string
	if err := json.Unmarshal([]byte(allowedJSON), &mediaTypes); err != nil {
		return "", nil, errors.New("current document index status scope is invalid")
	}
	slices.Sort(mediaTypes)
	mediaTypes = slices.Compact(mediaTypes)
	if profileID == "" || len(mediaTypes) == 0 || slices.Contains(mediaTypes, "") {
		return "", nil, ErrDocumentIndexStatusScopeUnavailable
	}
	return profileID, mediaTypes, nil
}

// RecordDocumentProviderConsent enables one exact immutable profile. A change
// to any privacy assertion or fingerprint requires a different profile and a
// new explicit consent record.
func (s *Store) RecordDocumentProviderConsent(
	ctx context.Context,
	consent DocumentProviderConsent,
) error {
	if consent.ProfileID == "" || !validLowerSHA256(consent.ProfileFingerprint) ||
		consent.RetentionPosture == "" || consent.TrainingPosture == "" ||
		consent.RetentionPosture == "unknown" || consent.TrainingPosture == "unknown" {
		return errors.New("document provider consent requires an exact profile and explicit privacy postures")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var fingerprint, retention, training string
		var retired bool
		if err := tx.QueryRow(`
			SELECT fingerprint, retention_posture, training_posture,
			       retired_at IS NOT NULL
			FROM document_extraction_profiles WHERE id = ?`, consent.ProfileID).Scan(
			&fingerprint, &retention, &training, &retired,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("document provider consent profile does not exist")
			}
			return fmt.Errorf("read document provider consent profile: %w", err)
		}
		if fingerprint != consent.ProfileFingerprint || retention != consent.RetentionPosture || training != consent.TrainingPosture {
			return errors.New("document provider consent does not match immutable profile")
		}
		if retired {
			return errors.New("document provider consent profile is retired")
		}
		if _, err := tx.Exec(`
			INSERT INTO document_provider_consents
				(profile_id, profile_fingerprint, retention_posture, training_posture)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (profile_id) DO NOTHING`,
			consent.ProfileID, consent.ProfileFingerprint,
			consent.RetentionPosture, consent.TrainingPosture,
		); err != nil {
			return fmt.Errorf("record document provider consent: %w", err)
		}
		var storedFingerprint, storedRetention, storedTraining string
		if err := tx.QueryRow(`
			SELECT profile_fingerprint, retention_posture, training_posture
			FROM document_provider_consents WHERE profile_id = ?`, consent.ProfileID).Scan(
			&storedFingerprint, &storedRetention, &storedTraining,
		); err != nil {
			return fmt.Errorf("read document provider consent: %w", err)
		}
		if storedFingerprint != consent.ProfileFingerprint ||
			storedRetention != consent.RetentionPosture || storedTraining != consent.TrainingPosture {
			return errors.New("document provider consent already records different assertions")
		}
		if _, err := tx.Exec(`
			UPDATE document_extraction_profiles SET enabled = TRUE
			WHERE id = ? AND retired_at IS NULL`, consent.ProfileID); err != nil {
			return fmt.Errorf("enable consented document extraction profile: %w", err)
		}
		var currentTarget sql.NullString
		if err := tx.QueryRow(`
			SELECT target_profile_id FROM document_index_state WHERE singleton = 1`,
		).Scan(&currentTarget); err != nil {
			return fmt.Errorf("read target document extraction profile: %w", err)
		}
		if currentTarget.Valid && currentTarget.String == consent.ProfileID {
			return nil
		}
		var hasHeads bool
		if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM document_extraction_heads)`).Scan(&hasHeads); err != nil {
			return fmt.Errorf("check document heads before selecting target profile: %w", err)
		}
		if _, err := tx.Exec(`
			UPDATE document_index_state
			SET target_profile_id = ?, updated_at = CURRENT_TIMESTAMP
			WHERE singleton = 1`, consent.ProfileID); err != nil {
			return fmt.Errorf("select target document extraction profile: %w", err)
		}
		if hasHeads {
			return bumpDocumentIndexRevision(tx)
		}
		return nil
	})
}

// ReconcileDocumentOccurrence resolves current metadata through the same
// trusted-CAS authority as file downloads. Only live, standalone occurrences
// with authoritative role provenance and locally available canonical bytes
// are retained. It returns false for an ineligible or missing attachment.
func (s *Store) ReconcileDocumentOccurrence(
	ctx context.Context,
	attachmentID int64,
	sourceSequence int64,
) (DocumentOccurrence, bool, error) {
	if attachmentID <= 0 || sourceSequence < 0 {
		return DocumentOccurrence{}, false, errors.New("document occurrence reconciliation has invalid coordinates")
	}
	var occurrence DocumentOccurrence
	var eligible bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockDocumentOccurrenceAttachmentTx(ctx, tx, attachmentID); err != nil {
			return err
		}
		file, found, err := s.getDocumentFileMetadataTx(ctx, tx, attachmentID)
		if err != nil {
			return err
		}
		if !found || !eligibleDocumentFile(file) {
			removed, removedOccurrence, err := removeDocumentOccurrenceTx(
				tx, attachmentID, sourceSequence,
			)
			if err != nil || !removed {
				return err
			}
			return s.publishDocumentOccurrencePersonSweepChangeTx(
				ctx, tx, removedOccurrence, peoplesweep.ChangeScope,
				peoplesweep.EvidenceEffectScopeUnlinked,
			)
		}
		if err := s.lockDocumentPublicationHashTx(ctx, tx, file.ContentHash); err != nil {
			return err
		}
		occurrence = DocumentOccurrence{
			OccurrenceKey:     documentOccurrenceKey(file.MessageID, file.SourcePartKey, file.ID),
			AttachmentID:      file.ID,
			MessageID:         file.MessageID,
			SourceID:          file.SourceID,
			SourcePartKey:     file.SourcePartKey,
			StableSourcePart:  file.SourcePartKey != "",
			CanonicalBlobHash: file.ContentHash,
			Filename:          file.Filename,
			MIMEType:          file.MimeType,
			AttachmentRole:    file.AttachmentRole,
			RoleSource:        file.RoleSource,
			SourceSequence:    sourceSequence,
		}
		linked, replaced, err := upsertDocumentOccurrenceTx(tx, occurrence)
		if err != nil {
			return err
		}
		if replaced != nil {
			kind := peoplesweep.ChangeScope
			effect := peoplesweep.EvidenceEffectScopeUnlinked
			if replaced.OccurrenceKey == occurrence.OccurrenceKey {
				kind = peoplesweep.ChangeUpsert
				effect = peoplesweep.EvidenceEffectSourceEdited
			}
			if err := s.publishDocumentOccurrencePersonSweepChangeTx(
				ctx, tx, *replaced, kind, effect,
			); err != nil {
				return err
			}
		}
		if linked {
			if err := s.publishLinkedDocumentOccurrencePersonSweepChangesTx(
				ctx, tx, occurrence,
			); err != nil {
				return err
			}
		}
		eligible = true
		return nil
	})
	if err != nil {
		return DocumentOccurrence{}, false, err
	}
	return occurrence, eligible, nil
}

func (s *Store) lockDocumentPublicationHashTx(
	ctx context.Context, tx *loggedTx, canonicalBlobHash string,
) error {
	if !s.IsPostgreSQL() {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(
		hashtextextended(CAST(? AS TEXT), 0))`,
		"msgvault.document_publication.hash:"+canonicalBlobHash,
	); err != nil {
		return fmt.Errorf("lock document publication hash: %w", err)
	}
	return nil
}

func (s *Store) lockDocumentOccurrenceAttachmentTx(
	ctx context.Context, tx *loggedTx, attachmentID int64,
) error {
	if s.IsPostgreSQL() {
		// The occurrence row may not exist yet, so there is no row to lock.
		// Serialize the eligibility read and its write by attachment identity.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(
			hashtextextended(CAST(? AS TEXT), 0))`,
			fmt.Sprintf("msgvault.document_occurrence.attachment:%d", attachmentID),
		); err != nil {
			return fmt.Errorf("lock document occurrence attachment: %w", err)
		}
		return nil
	}
	// Reserve SQLite's writer slot before reading attachment authority. A
	// deferred transaction cannot upgrade a stale WAL snapshot after a
	// concurrent reconciliation commits.
	if _, err := tx.ExecContext(ctx, `
		UPDATE document_index_state SET revision = revision WHERE singleton = 1`); err != nil {
		return fmt.Errorf("lock document occurrence reconciliation: %w", err)
	}
	return nil
}

func (s *Store) getDocumentFileMetadataTx(
	ctx context.Context, tx *loggedTx, attachmentID int64,
) (FileMetadata, bool, error) {
	var file FileMetadata
	err := tx.QueryRowContext(ctx, `
		SELECT a.id, a.message_id, m.conversation_id,
			m.source_id, COALESCE(m.source_message_id, ''),
			COALESCE(m.message_type, ''), COALESCE(c.conversation_type, ''),
			COALESCE(a.filename, ''), COALESCE(a.mime_type, ''), COALESCE(a.size, 0),
			COALESCE(a.content_hash, ''), COALESCE(a.storage_path, ''),
			a.attachment_role, a.role_source,
			COALESCE(a.source_part_key, ''), COALESCE(a.content_id, '')
		FROM attachments a
		JOIN messages m ON m.id = a.message_id
		JOIN conversations c ON c.id = m.conversation_id
		WHERE a.id = ? AND `+LiveMessagesWhere("m", true), attachmentID).Scan(
		&file.ID, &file.MessageID, &file.ConversationID,
		&file.SourceID, &file.SourceMessageID, &file.MessageType, &file.ConversationType,
		&file.Filename, &file.MimeType, &file.Size, &file.ContentHash, &file.StoragePath,
		&file.AttachmentRole, &file.RoleSource, &file.SourcePartKey, &file.ContentID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return FileMetadata{}, false, nil
	}
	if err != nil {
		return FileMetadata{}, false, fmt.Errorf("read document occurrence attachment metadata: %w", err)
	}
	normalizeFileMetadataStorage(&file)
	return file, true, nil
}

func (s *Store) GetDocumentIndexRevision(ctx context.Context) (int64, error) {
	var revision int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT revision FROM document_index_state WHERE singleton = 1`).Scan(&revision); err != nil {
		return 0, fmt.Errorf("read document index revision: %w", err)
	}
	return revision, nil
}

// HasActiveDocumentProviderConsent reports whether any enabled, unretired
// extraction profile has the exact recorded provider posture it requires.
// It is the activation gate for bootstrapping the attachment journal consumer.
func (s *Store) HasActiveDocumentProviderConsent(ctx context.Context) (bool, error) {
	var consented bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM document_extraction_profiles p
			JOIN document_provider_consents c ON c.profile_id = p.id
			WHERE p.enabled = TRUE
			  AND p.retired_at IS NULL
			  AND c.profile_fingerprint = p.fingerprint
			  AND c.retention_posture = p.retention_posture
			  AND c.training_posture = p.training_posture
		)`).Scan(&consented)
	if err != nil {
		return false, fmt.Errorf("read active document provider consent: %w", err)
	}
	return consented, nil
}

func (s *Store) GetDocumentIndexStatus(ctx context.Context, profileID string) (DocumentIndexStatus, error) {
	if profileID == "" {
		return DocumentIndexStatus{}, errors.New("document index status requires a profile ID")
	}
	var status DocumentIndexStatus
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT EXISTS (SELECT 1 FROM document_extraction_profiles WHERE id = ?),
		       EXISTS (
		           SELECT 1 FROM document_extraction_profiles
		           WHERE id = ? AND enabled = TRUE AND retired_at IS NULL
		       ),
		       EXISTS (
		           SELECT 1 FROM document_extraction_profiles p
		           JOIN document_provider_consents c ON c.profile_id = p.id
		           WHERE p.id = ?
		             AND c.profile_fingerprint = p.fingerprint
		             AND c.retention_posture = p.retention_posture
		             AND c.training_posture = p.training_posture
		       ),
		       (SELECT COUNT(*) FROM document_occurrences WHERE attachment_role = 'standalone'),
		       (SELECT COUNT(*) FROM document_extraction_heads WHERE profile_id = ?),
		       (SELECT COUNT(*) FROM document_extractions WHERE profile_id = ? AND state = 'staging'),
		       (SELECT COUNT(*) FROM document_extractions
		        WHERE profile_id = ? AND state = 'tombstoned' AND next_retry_at IS NOT NULL),
		       (SELECT COUNT(*) FROM document_extractions WHERE profile_id = ? AND state = 'terminal'),
		       (SELECT COALESCE(SUM(CASE WHEN state = 'ready' THEN 1 ELSE 0 END), 0) +
		               COALESCE(SUM(attempt_count), 0)
		        FROM document_extractions WHERE profile_id = ?),
		       (SELECT COUNT(*) FROM document_extractions WHERE profile_id = ? AND state = 'ready'),
		       (SELECT COALESCE(SUM(attempt_count), 0) FROM document_extractions WHERE profile_id = ?),
		       (SELECT COALESCE(SUM(request_count), 0) FROM document_extractions WHERE profile_id = ?),
		       (SELECT COALESCE(SUM(retry_count), 0) FROM document_extractions WHERE profile_id = ?),
		       (SELECT COALESCE(SUM(provider_latency_ms), 0) FROM document_extractions WHERE profile_id = ?),
		       (SELECT COALESCE(SUM(CASE WHEN request_count > 0 THEN local_bytes ELSE 0 END), 0)
		        FROM document_extractions WHERE profile_id = ?),
		       (SELECT COALESCE(SUM(units_processed), 0) FROM document_extractions WHERE profile_id = ?),
		       (SELECT COALESCE(SUM(provider_bytes), 0) FROM document_extractions WHERE profile_id = ?),
		       (SELECT COUNT(*) FROM document_extractions
		        WHERE profile_id = ? AND state = 'ready' AND provider_bytes IS NULL)`),
		profileID, profileID, profileID, profileID, profileID, profileID, profileID,
		profileID, profileID, profileID, profileID, profileID, profileID, profileID,
		profileID, profileID, profileID,
	).Scan(
		&status.ProfileExists, &status.ProfileEnabled, &status.ExactConsent,
		&status.EligibleOccurrences, &status.ReadyOwners, &status.StagingOwners,
		&status.RetryOwners, &status.TerminalOwners,
		&status.ExtractionAttempts, &status.SuccessfulAttempts, &status.FailedAttempts,
		&status.ProviderRequests, &status.ProviderRetries, &status.ProviderLatencyMillis,
		&status.VerifiedUploadBytes, &status.ProcessedProviderUnits,
		&status.ReportedProviderBytes, &status.MissingProviderByteReports,
	)
	if err != nil {
		return DocumentIndexStatus{}, fmt.Errorf("read document index status: %w", err)
	}
	if status.ProviderRequests > 0 {
		status.AverageProviderLatencyMS = float64(status.ProviderLatencyMillis) / float64(status.ProviderRequests)
	}
	return status, nil
}

// GetDocumentIndexStatusForScope classifies each currently eligible canonical
// owner once. Ready, staging, retry, terminal, and missing are mutually
// exclusive in that priority order, so repeated immutable attempts do not
// inflate operator-facing coverage.
func (s *Store) GetDocumentIndexStatusForScope(
	ctx context.Context,
	profileID string,
	extractionInputKey string,
	allowedMediaTypes []string,
	allowedMessageTypes []string,
) (DocumentIndexStatus, error) {
	status, err := s.GetDocumentIndexStatus(ctx, profileID)
	if err != nil {
		return DocumentIndexStatus{}, err
	}
	scopeSQL, scopeArgs, err := documentOccurrenceScopeSQL(
		"o", "m", allowedMediaTypes, allowedMessageTypes,
	)
	if err != nil {
		return DocumentIndexStatus{}, err
	}
	args := slices.Clone(scopeArgs)
	for range 4 {
		args = append(args, profileID, extractionInputKey)
	}
	err = s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		WITH eligible_occurrences AS (
			SELECT o.canonical_blob_hash, COALESCE(a.size, 0) AS owner_size
			FROM document_occurrences o
			JOIN attachments a ON a.id = o.attachment_id
			JOIN messages m ON m.id = o.message_id
			WHERE `+scopeSQL+`
		), eligible AS (
			SELECT canonical_blob_hash, MAX(owner_size) AS owner_size
			FROM eligible_occurrences
			GROUP BY canonical_blob_hash
		), classified AS (
			SELECT e.canonical_blob_hash, e.owner_size,
			       CASE
			         WHEN EXISTS (
			             SELECT 1 FROM document_extraction_heads h
			             WHERE h.profile_id = ? AND h.extraction_input_key = ?
			               AND h.canonical_blob_hash = e.canonical_blob_hash
			         ) THEN 'ready'
			         WHEN EXISTS (
			             SELECT 1 FROM document_extractions x
			             WHERE x.profile_id = ? AND x.extraction_input_key = ?
			               AND x.canonical_blob_hash = e.canonical_blob_hash AND x.state = 'staging'
			         ) THEN 'staging'
			         WHEN EXISTS (
			             SELECT 1 FROM document_extractions x
			             WHERE x.profile_id = ? AND x.extraction_input_key = ?
			               AND x.canonical_blob_hash = e.canonical_blob_hash
			               AND x.state = 'tombstoned' AND x.next_retry_at IS NOT NULL
			         ) THEN 'retry'
			         WHEN EXISTS (
			             SELECT 1 FROM document_extractions x
			             WHERE x.profile_id = ? AND x.extraction_input_key = ?
			               AND x.canonical_blob_hash = e.canonical_blob_hash AND x.state = 'terminal'
			         ) THEN 'terminal'
			         ELSE 'missing'
			       END AS coverage_state
			FROM eligible e
		)
		SELECT (SELECT COUNT(*) FROM eligible_occurrences),
		       COUNT(*), COALESCE(SUM(owner_size), 0),
		       COALESCE(SUM(CASE WHEN coverage_state = 'ready' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN coverage_state = 'staging' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN coverage_state = 'retry' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN coverage_state = 'terminal' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN coverage_state = 'missing' THEN 1 ELSE 0 END), 0)
		FROM classified`), args...).Scan(
		&status.EligibleOccurrences, &status.EligibleOwners, &status.EligibleBytes,
		&status.ReadyOwners, &status.StagingOwners, &status.RetryOwners,
		&status.TerminalOwners, &status.MissingOwners,
	)
	if err != nil {
		return DocumentIndexStatus{}, fmt.Errorf("read scoped document index coverage: %w", err)
	}
	mediaScopeSQL, mediaScopeArgs, err := documentOccurrenceMediaScopeSQL(
		"a", "m", allowedMediaTypes, allowedMessageTypes,
	)
	if err != nil {
		return DocumentIndexStatus{}, err
	}
	err = s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT COALESCE(SUM(CASE WHEN a.attachment_role = 'unknown' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN a.attachment_role NOT IN ('standalone', 'unknown') THEN 1 ELSE 0 END), 0)
		FROM attachments a
		JOIN messages m ON m.id = a.message_id
		WHERE `+mediaScopeSQL), mediaScopeArgs...).Scan(
		&status.UnknownRoleOccurrences, &status.IneligibleRoleOccurrences,
	)
	if err != nil {
		return DocumentIndexStatus{}, fmt.Errorf("read document index role exclusions: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_chunks`).Scan(
		&status.StoredPlaintextChunks,
	); err != nil {
		return DocumentIndexStatus{}, fmt.Errorf("count stored document plaintext chunks: %w", err)
	}
	return status, nil
}

func (s *Store) StartDocumentExtractionRebuild(
	ctx context.Context,
	rebuildID string,
	profileID string,
	extractionInputKey string,
	allowedMediaTypes []string,
	allowedMessageTypes []string,
) (DocumentExtractionRebuild, error) {
	if rebuildID == "" || profileID == "" || extractionInputKey == "" {
		return DocumentExtractionRebuild{}, errors.New("document extraction rebuild identity is incomplete")
	}
	scopeSQL, scopeArgs, err := documentOccurrenceScopeSQL(
		"o", "m", allowedMediaTypes, allowedMessageTypes,
	)
	if err != nil {
		return DocumentExtractionRebuild{}, err
	}
	rebuild := DocumentExtractionRebuild{
		ID: rebuildID, ProfileID: profileID, ExtractionInputKey: extractionInputKey, State: "building",
	}
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		var active bool
		if err := q.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM document_extraction_rebuilds
				WHERE profile_id = ? AND extraction_input_key = ? AND state = 'building'
			)`, profileID, extractionInputKey).Scan(&active); err != nil {
			return fmt.Errorf("check active document extraction rebuild: %w", err)
		}
		if active {
			return ErrDocumentExtractionRebuildActive
		}
		var authorized bool
		if err := q.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM document_extraction_profiles p
				JOIN document_provider_consents c ON c.profile_id = p.id
				WHERE p.id = ? AND p.enabled = TRUE AND p.retired_at IS NULL
				  AND c.profile_fingerprint = p.fingerprint
				  AND c.retention_posture = p.retention_posture
				  AND c.training_posture = p.training_posture
			)`, profileID).Scan(&authorized); err != nil {
			return fmt.Errorf("check document extraction rebuild authority: %w", err)
		}
		if !authorized {
			return errors.New("document extraction rebuild profile is not enabled with exact consent")
		}
		if _, err := q.Exec(`
			INSERT INTO document_extraction_rebuilds
				(id, profile_id, extraction_input_key, state)
			VALUES (?, ?, ?, 'building')`, rebuildID, profileID, extractionInputKey); err != nil {
			return fmt.Errorf("start document extraction rebuild: %w", err)
		}
		args := make([]any, 0, len(scopeArgs)+1)
		args = append(args, rebuildID)
		args = append(args, scopeArgs...)
		result, err := q.Exec(`
			INSERT INTO document_extraction_rebuild_targets (rebuild_id, canonical_blob_hash)
			SELECT ?, o.canonical_blob_hash
			FROM document_occurrences o
			JOIN messages m ON m.id = o.message_id
			WHERE `+scopeSQL+`
			GROUP BY o.canonical_blob_hash`, args...)
		if err != nil {
			return fmt.Errorf("snapshot document extraction rebuild targets: %w", err)
		}
		rebuild.SnapshotOwners, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document extraction rebuild target count: %w", err)
		}
		return q.QueryRow(`
			SELECT created_at FROM document_extraction_rebuilds WHERE id = ?`, rebuildID,
		).Scan(&rebuild.CreatedAt)
	})
	if err != nil {
		return DocumentExtractionRebuild{}, err
	}
	rebuild.IncompleteCurrentOwners = rebuild.SnapshotOwners
	return rebuild, nil
}

func (s *Store) GetActiveDocumentExtractionRebuild(
	ctx context.Context,
	profileID string,
	extractionInputKey string,
) (DocumentExtractionRebuild, error) {
	if profileID == "" || extractionInputKey == "" {
		return DocumentExtractionRebuild{}, errors.New("document extraction rebuild lookup is incomplete")
	}
	var rebuild DocumentExtractionRebuild
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT r.id, r.profile_id, r.extraction_input_key, r.state, r.created_at,
		       (SELECT COUNT(*) FROM document_extraction_rebuild_targets t WHERE t.rebuild_id = r.id)
		FROM document_extraction_rebuilds r
		WHERE r.profile_id = ? AND r.extraction_input_key = ? AND r.state = 'building'`),
		profileID, extractionInputKey,
	).Scan(&rebuild.ID, &rebuild.ProfileID, &rebuild.ExtractionInputKey,
		&rebuild.State, &rebuild.CreatedAt, &rebuild.SnapshotOwners)
	if errors.Is(err, sql.ErrNoRows) {
		return DocumentExtractionRebuild{}, ErrDocumentExtractionRebuildMissing
	}
	if err != nil {
		return DocumentExtractionRebuild{}, fmt.Errorf("read active document extraction rebuild: %w", err)
	}
	return rebuild, nil
}

func (s *Store) CountIncompleteDocumentExtractionRebuild(
	ctx context.Context,
	rebuild DocumentExtractionRebuild,
	allowedMediaTypes []string,
	allowedMessageTypes []string,
) (int64, error) {
	if rebuild.ID == "" || rebuild.ProfileID == "" || rebuild.ExtractionInputKey == "" || rebuild.State != "building" {
		return 0, errors.New("document extraction rebuild is invalid")
	}
	scopeSQL, scopeArgs, err := documentOccurrenceScopeSQL(
		"o", "m", allowedMediaTypes, allowedMessageTypes,
	)
	if err != nil {
		return 0, err
	}
	args := make([]any, 0, len(scopeArgs)+2)
	args = append(args, rebuild.ID)
	args = append(args, scopeArgs...)
	args = append(args, rebuild.ID)
	var incomplete int64
	err = s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT COUNT(*)
		FROM document_extraction_rebuild_targets t
		WHERE t.rebuild_id = ?
		  AND EXISTS (
		      SELECT 1 FROM document_occurrences o
		      JOIN messages m ON m.id = o.message_id
		      WHERE o.canonical_blob_hash = t.canonical_blob_hash AND `+scopeSQL+`
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM document_extractions e
		      WHERE e.rebuild_id = ? AND e.canonical_blob_hash = t.canonical_blob_hash
		        AND e.state = 'ready'
		  )`), args...).Scan(&incomplete)
	if err != nil {
		return 0, fmt.Errorf("count incomplete document extraction rebuild owners: %w", err)
	}
	return incomplete, nil
}

func (s *Store) CompleteDocumentExtractionRebuild(ctx context.Context, rebuildID string) error {
	if rebuildID == "" {
		return errors.New("document extraction rebuild completion requires an ID")
	}
	result, err := s.db.ExecContext(ctx, s.dialect.Rebind(`
		UPDATE document_extraction_rebuilds
		SET state = 'completed', completed_at = `+s.dialect.Now()+`
		WHERE id = ? AND state = 'building'`), rebuildID)
	if err != nil {
		return fmt.Errorf("complete document extraction rebuild: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read document extraction rebuild completion: %w", err)
	}
	if updated != 1 {
		return ErrDocumentExtractionRebuildMissing
	}
	return nil
}

// GarbageCollectDocumentDerivatives removes bounded stale extraction
// revisions. Superseded revisions are eligible after the recovery cutoff.
// Current owners and terminal suppression records are eligible only after
// their final live occurrence is gone. Active staging leases are never
// collected.
func (s *Store) GarbageCollectDocumentDerivatives(
	ctx context.Context,
	before time.Time,
	limit int,
) (DocumentDerivativeGCResult, error) {
	if before.IsZero() || limit <= 0 || limit > 10_000 {
		return DocumentDerivativeGCResult{}, errors.New("document derivative GC has invalid bounds")
	}
	result := DocumentDerivativeGCResult{}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.Query(`
			SELECT e.id, e.state,
			       CASE WHEN h.extraction_id IS NULL THEN FALSE ELSE TRUE END
			FROM document_extractions e
			LEFT JOIN document_extraction_heads h ON h.extraction_id = e.id
			WHERE e.updated_at < ?
			  AND (e.state != 'staging' OR e.lease_until IS NULL OR e.lease_until < `+s.dialect.Now()+`)
			  AND ((h.extraction_id IS NULL AND e.state != 'terminal') OR NOT EXISTS (
			      SELECT 1 FROM document_occurrences o
			      JOIN messages m ON m.id = o.message_id
			      WHERE o.canonical_blob_hash = e.canonical_blob_hash
			        AND o.attachment_role = 'standalone'
			        AND `+LiveMessagesWhere("m", true)+`
			  ))
			ORDER BY e.updated_at, e.id
			LIMIT ?`, before.UTC(), limit)
		if err != nil {
			return fmt.Errorf("list stale document derivatives: %w", err)
		}
		var extractionIDs []string
		var terminalRemoved bool
		for rows.Next() {
			var extractionID string
			var state string
			var current bool
			if err := rows.Scan(&extractionID, &state, &current); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan stale document derivative: %w", err)
			}
			extractionIDs = append(extractionIDs, extractionID)
			terminalRemoved = terminalRemoved || state == "terminal"
			if current {
				result.CurrentHeadsRemoved++
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate stale document derivatives: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close stale document derivative rows: %w", err)
		}
		if len(extractionIDs) == 0 {
			return nil
		}
		args := make([]any, len(extractionIDs))
		for index := range extractionIDs {
			args[index] = extractionIDs[index]
		}
		deleted, err := tx.Exec(`DELETE FROM document_extractions WHERE id IN (`+
			documentPlaceholders(len(extractionIDs))+`)`, args...)
		if err != nil {
			return fmt.Errorf("delete stale document derivatives: %w", err)
		}
		count, err := deleted.RowsAffected()
		if err != nil {
			return fmt.Errorf("read stale document derivative delete count: %w", err)
		}
		result.ExtractionsRemoved = int(count)
		if result.CurrentHeadsRemoved > 0 || terminalRemoved {
			return bumpDocumentIndexRevision(tx)
		}
		return nil
	})
	return result, err
}

func (s *Store) RetryDocumentExtraction(
	ctx context.Context,
	profileID string,
	canonicalBlobHash string,
) (bool, error) {
	if profileID == "" || !validLowerSHA256(canonicalBlobHash) {
		return false, errors.New("document extraction retry requires an exact profile and SHA-256")
	}
	var changed bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var hasHead bool
		if err := tx.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM document_extraction_heads
				WHERE profile_id = ? AND canonical_blob_hash = ?
			)`, profileID, canonicalBlobHash).Scan(&hasHead); err != nil {
			return fmt.Errorf("check current document extraction before retry: %w", err)
		}
		retryScope := ""
		if hasHead {
			var activeRebuildFailure bool
			if err := tx.QueryRow(`
				SELECT EXISTS (
					SELECT 1 FROM document_extractions e
					JOIN document_extraction_rebuilds r ON r.id = e.rebuild_id
					WHERE e.profile_id = ? AND e.canonical_blob_hash = ?
					  AND e.state IN ('terminal', 'tombstoned') AND r.state = 'building'
				)`, profileID, canonicalBlobHash).Scan(&activeRebuildFailure); err != nil {
				return fmt.Errorf("check active rebuild failure before retry: %w", err)
			}
			if !activeRebuildFailure {
				return errors.New("document extraction is already current for this profile")
			}
			retryScope = `
			  AND EXISTS (
			      SELECT 1 FROM document_extraction_rebuilds r
			      WHERE r.id = document_extractions.rebuild_id AND r.state = 'building'
			  )`
		}
		var terminalRows int
		if err := tx.QueryRow(`
			SELECT COUNT(*)
			FROM document_extractions
			WHERE profile_id = ? AND canonical_blob_hash = ?
			  AND state = 'terminal'`+retryScope, profileID, canonicalBlobHash).Scan(&terminalRows); err != nil {
			return fmt.Errorf("count terminal document extractions before retry: %w", err)
		}
		result, err := tx.Exec(`
			UPDATE document_extractions
			SET state = 'tombstoned', next_retry_at = `+s.dialect.Now()+`,
			    terminal_reason = NULL, updated_at = CURRENT_TIMESTAMP
			WHERE profile_id = ? AND canonical_blob_hash = ?
			  AND state IN ('terminal', 'tombstoned')`+retryScope, profileID, canonicalBlobHash)
		if err != nil {
			return fmt.Errorf("schedule document extraction retry: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document extraction retry result: %w", err)
		}
		changed = rows > 0
		if changed && terminalRows > 0 {
			return bumpDocumentIndexRevision(tx)
		}
		return nil
	})
	return changed, err
}

func (s *Store) RetireDocumentExtractionProfile(ctx context.Context, profileID string) (bool, error) {
	if profileID == "" || len(profileID) > 200 {
		return false, errors.New("document profile retirement requires an exact profile ID")
	}
	var changed bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var headCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM document_extraction_heads WHERE profile_id = ?`,
			profileID).Scan(&headCount); err != nil {
			return fmt.Errorf("count document profile heads before retirement: %w", err)
		}
		result, err := tx.Exec(`
			UPDATE document_extraction_profiles
			SET enabled = FALSE, retired_at = CURRENT_TIMESTAMP
			WHERE id = ? AND retired_at IS NULL`, profileID)
		if err != nil {
			return fmt.Errorf("retire document extraction profile: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document profile retirement result: %w", err)
		}
		changed = rows > 0
		if !changed {
			return nil
		}
		result, err = tx.Exec(`
			UPDATE document_index_state
			SET target_profile_id = NULL
			WHERE singleton = 1 AND target_profile_id = ?`, profileID)
		if err != nil {
			return fmt.Errorf("clear retired target document extraction profile: %w", err)
		}
		targetRows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read cleared document target result: %w", err)
		}
		if headCount > 0 || targetRows > 0 {
			return bumpDocumentIndexRevision(tx)
		}
		return nil
	})
	return changed, err
}

func (s *Store) PurgeDocumentDerivedByHash(
	ctx context.Context,
	canonicalBlobHash string,
) (DocumentDerivedPurgeResult, error) {
	if !validLowerSHA256(canonicalBlobHash) {
		return DocumentDerivedPurgeResult{}, errors.New("document derivative purge requires an exact lowercase SHA-256")
	}
	result := DocumentDerivedPurgeResult{}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := tx.QueryRow(`
			SELECT COUNT(*) FROM document_extraction_heads WHERE canonical_blob_hash = ?`,
			canonicalBlobHash).Scan(&result.HeadsRemoved); err != nil {
			return fmt.Errorf("count document heads before purge: %w", err)
		}
		deleted, err := tx.Exec(`DELETE FROM document_extractions WHERE canonical_blob_hash = ?`,
			canonicalBlobHash)
		if err != nil {
			return fmt.Errorf("purge document derivatives: %w", err)
		}
		rows, err := deleted.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document derivative purge result: %w", err)
		}
		result.ExtractionsRemoved = int(rows)
		if result.ExtractionsRemoved > 0 {
			return bumpDocumentIndexRevision(tx)
		}
		return nil
	})
	return result, err
}

// ListDocumentAttachmentIDsAfter supplies the bounded bootstrap scan used to
// reconcile archives created before document indexing was enabled. Callers
// advance by the last returned ID and may safely repeat a page.
func (s *Store) ListDocumentAttachmentIDsAfter(
	ctx context.Context,
	afterID int64,
	limit int,
) ([]int64, error) {
	if afterID < 0 || limit <= 0 || limit > 10_000 {
		return nil, errors.New("document attachment scan has invalid bounds")
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT id FROM attachments WHERE id > ? ORDER BY id LIMIT ?`), afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list document attachment scan page: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan document attachment ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate document attachment IDs: %w", err)
	}
	return ids, nil
}

// ReconcileMissingDocumentOccurrences removes catalog rows whose attachment
// disappeared through direct SQL or cascade deletion. Event replay usually
// removes them by old attachment ID; this bounded anti-join is the periodic
// correctness backstop for missed events and legacy catalog state.
func (s *Store) ReconcileMissingDocumentOccurrences(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 10_000 {
		return 0, errors.New("missing document occurrence scan has invalid bounds")
	}
	removed := 0
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		result, err := q.Exec(`
			DELETE FROM document_occurrences
			WHERE occurrence_key IN (
				SELECT o.occurrence_key
				FROM document_occurrences o
				LEFT JOIN attachments a ON a.id = o.attachment_id
				WHERE a.id IS NULL
				ORDER BY o.occurrence_key
				LIMIT ?
			)`, limit)
		if err != nil {
			return fmt.Errorf("remove missing document occurrences: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read missing document occurrence count: %w", err)
		}
		removed = int(rows)
		// Each removed row advances the revision through the occurrence delete
		// trigger. That same trigger also covers foreign-key cascades.
		return nil
	})
	return removed, err
}

// ListPendingDocumentExtractions returns one representative occurrence per
// canonical blob. It serves only an enabled profile with exact recorded
// consent and excludes owners with a ready head, terminal decision, or active
// retry delay. Every returned row is rechecked again by Claim and Publish.
func (s *Store) ListPendingDocumentExtractions(
	ctx context.Context,
	profileID string,
	extractionInputKey string,
	allowedMediaTypes []string,
	limit int,
) ([]DocumentExtractionCandidate, error) {
	return s.ListDocumentExtractionCandidates(
		ctx, profileID, extractionInputKey, allowedMediaTypes, nil, nil, limit,
	)
}

// ListDocumentExtractionCandidates returns one representative occurrence per
// canonical blob. A rebuild scan is restricted to its durable target snapshot
// and includes current heads until this exact rebuild has resolved each owner.
func (s *Store) ListDocumentExtractionCandidates(
	ctx context.Context,
	profileID string,
	extractionInputKey string,
	allowedMediaTypes []string,
	allowedMessageTypes []string,
	rebuild *DocumentExtractionRebuild,
	limit int,
) ([]DocumentExtractionCandidate, error) {
	if profileID == "" || extractionInputKey == "" || limit <= 0 || limit > 10_000 {
		return nil, errors.New("pending document extraction scan has invalid bounds")
	}
	outerScopeSQL, outerScopeArgs, err := documentOccurrenceScopeSQL(
		"o", "m", allowedMediaTypes, allowedMessageTypes,
	)
	if err != nil {
		return nil, err
	}
	innerScopeSQL, innerScopeArgs, err := documentOccurrenceScopeSQL(
		"o2", "m2", allowedMediaTypes, allowedMessageTypes,
	)
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(outerScopeArgs)+len(innerScopeArgs)+5)
	args = append(args, profileID)
	args = append(args, outerScopeArgs...)
	args = append(args, innerScopeArgs...)
	var ownerStateFilter string
	if rebuild == nil {
		ownerStateFilter = `
		  AND NOT EXISTS (
		      SELECT 1 FROM document_extraction_heads h
		      WHERE h.profile_id = p.id
		        AND h.canonical_blob_hash = o.canonical_blob_hash
		        AND h.extraction_input_key = ?
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM document_extractions e
		      WHERE e.profile_id = p.id
		        AND e.canonical_blob_hash = o.canonical_blob_hash
		        AND e.extraction_input_key = ?
		        AND (e.state = 'terminal' OR
		             (e.state = 'tombstoned' AND e.next_retry_at > ` + s.dialect.Now() + `))
		  )`
		args = append(args, extractionInputKey, extractionInputKey)
	} else {
		if rebuild.ID == "" || rebuild.ProfileID != profileID ||
			rebuild.ExtractionInputKey != extractionInputKey || rebuild.State != "building" {
			return nil, errors.New("pending document extraction scan has an invalid rebuild")
		}
		ownerStateFilter = `
		  AND EXISTS (
		      SELECT 1 FROM document_extraction_rebuild_targets t
		      WHERE t.rebuild_id = ? AND t.canonical_blob_hash = o.canonical_blob_hash
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM document_extractions e
		      WHERE e.rebuild_id = ? AND e.canonical_blob_hash = o.canonical_blob_hash
		        AND (e.state IN ('ready', 'terminal') OR
		             (e.state = 'tombstoned' AND e.next_retry_at > ` + s.dialect.Now() + `))
		  )`
		args = append(args, rebuild.ID, rebuild.ID)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT o.attachment_id, o.canonical_blob_hash, COALESCE(o.mime_type, ''),
		       COALESCE(a.size, 0), COALESCE(m.message_type, ''), o.source_sequence
		FROM document_occurrences o
		JOIN attachments a ON a.id = o.attachment_id
		JOIN messages m ON m.id = o.message_id
		JOIN document_extraction_profiles p ON p.id = ?
		JOIN document_provider_consents c ON c.profile_id = p.id
		WHERE p.enabled = TRUE AND p.retired_at IS NULL
		  AND c.profile_fingerprint = p.fingerprint
		  AND c.retention_posture = p.retention_posture
		  AND c.training_posture = p.training_posture
		  AND `+outerScopeSQL+`
		  AND o.occurrence_key = (
		      SELECT MIN(o2.occurrence_key) FROM document_occurrences o2
		      JOIN messages m2 ON m2.id = o2.message_id
		      WHERE o2.canonical_blob_hash = o.canonical_blob_hash
		        AND `+innerScopeSQL+`
		  )
		`+ownerStateFilter+`
		ORDER BY o.occurrence_key
		LIMIT ?`), args...)
	if err != nil {
		return nil, fmt.Errorf("list pending document extractions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	candidates := make([]DocumentExtractionCandidate, 0, limit)
	for rows.Next() {
		var candidate DocumentExtractionCandidate
		if err := rows.Scan(&candidate.AttachmentID, &candidate.CanonicalBlobHash,
			&candidate.MIMEType, &candidate.Size, &candidate.MessageType,
			&candidate.SourceSequence); err != nil {
			return nil, fmt.Errorf("scan pending document extraction: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending document extractions: %w", err)
	}
	return candidates, nil
}

func documentOccurrenceScopeSQL(
	occurrenceAlias string,
	messageAlias string,
	allowedMediaTypes []string,
	allowedMessageTypes []string,
) (string, []any, error) {
	mediaScopeSQL, args, err := documentOccurrenceMediaScopeSQL(
		occurrenceAlias, messageAlias, allowedMediaTypes, allowedMessageTypes,
	)
	if err != nil {
		return "", nil, err
	}
	return occurrenceAlias + ".attachment_role = 'standalone' AND " + mediaScopeSQL, args, nil
}

func documentOccurrenceMediaScopeSQL(
	occurrenceAlias string,
	messageAlias string,
	allowedMediaTypes []string,
	allowedMessageTypes []string,
) (string, []any, error) {
	if len(allowedMediaTypes) == 0 {
		return "", nil, errors.New("document occurrence scope requires media types")
	}
	mediaPlaceholders := make([]string, len(allowedMediaTypes))
	args := make([]any, 0, len(allowedMediaTypes)+len(allowedMessageTypes))
	for index, mediaType := range allowedMediaTypes {
		if mediaType == "" {
			return "", nil, errors.New("document occurrence scope has an empty media type")
		}
		mediaPlaceholders[index] = "?"
		args = append(args, mediaType)
	}
	conditions := []string{
		occurrenceAlias + ".mime_type IN (" + strings.Join(mediaPlaceholders, ",") + ")",
		LiveMessagesWhere(messageAlias, true),
	}
	if len(allowedMessageTypes) > 0 {
		messagePlaceholders := make([]string, len(allowedMessageTypes))
		for index, messageType := range allowedMessageTypes {
			if messageType == "" {
				return "", nil, errors.New("document occurrence scope has an empty message type")
			}
			messagePlaceholders[index] = "?"
			args = append(args, messageType)
		}
		conditions = append(conditions,
			messageAlias+".message_type IN ("+strings.Join(messagePlaceholders, ",")+")")
	}
	return strings.Join(conditions, " AND "), args, nil
}

func upsertDocumentOccurrenceTx(
	tx *loggedTx, occurrence DocumentOccurrence,
) (bool, *DocumentOccurrence, error) {
	var existing DocumentOccurrence
	err := tx.QueryRow(`
			SELECT occurrence_key, attachment_id, message_id, source_id,
			       COALESCE(source_part_key, ''), stable_source_part,
			       canonical_blob_hash, COALESCE(filename, ''), COALESCE(mime_type, ''),
			       attachment_role, role_source, source_sequence
			FROM document_occurrences WHERE occurrence_key = ?`, occurrence.OccurrenceKey).Scan(
		&existing.OccurrenceKey, &existing.AttachmentID, &existing.MessageID,
		&existing.SourceID, &existing.SourcePartKey, &existing.StableSourcePart,
		&existing.CanonicalBlobHash, &existing.Filename, &existing.MIMEType,
		&existing.AttachmentRole, &existing.RoleSource, &existing.SourceSequence,
	)
	if err == nil {
		existingSequence := existing.SourceSequence
		existing.SourceSequence = occurrence.SourceSequence
		if existing == occurrence {
			if existingSequence >= occurrence.SourceSequence {
				return false, nil, nil
			}
			if _, err := tx.Exec(`
					UPDATE document_occurrences
					SET source_sequence = ?, reconciled_at = CURRENT_TIMESTAMP
					WHERE occurrence_key = ? AND source_sequence = ?`,
				occurrence.SourceSequence, occurrence.OccurrenceKey, existingSequence,
			); err != nil {
				return false, nil, fmt.Errorf("advance document occurrence source sequence: %w", err)
			}
			return false, nil, nil
		}
		existing.SourceSequence = existingSequence
	}
	if err == nil && existing.SourceSequence > occurrence.SourceSequence {
		return false, nil, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, nil, fmt.Errorf("read document occurrence: %w", err)
	}
	newOccurrence := errors.Is(err, sql.ErrNoRows)
	attachmentOccurrence, attachmentFound, err := documentOccurrenceByAttachmentTx(
		tx, occurrence.AttachmentID,
	)
	if err != nil {
		return false, nil, err
	}
	if attachmentFound && attachmentOccurrence.SourceSequence > occurrence.SourceSequence {
		return false, nil, nil
	}
	var replaced *DocumentOccurrence
	if attachmentFound && attachmentOccurrence.OccurrenceKey != occurrence.OccurrenceKey {
		replaced = &attachmentOccurrence
	} else if !newOccurrence && documentOccurrenceEvidenceChanged(existing, occurrence) {
		replaced = &existing
	}
	removedResult, err := tx.Exec(`
			DELETE FROM document_occurrences
			WHERE attachment_id = ? AND occurrence_key != ? AND source_sequence <= ?`,
		occurrence.AttachmentID, occurrence.OccurrenceKey, occurrence.SourceSequence,
	)
	if err != nil {
		return false, nil, fmt.Errorf("remove replaced document occurrence: %w", err)
	}
	removed, err := removedResult.RowsAffected()
	if err != nil {
		return false, nil, fmt.Errorf("read replaced document occurrence count: %w", err)
	}
	result, err := tx.Exec(`
			INSERT INTO document_occurrences
				(occurrence_key, attachment_id, message_id, source_id,
				 source_part_key, stable_source_part, canonical_blob_hash,
				 filename, mime_type, attachment_role, role_source, source_sequence)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (occurrence_key) DO UPDATE SET
				attachment_id = EXCLUDED.attachment_id,
				message_id = EXCLUDED.message_id,
				source_id = EXCLUDED.source_id,
				source_part_key = EXCLUDED.source_part_key,
				stable_source_part = EXCLUDED.stable_source_part,
				canonical_blob_hash = EXCLUDED.canonical_blob_hash,
				filename = EXCLUDED.filename,
				mime_type = EXCLUDED.mime_type,
				attachment_role = EXCLUDED.attachment_role,
				role_source = EXCLUDED.role_source,
				source_sequence = EXCLUDED.source_sequence,
				reconciled_at = CURRENT_TIMESTAMP
			WHERE document_occurrences.source_sequence <= EXCLUDED.source_sequence`,
		occurrence.OccurrenceKey, occurrence.AttachmentID, occurrence.MessageID,
		occurrence.SourceID, nullIfEmpty(occurrence.SourcePartKey),
		occurrence.StableSourcePart, occurrence.CanonicalBlobHash,
		nullIfEmpty(occurrence.Filename), nullIfEmpty(occurrence.MIMEType),
		occurrence.AttachmentRole, occurrence.RoleSource, occurrence.SourceSequence,
	)
	if err != nil {
		return false, nil, fmt.Errorf("upsert document occurrence: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, nil, fmt.Errorf("read document occurrence upsert count: %w", err)
	}
	if changed == 0 {
		return false, nil, nil
	}
	if removed > 0 {
		// The delete trigger already advanced the revision for this atomic
		// replacement. One invalidation is sufficient for the new row too.
		return true, replaced, nil
	}
	if err := bumpDocumentIndexRevision(tx); err != nil {
		return false, nil, err
	}
	return newOccurrence || replaced != nil, replaced, nil
}

func documentOccurrenceByAttachmentTx(
	tx *loggedTx, attachmentID int64,
) (DocumentOccurrence, bool, error) {
	var occurrence DocumentOccurrence
	err := tx.QueryRow(`
		SELECT occurrence_key, attachment_id, message_id, source_id,
		       COALESCE(source_part_key, ''), stable_source_part,
		       canonical_blob_hash, COALESCE(filename, ''), COALESCE(mime_type, ''),
		       attachment_role, role_source, source_sequence
		FROM document_occurrences WHERE attachment_id = ?`, attachmentID).Scan(
		&occurrence.OccurrenceKey, &occurrence.AttachmentID, &occurrence.MessageID,
		&occurrence.SourceID, &occurrence.SourcePartKey, &occurrence.StableSourcePart,
		&occurrence.CanonicalBlobHash, &occurrence.Filename, &occurrence.MIMEType,
		&occurrence.AttachmentRole, &occurrence.RoleSource, &occurrence.SourceSequence,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DocumentOccurrence{}, false, nil
	}
	if err != nil {
		return DocumentOccurrence{}, false,
			fmt.Errorf("read document occurrence attachment: %w", err)
	}
	return occurrence, true, nil
}

func documentOccurrenceEvidenceChanged(old, current DocumentOccurrence) bool {
	return old.OccurrenceKey != current.OccurrenceKey ||
		old.AttachmentID != current.AttachmentID ||
		old.MessageID != current.MessageID ||
		old.SourceID != current.SourceID ||
		old.CanonicalBlobHash != current.CanonicalBlobHash ||
		old.MIMEType != current.MIMEType ||
		old.AttachmentRole != current.AttachmentRole ||
		old.RoleSource != current.RoleSource
}

func removeDocumentOccurrenceTx(
	tx *loggedTx, attachmentID, sourceSequence int64,
) (bool, DocumentOccurrence, error) {
	occurrence, found, err := documentOccurrenceByAttachmentTx(tx, attachmentID)
	if err != nil || !found || occurrence.SourceSequence > sourceSequence {
		return false, DocumentOccurrence{}, err
	}
	result, err := tx.Exec(`
			DELETE FROM document_occurrences
			WHERE attachment_id = ? AND source_sequence <= ?`, attachmentID, sourceSequence)
	if err != nil {
		return false, DocumentOccurrence{}, fmt.Errorf("remove document occurrence: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, DocumentOccurrence{}, fmt.Errorf("read removed document occurrence count: %w", err)
	}
	// The database trigger advances the revision when a row is removed,
	// including when foreign-key cascades bypass this method.
	if removed == 0 {
		return false, DocumentOccurrence{}, nil
	}
	return true, occurrence, nil
}

func bumpDocumentIndexRevision(q querier) error {
	if _, err := q.Exec(`
		UPDATE document_index_state
		SET revision = revision + 1, updated_at = CURRENT_TIMESTAMP
		WHERE singleton = 1`); err != nil {
		return fmt.Errorf("advance document index revision: %w", err)
	}
	return nil
}

func eligibleDocumentFile(file FileMetadata) bool {
	if file.AttachmentRole != AttachmentRoleStandalone || file.StoragePath == "" ||
		file.Size <= 0 || !validLowerSHA256(file.ContentHash) {
		return false
	}
	switch file.RoleSource {
	case AttachmentRoleSourceMIMEDisposition, AttachmentRoleSourceProviderExplicit,
		AttachmentRoleSourceImporterSemantics, AttachmentRoleSourceRawMIMERepair:
		return true
	default:
		return false
	}
}

func documentOccurrenceKey(messageID int64, sourcePartKey string, attachmentID int64) string {
	identity := fmt.Sprintf("unstable\x00%d\x00%d", messageID, attachmentID)
	if sourcePartKey != "" {
		identity = fmt.Sprintf("stable\x00%d\x00%s", messageID, sourcePartKey)
	}
	digest := sha256.Sum256([]byte(identity))
	return "dococc_" + hex.EncodeToString(digest[:])
}

func validateDocumentProfile(profile DocumentExtractionProfile) ([]byte, []byte, error) {
	if profile.ID == "" || len(profile.ID) > 200 || !validLowerSHA256(profile.Fingerprint) ||
		profile.Provider == "" || profile.Endpoint == "" || profile.Region == "" || profile.Model == "" ||
		profile.RetentionPosture == "" || profile.TrainingPosture == "" ||
		profile.RetentionPosture == "unknown" || profile.TrainingPosture == "unknown" {
		return nil, nil, errors.New("document extraction profile is incomplete")
	}
	allowed := slices.Clone(profile.AllowedMediaTypes)
	slices.Sort(allowed)
	allowed = slices.Compact(allowed)
	if len(allowed) == 0 || slices.Contains(allowed, "") {
		return nil, nil, errors.New("document extraction profile requires a media allowlist")
	}
	allowedJSON, err := json.Marshal(allowed)
	if err != nil {
		return nil, nil, fmt.Errorf("encode document extraction profile allowlist: %w", err)
	}
	policyJSON, err := compactJSON(profile.PolicyJSON)
	if err != nil {
		return nil, nil, fmt.Errorf("validate document extraction profile policy: %w", err)
	}
	return allowedJSON, policyJSON, nil
}

func compactJSON(value []byte) ([]byte, error) {
	if len(value) == 0 || !json.Valid(value) {
		return nil, errors.New("value is not valid JSON")
	}
	var output bytes.Buffer
	if err := json.Compact(&output, value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func equalJSON(first, second []byte) bool {
	decode := func(value []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return nil, err
		}
		return decoded, nil
	}
	firstValue, firstErr := decode(first)
	secondValue, secondErr := decode(second)
	return firstErr == nil && secondErr == nil && reflect.DeepEqual(firstValue, secondValue)
}

func validLowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
