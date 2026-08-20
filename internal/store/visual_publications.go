package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type VisualGenerationState string

const (
	VisualGenerationBuilding VisualGenerationState = "building"
	VisualGenerationActive   VisualGenerationState = "active"
	VisualGenerationRetired  VisualGenerationState = "retired"
)

type VisualGenerationSpec struct {
	Fingerprint string
	Model       string
	Dimension   int
}

type VisualGeneration struct {
	ID          int64                 `json:"id"`
	Fingerprint string                `json:"fingerprint"`
	Model       string                `json:"model"`
	Dimension   int                   `json:"dimension"`
	State       VisualGenerationState `json:"state"`
	SourceFence int64                 `json:"source_fence"`
	Consented   bool                  `json:"consented"`
	// ConsentPolicyFingerprint is the docbank Voyage policy identity the
	// operator consented to; empty when consent was never recorded.
	ConsentPolicyFingerprint string `json:"consent_policy_fingerprint,omitempty"`
}

type VisualOwner struct {
	MessageID     int64
	BlobHash      string
	MediaInputKey string
}

type VisualClaimRequest struct {
	GenerationID     int64
	Owner            VisualOwner
	ProposedRevision string
	LeaseOwner       string
	Now              time.Time
	LeaseDuration    time.Duration
	SourceFence      int64
	// ExpectedContentStamp, when non-nil, is recorded as the claim's
	// content stamp instead of the live value: the caller read it together
	// with the context snapshot the document was assembled from, so an edit
	// racing the snapshot fails the commit-time CAS rather than being
	// absorbed. The empty string is a legitimate never-edited stamp, hence
	// the pointer.
	ExpectedContentStamp *string
}

type VisualWorkClaim struct {
	GenerationID     int64
	Owner            VisualOwner
	ProposedRevision string
	LeaseOwner       string
	LeaseExpiresAt   time.Time
	FencingToken     int64
	SourceFence      int64
	// ContentStamp is the owning message's content_changed_at stamp at claim
	// time; commit refuses when the live stamp differs.
	ContentStamp string
}

type PreparedVisualPublication struct {
	Claim                      VisualWorkClaim
	RepresentativeAttachmentID int64
	Role                       AttachmentRole
	RoleSource                 AttachmentRoleSource
}

type VisualOutcome struct {
	Kind   string
	Reason string
}

type VisualPublicationState string

const (
	VisualPublicationCurrent    VisualPublicationState = "current"
	VisualPublicationStale      VisualPublicationState = "stale"
	VisualPublicationTombstoned VisualPublicationState = "tombstoned"
)

type VisualPublication struct {
	GenerationID               int64
	Owner                      VisualOwner
	PublishedRevision          string
	PreparedRevision           string
	SourceFence                int64
	RepresentativeAttachmentID int64
	Role                       AttachmentRole
	RoleSource                 AttachmentRoleSource
	CurrentVectorToken         string
	PendingVectorToken         string
	State                      VisualPublicationState
	OutcomeKind                string
	OutcomeReason              string
}

type VisualSearchOccurrence struct {
	VectorToken     string
	GenerationID    int64
	AttachmentID    int64
	MessageID       int64
	ConversationID  int64
	SourceID        int64
	SourceMessageID string
	BlobHash        string
	Filename        string
	MIMEType        string
	Size            int64
	SentAt          time.Time
}

// ResolveVisualSearchOccurrence resolves a winning vector token to the exact
// representative attachment occurrence that was validated at publication.
func (s *Store) ResolveVisualSearchOccurrence(ctx context.Context, generationID int64, token string) (VisualSearchOccurrence, error) {
	var occurrence VisualSearchOccurrence
	var sentAt sql.NullTime
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT vp.current_vector_token, vp.generation_id, a.id, m.id,
		       COALESCE(m.conversation_id, 0), m.source_id,
		       COALESCE(m.source_message_id, ''), vp.blob_hash,
		       COALESCE(a.filename, ''), COALESCE(a.mime_type, ''),
		       COALESCE(a.size, 0), COALESCE(m.sent_at, m.received_at, m.internal_date)
		FROM visual_publications vp
		JOIN visual_generations vg ON vg.id = vp.generation_id AND vg.state = 'active'
		JOIN messages m ON m.id = vp.message_id
		JOIN attachments a ON a.id = vp.representative_attachment_id
		WHERE vp.generation_id = ? AND vp.current_vector_token = ?
		  AND vp.state = 'current' AND `+LiveMessagesWhere("m", true)+`
		  AND a.message_id = vp.message_id AND a.attachment_role = 'standalone'
	`), generationID, token).Scan(&occurrence.VectorToken, &occurrence.GenerationID,
		&occurrence.AttachmentID, &occurrence.MessageID, &occurrence.ConversationID,
		&occurrence.SourceID, &occurrence.SourceMessageID, &occurrence.BlobHash,
		&occurrence.Filename, &occurrence.MIMEType, &occurrence.Size, &sentAt)
	if err != nil {
		return VisualSearchOccurrence{}, fmt.Errorf("resolve visual search occurrence: %w", err)
	}
	occurrence.SentAt = sentAt.Time
	return occurrence, nil
}

// ListStaleVisualMessageIDs returns distinct message IDs whose publications
// were marked stale in place by the source-invalidation triggers. Message
// subject and body changes do not enter the shared attachment journal, so the
// reconciler sweeps these between journal pages.
func (s *Store) ListStaleVisualMessageIDs(ctx context.Context, generationID int64, limit int) ([]int64, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("stale visual message limit must be between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT DISTINCT message_id FROM visual_publications
		WHERE generation_id = ? AND state = 'stale' AND outcome_kind IS NULL
		ORDER BY message_id LIMIT ?`), generationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list stale visual messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanInt64Rows(rows)
}

func (s *Store) ListObsoleteVisualTokens(ctx context.Context, generationID int64, limit int) ([]string, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("obsolete visual token limit must be between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT current_vector_token FROM visual_publications
		WHERE generation_id = ? AND state <> 'current' AND current_vector_token IS NOT NULL
		  AND (state = 'tombstoned' OR outcome_kind IS NOT NULL)
		UNION
		SELECT vector_token FROM visual_obsolete_tokens
		WHERE generation_id = ?
		ORDER BY 1 LIMIT ?`), generationID, generationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list obsolete visual tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var tokens []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *Store) ListVisualGenerationTokens(ctx context.Context, generationID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT current_vector_token FROM visual_publications
		WHERE generation_id = ? AND current_vector_token IS NOT NULL
		UNION SELECT pending_vector_token FROM visual_publications
		WHERE generation_id = ? AND pending_vector_token IS NOT NULL
		UNION SELECT vector_token FROM visual_obsolete_tokens
		WHERE generation_id = ?`), generationID, generationID, generationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tokens []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *Store) ClearObsoleteVisualToken(ctx context.Context, generationID int64, token string) error {
	if _, err := s.db.ExecContext(ctx, s.dialect.Rebind(`
		UPDATE visual_publications SET current_vector_token = NULL, updated_at = `+s.dialect.Now()+`
		WHERE generation_id = ? AND state <> 'current' AND current_vector_token = ?`), generationID, token); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, s.dialect.Rebind(`
		DELETE FROM visual_obsolete_tokens
		WHERE generation_id = ? AND vector_token = ?`), generationID, token)
	return err
}

// ClearVisualOutcome removes an owner's durable outcome so an explicit
// operator retry re-evaluates it: reconciliation deliberately treats a
// matching terminal outcome as converged, which would otherwise make the
// retry a silent no-op.
func (s *Store) ClearVisualOutcome(ctx context.Context, generationID int64, owner VisualOwner) error {
	_, err := s.db.ExecContext(ctx, s.dialect.Rebind(`
		UPDATE visual_publications
		SET outcome_kind = NULL, outcome_reason = NULL, updated_at = `+s.dialect.Now()+`
		WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
		  AND media_input_key = ? AND outcome_kind IS NOT NULL`),
		generationID, owner.MessageID, owner.BlobHash, owner.MediaInputKey)
	if err != nil {
		return fmt.Errorf("clear visual outcome: %w", err)
	}
	return nil
}

// ParkObsoleteVisualToken records a backend vector token whose inline delete
// failed so the obsolete-token sweep retries it. The ledger is multi-row, so
// parking never evicts another token still awaiting cleanup.
func (s *Store) ParkObsoleteVisualToken(
	ctx context.Context,
	generationID int64,
	owner VisualOwner,
	token string,
) error {
	_ = owner
	if strings.TrimSpace(token) == "" {
		return errors.New("visual token to park is required")
	}
	_, err := s.db.ExecContext(ctx, s.dialect.Rebind(`
		INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
		VALUES (?, ?)
		ON CONFLICT (generation_id, vector_token) DO NOTHING`), generationID, token)
	if err != nil {
		return fmt.Errorf("park obsolete visual token: %w", err)
	}
	return nil
}

var (
	ErrVisualClaimLost = errors.New("visual work claim was lost")
	// ErrVisualRetiredTokensRemain refuses to restart a retired generation
	// whose ledger still references backend vectors; the caller must delete
	// those tokens and purge the ledger first.
	ErrVisualRetiredTokensRemain = errors.New("retired visual generation still references backend vectors")
	ErrVisualSourceChanged       = errors.New("visual publication source changed")
	ErrVisualOwnerMissing        = errors.New("visual owner has no qualifying live attachment")
)

func (s *Store) EnsureVisualGeneration(
	ctx context.Context,
	spec VisualGenerationSpec,
) (VisualGeneration, error) {
	if strings.TrimSpace(spec.Fingerprint) == "" || strings.TrimSpace(spec.Model) == "" {
		return VisualGeneration{}, errors.New("visual generation fingerprint and model are required")
	}
	if spec.Dimension != 1024 {
		return VisualGeneration{}, errors.New("visual generation dimension must be 1024")
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO visual_generations (fingerprint, model, dimension, state)
		VALUES (?, ?, ?, 'building')
		ON CONFLICT (fingerprint) DO NOTHING
	`, spec.Fingerprint, spec.Model, spec.Dimension); err != nil {
		return VisualGeneration{}, fmt.Errorf("ensure visual generation: %w", err)
	}
	var generation VisualGeneration
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, fingerprint, model, dimension, state, source_fence, consented_at IS NOT NULL,
		       COALESCE(consent_policy_fingerprint, '')
		FROM visual_generations WHERE fingerprint = ?
	`, spec.Fingerprint).Scan(
		&generation.ID, &generation.Fingerprint, &generation.Model,
		&generation.Dimension, &generation.State, &generation.SourceFence, &generation.Consented,
		&generation.ConsentPolicyFingerprint,
	); err != nil {
		return VisualGeneration{}, fmt.Errorf("read visual generation: %w", err)
	}
	if generation.Model != spec.Model || generation.Dimension != spec.Dimension {
		return VisualGeneration{}, errors.New("visual generation fingerprint already has different immutable settings")
	}
	if generation.State == VisualGenerationRetired {
		if err := s.withTxContext(ctx, func(tx *loggedTx) error {
			q := boundQuerier{ctx: ctx, q: tx}
			// Deleting the ledger while rows still reference backend vectors
			// would make those vectors untraceable if the retirement's token
			// cleanup crashed or failed. The serve runtime deletes the
			// retired generation's backend tokens and purges the ledger
			// before restarting; refuse to restart over live references.
			var remaining int64
			if err := q.QueryRow(`
				SELECT (SELECT COUNT(*) FROM visual_publications
				        WHERE generation_id = ?
				          AND (current_vector_token IS NOT NULL
				               OR pending_vector_token IS NOT NULL))
				     + (SELECT COUNT(*) FROM visual_obsolete_tokens
				        WHERE generation_id = ?)`, generation.ID, generation.ID).Scan(&remaining); err != nil {
				return err
			}
			if remaining > 0 {
				return ErrVisualRetiredTokensRemain
			}
			if _, err := q.Exec(`DELETE FROM visual_work_claims WHERE generation_id = ?`, generation.ID); err != nil {
				return err
			}
			if _, err := q.Exec(`DELETE FROM visual_publications WHERE generation_id = ?`, generation.ID); err != nil {
				return err
			}
			_, err := q.Exec(`UPDATE visual_generations
				SET state = 'building', source_fence = 0, consented_at = NULL,
				    consent_policy_fingerprint = NULL, activated_at = NULL
				WHERE id = ? AND state = 'retired'`, generation.ID)
			return err
		}); err != nil {
			return VisualGeneration{}, fmt.Errorf("restart retired visual generation: %w", err)
		}
		generation.State = VisualGenerationBuilding
		generation.SourceFence = 0
		generation.Consented = false
	}
	return generation, nil
}

// ConsentVisualGeneration records explicit hosted-processing consent bound
// to the docbank policy fingerprint of the manifest in force. Consent for a
// different fingerprint does not carry over.
func (s *Store) ConsentVisualGeneration(ctx context.Context, generationID int64, policyFingerprint string) error {
	if policyFingerprint == "" {
		return errors.New("visual consent requires the policy fingerprint of a validated capability manifest")
	}
	result, err := s.db.ExecContext(ctx, s.dialect.Rebind(`
		UPDATE visual_generations
		SET consented_at = `+s.dialect.Now()+`, consent_policy_fingerprint = ?
		WHERE id = ? AND state IN ('building', 'active')`), policyFingerprint, generationID)
	if err != nil {
		return fmt.Errorf("consent visual generation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return errors.New("visual generation is not available for consent")
	}
	return nil
}

func (s *Store) ClaimVisualWork(
	ctx context.Context,
	request VisualClaimRequest,
) (VisualWorkClaim, bool, error) {
	if err := validateVisualClaimRequest(request); err != nil {
		return VisualWorkClaim{}, false, err
	}
	expiresAt := request.Now.Add(request.LeaseDuration)
	var claim VisualWorkClaim
	claim.GenerationID = request.GenerationID
	claim.Owner = request.Owner
	claim.ProposedRevision = request.ProposedRevision
	stampExpr := visualContentStampExpr
	stampArgs := []any{}
	if request.ExpectedContentStamp != nil {
		stampExpr = "?"
		stampArgs = append(stampArgs, *request.ExpectedContentStamp)
	}
	args := []any{request.GenerationID, request.Owner.MessageID, request.Owner.BlobHash,
		request.Owner.MediaInputKey, request.ProposedRevision, request.LeaseOwner,
		expiresAt, request.SourceFence}
	args = append(args, stampArgs...)
	args = append(args, request.Owner.MessageID, request.Now)
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO visual_work_claims
			(generation_id, message_id, blob_hash, media_input_key,
			 proposed_revision, lease_owner, lease_expires_at, fencing_token,
			 source_fence, claimed_content_stamp)
		SELECT ?, ?, ?, ?, ?, ?, ?, 1, ?, `+stampExpr+`
		FROM messages WHERE id = ?
		ON CONFLICT (generation_id, message_id, blob_hash, media_input_key, proposed_revision)
		DO UPDATE SET
			lease_owner = excluded.lease_owner,
			lease_expires_at = excluded.lease_expires_at,
			fencing_token = visual_work_claims.fencing_token + 1,
			source_fence = excluded.source_fence,
			claimed_content_stamp = excluded.claimed_content_stamp,
			updated_at = `+s.dialect.Now()+`
		WHERE visual_work_claims.lease_expires_at <= ?
		RETURNING lease_owner, lease_expires_at, fencing_token, source_fence, claimed_content_stamp
	`, args...).Scan(
		&claim.LeaseOwner, &claim.LeaseExpiresAt, &claim.FencingToken, &claim.SourceFence,
		&claim.ContentStamp,
	)
	if err == nil {
		return claim, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return VisualWorkClaim{}, false, fmt.Errorf("claim visual work: %w", err)
	}
	if err := s.scanVisualClaim(ctx, request.GenerationID, request.Owner, request.ProposedRevision, &claim); err != nil {
		return VisualWorkClaim{}, false, err
	}
	return claim, false, nil
}

func (s *Store) RenewVisualWork(
	ctx context.Context,
	claim VisualWorkClaim,
	now time.Time,
	leaseDuration time.Duration,
) error {
	if leaseDuration <= 0 {
		return errors.New("visual work lease duration must be positive")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE visual_work_claims
		SET lease_expires_at = ?, updated_at = `+s.dialect.Now()+`
		WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
		  AND media_input_key = ? AND proposed_revision = ?
		  AND lease_owner = ? AND fencing_token = ? AND lease_expires_at > ?
	`, now.Add(leaseDuration), claim.GenerationID, claim.Owner.MessageID,
		claim.Owner.BlobHash, claim.Owner.MediaInputKey, claim.ProposedRevision,
		claim.LeaseOwner, claim.FencingToken, now)
	return visualClaimMutationResult(result, err, "renew")
}

func (s *Store) ReleaseVisualWork(ctx context.Context, claim VisualWorkClaim) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE visual_work_claims
		SET lease_expires_at = '1970-01-01 00:00:00+00:00',
		    updated_at = `+s.dialect.Now()+`
		WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
		  AND media_input_key = ? AND proposed_revision = ?
		  AND lease_owner = ? AND fencing_token = ?
	`, claim.GenerationID, claim.Owner.MessageID, claim.Owner.BlobHash,
		claim.Owner.MediaInputKey, claim.ProposedRevision,
		claim.LeaseOwner, claim.FencingToken)
	return visualClaimMutationResult(result, err, "release")
}

func (s *Store) PrepareVisualPublication(
	ctx context.Context,
	prepared PreparedVisualPublication,
) (string, error) {
	if !prepared.Role.valid() || !prepared.RoleSource.valid() ||
		prepared.Role != AttachmentRoleStandalone || !authoritativeVisualRoleSource(prepared.RoleSource) {
		return "", errors.New("prepared visual publication requires authoritative standalone role")
	}
	token, err := newVisualVectorToken()
	if err != nil {
		return "", err
	}
	sourceChanged := false
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := verifyVisualClaim(q, prepared.Claim, time.Now().UTC()); err != nil {
			return err
		}
		if err := verifyVisualOwner(q, prepared.Claim.Owner); err != nil {
			return err
		}
		// The provider request follows this reservation; repeat the journal
		// and context freshness checks here so a source that moved between
		// claim and prepare skips the paid upload instead of being caught
		// only after it.
		changed, err := visualOwnerChangedAfter(q, prepared.Claim.Owner, prepared.Claim.SourceFence)
		if err != nil {
			return err
		}
		if !changed {
			changed, err = visualOwnerContextChanged(q, prepared.Claim)
			if err != nil {
				return err
			}
		}
		if changed {
			sourceChanged = true
			return releaseVisualClaim(q, s.dialect, prepared.Claim)
		}
		// A crash between PutUnpublished and commit leaves the previous
		// pending token behind; ledger it before this reservation replaces
		// it so its possible backend vector is swept rather than orphaned.
		if _, err := q.Exec(`
			INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
			SELECT generation_id, pending_vector_token FROM visual_publications
			WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
			  AND media_input_key = ? AND pending_vector_token IS NOT NULL
			ON CONFLICT (generation_id, vector_token) DO NOTHING
		`, prepared.Claim.GenerationID, prepared.Claim.Owner.MessageID,
			prepared.Claim.Owner.BlobHash, prepared.Claim.Owner.MediaInputKey); err != nil {
			return err
		}
		_, err = q.Exec(`
			INSERT INTO visual_publications
				(generation_id, message_id, blob_hash, media_input_key,
				 prepared_revision, source_fence, representative_attachment_id,
				 attachment_role, role_source, pending_vector_token, state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'stale')
			ON CONFLICT (generation_id, message_id, blob_hash, media_input_key)
			DO UPDATE SET
				prepared_revision = excluded.prepared_revision,
				source_fence = excluded.source_fence,
				representative_attachment_id = excluded.representative_attachment_id,
				attachment_role = excluded.attachment_role,
				role_source = excluded.role_source,
				pending_vector_token = excluded.pending_vector_token,
				outcome_kind = NULL,
				outcome_reason = NULL,
				updated_at = `+s.dialect.Now()+`
		`, prepared.Claim.GenerationID, prepared.Claim.Owner.MessageID,
			prepared.Claim.Owner.BlobHash, prepared.Claim.Owner.MediaInputKey,
			prepared.Claim.ProposedRevision, prepared.Claim.SourceFence,
			prepared.RepresentativeAttachmentID, string(prepared.Role),
			string(prepared.RoleSource), token)
		return err
	})
	if err != nil {
		return "", err
	}
	if sourceChanged {
		return "", ErrVisualSourceChanged
	}
	return token, nil
}

func (s *Store) CommitVisualPublication(
	ctx context.Context,
	claim VisualWorkClaim,
	vectorToken string,
) error {
	if vectorToken == "" {
		return errors.New("visual vector token is required")
	}
	sourceChanged := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := verifyVisualClaim(q, claim, time.Now().UTC()); err != nil {
			return err
		}
		changed, err := visualOwnerChangedAfter(q, claim.Owner, claim.SourceFence)
		if err != nil {
			return err
		}
		if !changed {
			changed, err = visualOwnerContextChanged(q, claim)
			if err != nil {
				return err
			}
		}
		if !changed {
			if ownerErr := verifyVisualOwner(q, claim.Owner); ownerErr != nil {
				if errors.Is(ownerErr, ErrVisualOwnerMissing) {
					changed = true
				} else {
					return ownerErr
				}
			}
		}
		if changed {
			sourceChanged = true
			// The discarded pending token already holds a backend vector
			// (PutUnpublished precedes commit), so ledger it for the sweep
			// before clearing the reference.
			if _, err := q.Exec(`
				INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
				SELECT generation_id, pending_vector_token FROM visual_publications
				WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
				  AND media_input_key = ? AND pending_vector_token = ?
				ON CONFLICT (generation_id, vector_token) DO NOTHING
			`, claim.GenerationID, claim.Owner.MessageID, claim.Owner.BlobHash,
				claim.Owner.MediaInputKey, vectorToken); err != nil {
				return err
			}
			if _, err := q.Exec(`
				UPDATE visual_publications
				SET state = 'stale', pending_vector_token = NULL, updated_at = `+s.dialect.Now()+`
				WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
				  AND media_input_key = ? AND pending_vector_token = ?
			`, claim.GenerationID, claim.Owner.MessageID, claim.Owner.BlobHash,
				claim.Owner.MediaInputKey, vectorToken); err != nil {
				return err
			}
			return releaseVisualClaim(q, s.dialect, claim)
		}
		// The replaced current token is recorded in the obsolete-token
		// ledger in the same transaction that drops it, so a failed inline
		// backend delete is retried by the sweep instead of orphaning the
		// vector. The ledger is multi-row: parking one token never evicts
		// another still awaiting cleanup.
		if _, err := q.Exec(`
			INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
			SELECT generation_id, current_vector_token FROM visual_publications
			WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
			  AND media_input_key = ? AND pending_vector_token = ?
			  AND current_vector_token IS NOT NULL AND current_vector_token <> pending_vector_token
			ON CONFLICT (generation_id, vector_token) DO NOTHING
		`, claim.GenerationID, claim.Owner.MessageID, claim.Owner.BlobHash,
			claim.Owner.MediaInputKey, vectorToken); err != nil {
			return err
		}
		result, err := q.Exec(`
			UPDATE visual_publications
			SET published_revision = prepared_revision,
			    prepared_revision = NULL,
			    current_vector_token = pending_vector_token,
			    pending_vector_token = NULL,
			    state = 'current', outcome_kind = NULL, outcome_reason = NULL,
			    updated_at = `+s.dialect.Now()+`
			WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
			  AND media_input_key = ? AND pending_vector_token = ?
		`, claim.GenerationID, claim.Owner.MessageID, claim.Owner.BlobHash,
			claim.Owner.MediaInputKey, vectorToken)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return ErrVisualClaimLost
		}
		return releaseVisualClaim(q, s.dialect, claim)
	})
	if err != nil {
		return err
	}
	if sourceChanged {
		return ErrVisualSourceChanged
	}
	return nil
}

// RestoreVisualPublication re-exposes an existing vector when the exact model
// input revision is unchanged. The source owner and fencing token are checked
// in the same transaction, so metadata-only source changes can advance the
// publication fence without paying for another provider request.
func (s *Store) RestoreVisualPublication(
	ctx context.Context,
	prepared PreparedVisualPublication,
	expectedVectorToken string,
) error {
	if strings.TrimSpace(expectedVectorToken) == "" ||
		prepared.Role != AttachmentRoleStandalone ||
		!authoritativeVisualRoleSource(prepared.RoleSource) {
		return errors.New("restored visual publication requires an existing vector and authoritative standalone role")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := verifyVisualClaim(q, prepared.Claim, time.Now().UTC()); err != nil {
			return err
		}
		if err := verifyVisualOwner(q, prepared.Claim.Owner); err != nil {
			return err
		}
		changed, err := visualOwnerChangedAfter(q, prepared.Claim.Owner, prepared.Claim.SourceFence)
		if err != nil {
			return err
		}
		if changed {
			return ErrVisualSourceChanged
		}
		result, err := q.Exec(`
			UPDATE visual_publications
			SET source_fence = ?, representative_attachment_id = ?,
			    attachment_role = ?, role_source = ?, state = 'current',
			    prepared_revision = NULL, pending_vector_token = NULL,
			    outcome_kind = NULL, outcome_reason = NULL,
			    updated_at = `+s.dialect.Now()+`
			WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
			  AND media_input_key = ? AND published_revision = ?
			  AND current_vector_token = ?
		`, prepared.Claim.SourceFence, prepared.RepresentativeAttachmentID,
			string(prepared.Role), string(prepared.RoleSource),
			prepared.Claim.GenerationID, prepared.Claim.Owner.MessageID,
			prepared.Claim.Owner.BlobHash, prepared.Claim.Owner.MediaInputKey,
			prepared.Claim.ProposedRevision, expectedVectorToken)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return ErrVisualSourceChanged
		}
		return releaseVisualClaim(q, s.dialect, prepared.Claim)
	})
}

func (s *Store) RejectVisualPublication(
	ctx context.Context,
	claim VisualWorkClaim,
	outcome VisualOutcome,
) error {
	if outcome.Kind == "" || outcome.Reason == "" {
		return errors.New("visual outcome kind and reason are required")
	}
	sourceChanged := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := verifyVisualClaim(q, claim, time.Now().UTC()); err != nil {
			return err
		}
		// A rejection recorded for a moved source would mask the change: a
		// stale row with a durable outcome is excluded from the sweep and
		// the stale count, so a context-only edit would never re-evaluate.
		// Apply the same drift checks as CommitVisualPublication and record
		// nothing when the source moved.
		changed, err := visualOwnerChangedAfter(q, claim.Owner, claim.SourceFence)
		if err != nil {
			return err
		}
		if !changed {
			changed, err = visualOwnerContextChanged(q, claim)
			if err != nil {
				return err
			}
		}
		if !changed {
			if ownerErr := verifyVisualOwner(q, claim.Owner); ownerErr != nil {
				if errors.Is(ownerErr, ErrVisualOwnerMissing) {
					changed = true
				} else {
					return ownerErr
				}
			}
		}
		if changed {
			sourceChanged = true
			return releaseVisualClaim(q, s.dialect, claim)
		}
		if _, err := q.Exec(`
			INSERT INTO visual_obsolete_tokens (generation_id, vector_token)
			SELECT generation_id, pending_vector_token FROM visual_publications
			WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
			  AND media_input_key = ? AND pending_vector_token IS NOT NULL
			ON CONFLICT (generation_id, vector_token) DO NOTHING
		`, claim.GenerationID, claim.Owner.MessageID, claim.Owner.BlobHash,
			claim.Owner.MediaInputKey); err != nil {
			return err
		}
		if _, err := q.Exec(`
			INSERT INTO visual_publications
				(generation_id, message_id, blob_hash, media_input_key,
				 prepared_revision, source_fence, attachment_role, role_source,
				 state, outcome_kind, outcome_reason)
			VALUES (?, ?, ?, ?, ?, ?, 'unknown', 'unknown', 'stale', ?, ?)
			ON CONFLICT (generation_id, message_id, blob_hash, media_input_key)
			DO UPDATE SET
				prepared_revision = excluded.prepared_revision,
				source_fence = excluded.source_fence,
				pending_vector_token = NULL,
				state = 'stale',
				outcome_kind = excluded.outcome_kind,
				outcome_reason = excluded.outcome_reason,
				updated_at = `+s.dialect.Now()+`
		`, claim.GenerationID, claim.Owner.MessageID, claim.Owner.BlobHash,
			claim.Owner.MediaInputKey, claim.ProposedRevision, claim.SourceFence,
			outcome.Kind, outcome.Reason); err != nil {
			return err
		}
		return releaseVisualClaim(q, s.dialect, claim)
	})
	if err != nil {
		return err
	}
	if sourceChanged {
		return ErrVisualSourceChanged
	}
	return nil
}

func (s *Store) TombstoneVisualOwner(
	ctx context.Context,
	generationID int64,
	owner VisualOwner,
	sourceFence int64,
) error {
	if err := validateVisualOwner(owner); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO visual_publications
			(generation_id, message_id, blob_hash, media_input_key,
			 source_fence, attachment_role, role_source, state)
		VALUES (?, ?, ?, ?, ?, 'unknown', 'unknown', 'tombstoned')
		ON CONFLICT (generation_id, message_id, blob_hash, media_input_key)
		DO UPDATE SET
			source_fence = excluded.source_fence,
			pending_vector_token = NULL,
			state = 'tombstoned',
			updated_at = `+s.dialect.Now()+`
	`, generationID, owner.MessageID, owner.BlobHash, owner.MediaInputKey, sourceFence)
	return err
}

func (s *Store) GetVisualPublication(
	ctx context.Context,
	generationID int64,
	owner VisualOwner,
) (VisualPublication, error) {
	if err := validateVisualOwner(owner); err != nil {
		return VisualPublication{}, err
	}
	var publication VisualPublication
	publication.GenerationID = generationID
	publication.Owner = owner
	var published, prepared, current, pending, outcomeKind, outcomeReason sql.NullString
	var representative sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT published_revision, prepared_revision, source_fence,
		       representative_attachment_id, attachment_role, role_source,
		       current_vector_token, pending_vector_token, state,
		       outcome_kind, outcome_reason
		FROM visual_publications
		WHERE generation_id = ? AND message_id = ? AND blob_hash = ? AND media_input_key = ?
	`, generationID, owner.MessageID, owner.BlobHash, owner.MediaInputKey).Scan(
		&published, &prepared, &publication.SourceFence, &representative,
		&publication.Role, &publication.RoleSource, &current, &pending,
		&publication.State, &outcomeKind, &outcomeReason,
	)
	if err != nil {
		return VisualPublication{}, err
	}
	publication.PublishedRevision = published.String
	publication.PreparedRevision = prepared.String
	publication.RepresentativeAttachmentID = representative.Int64
	publication.CurrentVectorToken = current.String
	publication.PendingVectorToken = pending.String
	publication.OutcomeKind = outcomeKind.String
	publication.OutcomeReason = outcomeReason.String
	return publication, nil
}

func (s *Store) scanVisualClaim(
	ctx context.Context,
	generationID int64,
	owner VisualOwner,
	revision string,
	claim *VisualWorkClaim,
) error {
	claim.GenerationID = generationID
	claim.Owner = owner
	claim.ProposedRevision = revision
	err := s.db.QueryRowContext(ctx, `
		SELECT lease_owner, lease_expires_at, fencing_token, source_fence, claimed_content_stamp
		FROM visual_work_claims
		WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
		  AND media_input_key = ? AND proposed_revision = ?
	`, generationID, owner.MessageID, owner.BlobHash, owner.MediaInputKey, revision).Scan(
		&claim.LeaseOwner, &claim.LeaseExpiresAt, &claim.FencingToken, &claim.SourceFence,
		&claim.ContentStamp,
	)
	if err != nil {
		return fmt.Errorf("read visual work claim: %w", err)
	}
	return nil
}

func validateVisualClaimRequest(request VisualClaimRequest) error {
	if request.GenerationID <= 0 || request.SourceFence < 0 ||
		strings.TrimSpace(request.ProposedRevision) == "" ||
		strings.TrimSpace(request.LeaseOwner) == "" || request.Now.IsZero() ||
		request.LeaseDuration <= 0 {
		return errors.New("invalid visual work claim request")
	}
	return validateVisualOwner(request.Owner)
}

func validateVisualOwner(owner VisualOwner) error {
	if owner.MessageID <= 0 || len(owner.BlobHash) != 64 ||
		strings.TrimSpace(owner.MediaInputKey) == "" {
		return errors.New("invalid visual owner")
	}
	if _, err := hex.DecodeString(owner.BlobHash); err != nil {
		return errors.New("invalid visual owner blob hash")
	}
	return nil
}

func verifyVisualClaim(q querier, claim VisualWorkClaim, now time.Time) error {
	var exists bool
	if err := q.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM visual_work_claims
		WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
		  AND media_input_key = ? AND proposed_revision = ?
		  AND lease_owner = ? AND fencing_token = ? AND lease_expires_at > ?
	)`, claim.GenerationID, claim.Owner.MessageID, claim.Owner.BlobHash,
		claim.Owner.MediaInputKey, claim.ProposedRevision,
		claim.LeaseOwner, claim.FencingToken, now).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrVisualClaimLost
	}
	return nil
}

func verifyVisualOwner(q querier, owner VisualOwner) error {
	canonicalPath := strings.ToLower(owner.BlobHash[:2] + "/" + owner.BlobHash)
	var exists bool
	if err := q.QueryRow(`SELECT EXISTS (
		SELECT 1
		FROM attachments a
		JOIN messages m ON m.id = a.message_id
		WHERE a.message_id = ?
		  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		  AND a.attachment_role = 'standalone'
		  AND a.role_source IN ('mime_disposition', 'provider_explicit', 'importer_semantics', 'raw_mime_repair')
		  AND (LOWER(COALESCE(a.content_hash, '')) = ?
		       OR ((a.content_hash IS NULL OR a.content_hash = '') AND LOWER(a.storage_path) = ?))
	)`, owner.MessageID, strings.ToLower(owner.BlobHash), canonicalPath).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrVisualOwnerMissing
	}
	return nil
}

// visualContentStampExpr is the dialect-portable CAS stamp of a message's
// context columns. CAST defeats driver time coercion so the stamp
// round-trips byte-identically between claim and commit.
const visualContentStampExpr = `COALESCE(CAST(content_changed_at AS TEXT), '')`

// visualOwnerContextChanged reports whether the owning message's context
// stamp moved since the claim captured it. Subject and body edits do not
// enter the shared attachment journal, so the journal fence alone cannot
// prove context freshness.
func visualOwnerContextChanged(q querier, claim VisualWorkClaim) (bool, error) {
	var stamp string
	err := q.QueryRow(`SELECT `+visualContentStampExpr+` FROM messages WHERE id = ?`,
		claim.Owner.MessageID).Scan(&stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return stamp != claim.ContentStamp, nil
}

func visualOwnerChangedAfter(q querier, owner VisualOwner, sourceFence int64) (bool, error) {
	var changed bool
	err := q.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM attachment_change_log
		WHERE sequence > ? AND (old_message_id = ? OR new_message_id = ?)
	)`, sourceFence, owner.MessageID, owner.MessageID).Scan(&changed)
	return changed, err
}

func authoritativeVisualRoleSource(source AttachmentRoleSource) bool {
	switch source {
	case AttachmentRoleSourceMIMEDisposition,
		AttachmentRoleSourceProviderExplicit,
		AttachmentRoleSourceImporterSemantics,
		AttachmentRoleSourceRawMIMERepair:
		return true
	default:
		return false
	}
}

func visualClaimMutationResult(result sql.Result, err error, operation string) error {
	if err != nil {
		return fmt.Errorf("%s visual work claim: %w", operation, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read %s visual work claim result: %w", operation, err)
	}
	if rows != 1 {
		return ErrVisualClaimLost
	}
	return nil
}

func releaseVisualClaim(q querier, dialect Dialect, claim VisualWorkClaim) error {
	result, err := q.Exec(`
		UPDATE visual_work_claims
		SET lease_expires_at = '1970-01-01 00:00:00+00:00',
		    updated_at = `+dialect.Now()+`
		WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
		  AND media_input_key = ? AND proposed_revision = ?
		  AND lease_owner = ? AND fencing_token = ?
	`, claim.GenerationID, claim.Owner.MessageID, claim.Owner.BlobHash,
		claim.Owner.MediaInputKey, claim.ProposedRevision,
		claim.LeaseOwner, claim.FencingToken)
	return visualClaimMutationResult(result, err, "release")
}

func newVisualVectorToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate visual vector token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}
