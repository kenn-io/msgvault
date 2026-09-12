package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type CardDAVMutationOperation string

const (
	CardDAVMutationCreate CardDAVMutationOperation = "create"
	CardDAVMutationUpdate CardDAVMutationOperation = "update"
	CardDAVMutationDelete CardDAVMutationOperation = "delete"
)

var (
	ErrCardDAVPublicationNotFound = errors.New("CardDAV publication not found")
	ErrCardDAVPublicationPending  = errors.New("CardDAV publication mutation is pending")
	ErrCardDAVPublicationMismatch = errors.New("CardDAV canonical resource does not match publication intent")
	ErrCardDAVResourceAmbiguous   = errors.New("multiple CardDAV resources are mapped to this person")
	ErrCardDAVNoWriteTarget       = errors.New("CardDAV write target is unavailable")
	ErrCardDAVRetryAfter          = errors.New("CardDAV account is throttled")
)

type CardDAVPublicationPlan struct {
	SourceFence              *CardDAVReviewArtifactFence
	OutgoingEnvelopeMetadata []byte
	PersonID                 int64
	Desired                  bool
	AddressBookID            int64
	Href                     string
	OutgoingBody             []byte
	OutgoingSemanticHash     string
	LocalHash                string
}

type CardDAVPublication struct {
	ConflictOwned             bool
	OutgoingEnvelopeMetadata  []byte
	ApprovedBodySHA256        *string
	ApprovedInferenceRevision *int64
	ApprovedMutationRevision  *int64
	PersonID                  int64
	Desired                   bool
	AddressBookID             int64
	Href                      string
	PendingOperation          CardDAVMutationOperation
	OutgoingBody              []byte
	OutgoingSemanticHash      string
	LocalHash                 string
	RemoteETag                string
	ConnectionGeneration      int64
	BookSyncRevision          int64
	MappingRevision           int64
	PreviousMappingRevision   int64
	CreateRecoveryUsed        bool
	MutationRevision          int64
	PendingStartedAt          *time.Time
	RecoveryOnly              bool
	Noop                      bool
	ResolutionConflictID      int64
}

// CardDAVPublicationStateSource contains only the safe fields needed to derive
// the public publication state. Mutation evidence remains in the publication
// table and never gets copied into this read model.
type CardDAVPublicationStateSource struct {
	InferenceReviewRequired bool
	PersonID                int64
	HasPublication          bool
	Desired                 bool
	PendingOperation        CardDAVMutationOperation
	AddressBookID           int64
	AddressBookName         string
	ConflictID              int64
	ProspectiveBookID       int64
	ProspectiveName         string
}

type CardDAVCanonicalMutation struct {
	Publication CardDAVPublication
	Remote      CardDAVRemoteResource
	Tombstone   bool
}

func (s *Store) PrepareCardDAVPublicationContext(
	ctx context.Context, plan CardDAVPublicationPlan,
) (*CardDAVPublication, error) {
	if plan.PersonID <= 0 || plan.AddressBookID <= 0 || strings.TrimSpace(plan.Href) == "" {
		return nil, ErrCardDAVInvalidPlan
	}
	if plan.Desired && (len(plan.OutgoingBody) == 0 || plan.OutgoingSemanticHash == "" || plan.LocalHash == "") {
		return nil, ErrCardDAVInvalidPlan
	}
	return s.prepareCardDAVPublicationContext(ctx, plan, nil)
}

func (s *Store) prepareCardDAVPublicationContext(ctx context.Context, plan CardDAVPublicationPlan, review *CardDAVReviewedPublicationPlan) (*CardDAVPublication, error) {
	var prepared *CardDAVPublication
	var options *sql.TxOptions
	if review != nil {
		options = &sql.TxOptions{Isolation: sql.LevelRepeatableRead}
	}
	err := s.withTxOptionsContext(ctx, options, func(tx *loggedTx) error {
		var err error
		prepared, err = s.prepareCardDAVPublicationTx(ctx, tx, plan, review)
		return err
	})
	if review != nil && (s.dialect.IsSerializationFailureError(err) || errors.Is(err, ErrVCardProjectionConflict) || errors.Is(err, ErrCardDAVStalePlan) || errors.Is(err, ErrCardDAVNoWriteTarget)) {
		return nil, ErrCardDAVReviewStale
	}
	return prepared, err
}

func (s *Store) prepareCardDAVPublicationTx(ctx context.Context, tx *loggedTx, plan CardDAVPublicationPlan, review *CardDAVReviewedPublicationPlan) (*CardDAVPublication, error) {
	var prepared *CardDAVPublication
	err := func() error {
		if lock := s.dialect.RowWriterLockSQL("carddav_accounts", "connection_generation"); lock != "" {
			if _, err := tx.ExecContext(ctx, lock, 1); err != nil {
				return err
			}
		}
		if gate, err := getCardDAVRetryAfterFrom(ctx, tx); err != nil {
			return err
		} else if gate != nil && gate.After(time.Now()) {
			return ErrCardDAVRetryAfter
		}
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT connection_generation FROM carddav_accounts
			WHERE id = 1`+s.dialect.SelectForUpdate()).Scan(&generation); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrCardDAVNoWriteTarget
			}
			return fmt.Errorf("lock CardDAV publication account: %w", err)
		}
		var bookRevision int64
		var canCreate, canUpdate, canDelete sql.NullBool
		if err := tx.QueryRowContext(ctx, `SELECT sync_revision, can_create, can_update, can_delete
			FROM carddav_address_books
			WHERE id = ? AND is_write_target = TRUE AND is_subscribed = TRUE`+
			s.dialect.SelectForUpdate(), plan.AddressBookID).Scan(
			&bookRevision, &canCreate, &canUpdate, &canDelete,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrCardDAVNoWriteTarget
			}
			return fmt.Errorf("lock CardDAV publication book: %w", err)
		}
		if s.cardDAVReviewPersonLockHook != nil {
			s.cardDAVReviewPersonLockHook()
		}
		if err := s.lockPersonVCardProjectionTx(ctx, tx, plan.PersonID, plan.LocalHash); err != nil {
			return err
		}
		inference, err := s.getCardDAVInferenceExportStateTx(ctx, tx, plan.PersonID)
		if err != nil {
			return err
		}
		snapshot, err := s.loadPersonVCardSnapshotTx(ctx, tx, plan.PersonID)
		if err != nil {
			return err
		}
		if len(plan.OutgoingEnvelopeMetadata) > 0 {
			if _, err := validateCardDAVPublicationMetadata(plan.OutgoingBody, plan.OutgoingEnvelopeMetadata,
				fmt.Sprintf("carddav:%d", plan.AddressBookID), plan.Href, snapshot.Profile.Person.VCardUID); err != nil {
				return err
			}
		}
		if plan.Desired && snapshot.Fingerprint != plan.LocalHash {
			return ErrCardDAVStalePlan
		}
		current, err := getCardDAVPublicationFrom(ctx, tx, plan.PersonID, s.dialect.SelectForUpdate())
		if err != nil && !errors.Is(err, ErrCardDAVPublicationNotFound) {
			return err
		}
		if err == nil && current.PendingOperation != "" {
			if review != nil {
				return ErrCardDAVReviewStale
			}
			if current.Desired != plan.Desired {
				return ErrCardDAVPublicationPending
			}
			current.RecoveryOnly = true
			prepared = current
			return nil
		}

		resource, err := findCardDAVResourceForPersonTx(ctx, tx, plan.AddressBookID, plan.PersonID, s.dialect.SelectForUpdate())
		if err != nil && !errors.Is(err, ErrCardDAVResourceNotFound) {
			return err
		}
		if plan.SourceFence != nil {
			source, err := s.loadCardDAVPublicationReviewSourceTx(ctx, tx, plan.PersonID)
			if err != nil {
				return err
			}
			currentFence := CardDAVCurrentReviewFence(source, plan.OutgoingBody, plan.Href)
			if source.Conflict != nil || CardDAVReviewToken(currentFence) != CardDAVReviewToken(*plan.SourceFence) {
				return ErrCardDAVStalePlan
			}
		}
		if review != nil {
			source, err := s.loadCardDAVPublicationReviewSourceTx(ctx, tx, plan.PersonID)
			if err != nil {
				return err
			}
			if source.Conflict != nil {
				return ErrCardDAVReviewStale
			}
			fence := CardDAVCurrentReviewFence(source, plan.OutgoingBody, plan.Href)
			if CardDAVReviewToken(fence) != review.ApprovalToken || CardDAVReviewToken(review.Fence) != review.ApprovalToken ||
				review.Fence.PersonFingerprint != plan.LocalHash {
				return ErrCardDAVReviewStale
			}
			if review.Fence.Kind != CardDAVReviewCurrent {
				return ErrCardDAVReviewStale
			}
			if err := s.approveCardDAVInferenceTx(ctx, tx, inference, generation, plan.AddressBookID); err != nil {
				return err
			}
		} else if plan.Desired && inference.ReviewRequired(generation, plan.AddressBookID) {
			return ErrCardDAVInferenceReviewRequired
		}
		if plan.Desired && resource == nil {
			_, hrefErr := s.findCardDAVResourceTx(ctx, tx, plan.AddressBookID, plan.Href)
			if hrefErr == nil {
				return ErrCardDAVPublicationMismatch
			}
			if !errors.Is(hrefErr, ErrCardDAVResourceNotFound) {
				return hrefErr
			}
		}
		if !plan.Desired && errors.Is(err, ErrCardDAVResourceNotFound) {
			if err := s.clearPersonCardDAVInferenceApprovalTx(ctx, tx, plan.PersonID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM carddav_publications WHERE person_id = ?`, plan.PersonID); err != nil {
				return fmt.Errorf("clear absent CardDAV publication: %w", err)
			}
			prepared = &CardDAVPublication{PersonID: plan.PersonID, Desired: false, Noop: true}
			return nil
		}

		operation := CardDAVMutationCreate
		remoteETag := ""
		mappingRevision, previousMappingRevision := int64(0), int64(0)
		if resource != nil {
			if resource.MappingStatus != CardDAVMappingMapped || resource.PersonID == nil || *resource.PersonID != plan.PersonID {
				return ErrCardDAVPublicationMismatch
			}
			if resource.Href != plan.Href {
				return ErrCardDAVPublicationMismatch
			}
			if etag := strings.TrimSpace(resource.RemoteETag); etag == "" || etag == "*" {
				return ErrCardDAVInvalidPlan
			}
			if plan.Desired && plan.OutgoingSemanticHash == resource.RemoteSemanticHash {
				if len(plan.OutgoingEnvelopeMetadata) > 0 {
					if err := s.putCardDAVPublicationEnvelopeTx(ctx, tx, plan.AddressBookID, plan.PersonID, resource.Href,
						plan.OutgoingBody, plan.OutgoingEnvelopeMetadata, resource.RemoteBody); err != nil {
						return err
					}
				}
				if _, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET
					local_hash = ?, updated_at = `+s.dialect.Now()+` WHERE id = ?`,
					snapshot.Fingerprint, resource.ID); err != nil {
					return fmt.Errorf("refresh unchanged CardDAV publication ledger: %w", err)
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO carddav_publications (
					person_id, desired, address_book_id, href
				) VALUES (?, TRUE, ?, ?)
				ON CONFLICT(person_id) DO UPDATE SET
					desired = TRUE, address_book_id = excluded.address_book_id,
					href = excluded.href, pending_operation = NULL,
					outgoing_body = NULL, outgoing_semantic_hash = NULL,
					approved_body_sha256 = NULL, approved_inference_revision = NULL,
					approved_mutation_revision = NULL, outgoing_envelope_metadata = NULL,
					local_hash = NULL, remote_etag = NULL,
					connection_generation = NULL, book_sync_revision = NULL,
					mapping_revision = NULL, previous_mapping_revision = NULL,
					create_recovery_used = FALSE, pending_started_at = NULL,
					updated_at = `+s.dialect.Now(), plan.PersonID, plan.AddressBookID, plan.Href)
				if err != nil {
					return fmt.Errorf("persist unchanged CardDAV publication: %w", err)
				}
				prepared, err = getCardDAVPublicationFrom(ctx, tx, plan.PersonID, "")
				if err == nil {
					prepared.Noop = true
				}
				return err
			}
			if plan.Desired {
				operation = CardDAVMutationUpdate
			} else {
				operation = CardDAVMutationDelete
			}
			remoteETag = resource.RemoteETag
			previousMappingRevision = resource.MappingRevision
			mappingRevision = resource.MappingRevision + 1
		}
		var requiredCapability sql.NullBool
		switch operation {
		case CardDAVMutationCreate:
			requiredCapability = canCreate
		case CardDAVMutationUpdate:
			requiredCapability = canUpdate
		case CardDAVMutationDelete:
			requiredCapability = canDelete
		}
		if cardDAVCapabilityDenied(requiredCapability) {
			return ErrCardDAVReadOnlyAddressBook
		}
		if resource != nil {
			result, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET
				mapping_revision = ?, updated_at = `+s.dialect.Now()+`
				WHERE id = ? AND mapping_revision = ?`, mappingRevision, resource.ID, resource.MappingRevision)
			if err != nil {
				return fmt.Errorf("advance CardDAV mutation fence: %w", err)
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return ErrCardDAVStalePlan
			}
		}
		outgoingBody, outgoingHash := plan.OutgoingBody, plan.OutgoingSemanticHash
		if operation == CardDAVMutationDelete {
			outgoingBody, outgoingHash = nil, ""
		}
		nextMutationRevision := int64(1)
		if current != nil {
			nextMutationRevision = current.MutationRevision + 1
		}
		now := time.Now().UTC()
		_, err = tx.ExecContext(ctx, `INSERT INTO carddav_publications (
			person_id, desired, address_book_id, href, pending_operation,
			outgoing_body, outgoing_semantic_hash, local_hash, remote_etag,
			connection_generation, book_sync_revision, mapping_revision,
			previous_mapping_revision, create_recovery_used, mutation_revision,
			pending_started_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, NULLIF(?, ''), ?, ?, ?, ?, FALSE, ?, ?, `+s.dialect.Now()+`)
		ON CONFLICT(person_id) DO UPDATE SET
			desired = excluded.desired, address_book_id = excluded.address_book_id,
			href = excluded.href, pending_operation = excluded.pending_operation,
			outgoing_body = excluded.outgoing_body,
			approved_body_sha256 = NULL, approved_inference_revision = NULL,
			approved_mutation_revision = NULL, outgoing_envelope_metadata = NULL,
			outgoing_semantic_hash = excluded.outgoing_semantic_hash,
			local_hash = excluded.local_hash, remote_etag = excluded.remote_etag,
			connection_generation = excluded.connection_generation,
			book_sync_revision = excluded.book_sync_revision,
			mapping_revision = excluded.mapping_revision,
			previous_mapping_revision = excluded.previous_mapping_revision,
			create_recovery_used = FALSE, mutation_revision = excluded.mutation_revision,
			pending_started_at = excluded.pending_started_at, updated_at = `+s.dialect.Now(),
			plan.PersonID, plan.Desired, plan.AddressBookID, plan.Href, operation,
			outgoingBody, outgoingHash, snapshot.Fingerprint, remoteETag,
			generation, bookRevision, mappingRevision, previousMappingRevision,
			nextMutationRevision, timeValue(&now))
		if err != nil {
			return fmt.Errorf("persist CardDAV publication intent: %w", err)
		}
		if plan.Desired {
			if _, err := tx.ExecContext(ctx, `UPDATE carddav_publications SET approved_body_sha256 = ?,
    approved_inference_revision = ?, approved_mutation_revision = mutation_revision, outgoing_envelope_metadata = ? WHERE person_id = ?`,
				CardDAVBodySHA256(plan.OutgoingBody), inference.InferenceRevision, nullableCardDAVMetadata(plan.OutgoingEnvelopeMetadata), plan.PersonID); err != nil {
				return err
			}
		}
		prepared, err = getCardDAVPublicationFrom(ctx, tx, plan.PersonID, "")
		return err
	}()
	return prepared, err
}

func (s *Store) GetCardDAVPublicationContext(ctx context.Context, personID int64) (*CardDAVPublication, error) {
	return getCardDAVPublicationFrom(ctx, s.db, personID, "")
}

func (s *Store) GetCardDAVPublicationStateSourceContext(
	ctx context.Context, personID int64,
) (*CardDAVPublicationStateSource, error) {
	if personID <= 0 {
		return nil, ErrPersonNotFound
	}
	var source *CardDAVPublicationStateSource
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		if _, err := s.getPersonTx(ctx, tx, personID); err != nil {
			return err
		}
		if s.cardDAVPublicationStateReadHook != nil {
			s.cardDAVPublicationStateReadHook()
		}
		current := &CardDAVPublicationStateSource{PersonID: personID}
		var publicationHref string
		var pendingOperation sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT desired, address_book_id, href, pending_operation
			FROM carddav_publications WHERE person_id = ?`, personID).Scan(
			&current.Desired, &current.AddressBookID, &publicationHref, &pendingOperation)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("get CardDAV publication state: %w", err)
		}
		if err == nil {
			current.HasPublication = true
			current.PendingOperation = CardDAVMutationOperation(pendingOperation.String)
			err = tx.QueryRowContext(ctx, `SELECT id, display_name
				FROM carddav_address_books WHERE id = ?`, current.AddressBookID).Scan(
				&current.AddressBookID, &current.AddressBookName)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrCardDAVAddressBookNotFound
			}
			if err != nil {
				return fmt.Errorf("get CardDAV publication address book: %w", err)
			}
			err = tx.QueryRowContext(ctx, `SELECT id FROM carddav_conflicts
				WHERE address_book_id = ? AND href = ? AND status = 'unresolved'`,
				current.AddressBookID, publicationHref).Scan(&current.ConflictID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("get CardDAV publication conflict: %w", err)
			}
			source = current
			return s.loadCardDAVPublicationReviewRequiredTx(ctx, tx, current)
		}
		err = tx.QueryRowContext(ctx, `SELECT id, display_name
			FROM carddav_address_books
			WHERE is_write_target = TRUE AND is_subscribed = TRUE
			ORDER BY discovery_index, id LIMIT 1`).Scan(
			&current.ProspectiveBookID, &current.ProspectiveName)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("get prospective CardDAV publication book: %w", err)
		}
		source = current
		return s.loadCardDAVPublicationReviewRequiredTx(ctx, tx, current)
	})
	return source, err
}

func (s *Store) RefreshCardDAVPublicationFenceContext(
	ctx context.Context, personID int64,
) (*CardDAVPublication, error) {
	identity, err := s.GetCardDAVPublicationContext(ctx, personID)
	if err != nil {
		return nil, err
	}
	var publication *CardDAVPublication
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		current, err := s.lockCardDAVPublicationOperationTx(ctx, tx, personID, identity.AddressBookID)
		if err != nil {
			return err
		}
		if current.PendingOperation == "" {
			return ErrCardDAVPublicationNotFound
		}
		var generation, bookRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT connection_generation FROM carddav_accounts WHERE id = 1`).Scan(&generation); err != nil {
			return err
		}
		if generation != current.ConnectionGeneration {
			return ErrCardDAVStalePlan
		}
		if err := tx.QueryRowContext(ctx, `SELECT sync_revision FROM carddav_address_books WHERE id = ?`, current.AddressBookID).Scan(&bookRevision); err != nil {
			return err
		}
		mappingRevision := int64(0)
		resource, resourceErr := findCardDAVResourceForPersonTx(ctx, tx, current.AddressBookID, personID, s.dialect.SelectForUpdate())
		if current.PreviousMappingRevision > 0 {
			if resourceErr != nil && (current.PendingOperation != CardDAVMutationDelete || !errors.Is(resourceErr, ErrCardDAVResourceNotFound)) {
				return resourceErr
			}
		} else if resourceErr != nil && !errors.Is(resourceErr, ErrCardDAVResourceNotFound) {
			return resourceErr
		}
		if resource != nil {
			if resource.Href != current.Href {
				return ErrCardDAVPublicationMismatch
			}
			mappingRevision = resource.MappingRevision
		}
		_, err = tx.ExecContext(ctx, `UPDATE carddav_publications SET
			book_sync_revision = ?, mapping_revision = ?, updated_at = `+s.dialect.Now()+`
			WHERE person_id = ? AND mutation_revision = ?`,
			bookRevision, mappingRevision, personID, current.MutationRevision)
		if err != nil {
			return err
		}
		publication, err = getCardDAVPublicationFrom(ctx, tx, personID, "")
		return err
	})
	return publication, err
}

// FenceCardDAVCreateCollisionContext materializes the canonical resource that
// won a conditional create race. The mapping gives the normal conflict ledger
// a durable base revision instead of leaving an unresolvable create intent.
func (s *Store) FenceCardDAVCreateCollisionContext(
	ctx context.Context, pending CardDAVPublication, remote CardDAVRemoteResource,
) (*CardDAVPublication, error) {
	if pending.PersonID <= 0 || pending.PendingOperation != CardDAVMutationCreate ||
		remote.Href != pending.Href || remote.RemoteETag == "" || len(remote.RemoteBody) == 0 ||
		remote.SemanticHash == "" || remote.SemanticHash == pending.OutgoingSemanticHash {
		return nil, ErrCardDAVInvalidPlan
	}
	var fenced *CardDAVPublication
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockCardDAVPublicationTargetTx(ctx, tx, pending.AddressBookID); err != nil {
			return err
		}
		if s.cardDAVCollisionIdentityLockHook != nil {
			s.cardDAVCollisionIdentityLockHook()
		}
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		current, err := s.lockCardDAVPublicationOperationTx(ctx, tx, pending.PersonID, pending.AddressBookID)
		if err != nil {
			return err
		}
		if current.MutationRevision != pending.MutationRevision ||
			current.PendingOperation != CardDAVMutationCreate || current.AddressBookID != pending.AddressBookID ||
			current.Href != remote.Href || current.MappingRevision != 0 {
			return ErrCardDAVStalePlan
		}
		var generation, bookRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT connection_generation FROM carddav_accounts
			WHERE id = 1`+s.dialect.SelectForUpdate()).Scan(&generation); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT sync_revision FROM carddav_address_books
			WHERE id = ?`+s.dialect.SelectForUpdate(), current.AddressBookID).Scan(&bookRevision); err != nil {
			return err
		}
		if generation != current.ConnectionGeneration || bookRevision != current.BookSyncRevision {
			return ErrCardDAVStalePlan
		}
		resource, err := s.findCardDAVResourceTx(ctx, tx, current.AddressBookID, current.Href)
		if errors.Is(err, ErrCardDAVResourceNotFound) {
			var personRevision, resourceID int64
			if err := tx.QueryRowContext(ctx, `SELECT revision FROM persons WHERE id = ?`, current.PersonID).
				Scan(&personRevision); err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, `INSERT INTO carddav_resources (
				address_book_id, href, remote_uid, remote_etag, remote_body,
				remote_semantic_hash, local_hash, mapping_status, mapping_revision,
				governance, person_id, person_revision_at_bind
			) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, 1, ?, ?, ?) RETURNING id`,
				current.AddressBookID, remote.Href, remote.RemoteUID, remote.RemoteETag,
				remote.RemoteBody, remote.SemanticHash, current.LocalHash,
				CardDAVMappingMapped, CardDAVGovernanceLocal, current.PersonID, personRevision).
				Scan(&resourceID); err != nil {
				return fmt.Errorf("materialize CardDAV create collision: %w", err)
			}
			resource, err = s.findCardDAVResourceTx(ctx, tx, current.AddressBookID, current.Href)
		}
		if err != nil {
			return err
		}
		if resource.PersonID == nil || *resource.PersonID != current.PersonID ||
			resource.MappingStatus != CardDAVMappingMapped {
			return ErrCardDAVPublicationMismatch
		}
		if err := s.putCardDAVEnvelopeTx(ctx, tx, current.AddressBookID, current.PersonID, remote); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE carddav_publications SET
			mapping_revision = ?, previous_mapping_revision = ?, updated_at = `+s.dialect.Now()+`
			WHERE person_id = ? AND mutation_revision = ? AND pending_operation = ?`,
			resource.MappingRevision, resource.MappingRevision, current.PersonID,
			current.MutationRevision, CardDAVMutationCreate)
		if err != nil {
			return fmt.Errorf("fence CardDAV create collision: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrCardDAVStalePlan
		}
		fenced, err = getCardDAVPublicationFrom(ctx, tx, current.PersonID, "")
		return err
	})
	return fenced, err
}

func (s *Store) CommitCardDAVPublicationContext(
	ctx context.Context, input CardDAVCanonicalMutation,
) error {
	pending := input.Publication
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		current, err := s.lockCardDAVPublicationOperationTx(ctx, tx, pending.PersonID, pending.AddressBookID)
		if err != nil {
			return err
		}
		if current.MutationRevision != pending.MutationRevision || current.PendingOperation != pending.PendingOperation {
			return ErrCardDAVStalePlan
		}
		var generation, bookRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT connection_generation FROM carddav_accounts WHERE id = 1`+
			s.dialect.SelectForUpdate()).Scan(&generation); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT sync_revision FROM carddav_address_books WHERE id = ?`+
			s.dialect.SelectForUpdate(), current.AddressBookID).Scan(&bookRevision); err != nil {
			return err
		}
		if generation != current.ConnectionGeneration || bookRevision != current.BookSyncRevision {
			return ErrCardDAVStalePlan
		}
		snapshot, err := s.loadPersonVCardSnapshotTx(ctx, tx, current.PersonID)
		if err != nil {
			return err
		}
		if snapshot.Fingerprint != current.LocalHash && current.PendingOperation != CardDAVMutationCreate {
			return ErrCardDAVPublicationMismatch
		}
		if current.PendingOperation == CardDAVMutationDelete {
			if !input.Tombstone {
				return ErrCardDAVPublicationMismatch
			}
			if err := s.clearPersonCardDAVInferenceApprovalTx(ctx, tx, current.PersonID); err != nil {
				return err
			}
			resource, err := findCardDAVResourceForPersonTx(ctx, tx, current.AddressBookID, current.PersonID, s.dialect.SelectForUpdate())
			if errors.Is(err, ErrCardDAVResourceNotFound) && current.MappingRevision == 0 {
				_, err = tx.ExecContext(ctx, `DELETE FROM carddav_publications WHERE person_id = ?`, current.PersonID)
				return err
			}
			if err != nil {
				return err
			}
			if resource.MappingRevision != current.MappingRevision || resource.Href != current.Href {
				return ErrCardDAVStalePlan
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM identity_match_candidates
				WHERE (left_kind = ? AND left_id = ?) OR (right_kind = ? AND right_id = ?)`,
				IdentityMatchCardDAVResource, resource.ID, IdentityMatchCardDAVResource, resource.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM vcard_resource_envelopes
				WHERE source_ref = ? AND source_resource_uid = ?`, fmt.Sprintf("carddav:%d", current.AddressBookID), current.Href); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM carddav_resources WHERE id = ?`, resource.ID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM carddav_publications WHERE person_id = ?`, current.PersonID); err != nil {
				return err
			}
			return s.resolvePublicationConflictAuditTx(ctx, tx, pending)
		}
		if input.Tombstone || input.Remote.Href != current.Href || input.Remote.SemanticHash != current.OutgoingSemanticHash ||
			len(input.Remote.RemoteBody) == 0 || input.Remote.RemoteETag == "" {
			return ErrCardDAVPublicationMismatch
		}
		var resource *CardDAVResource
		if current.PendingOperation == CardDAVMutationCreate {
			resource, err = findCardDAVResourceForPersonTx(ctx, tx, current.AddressBookID, current.PersonID, s.dialect.SelectForUpdate())
			if errors.Is(err, ErrCardDAVResourceNotFound) && current.MappingRevision == 0 {
				var personRevision int64
				if err := tx.QueryRowContext(ctx, `SELECT revision FROM persons WHERE id = ?`, current.PersonID).Scan(&personRevision); err != nil {
					return err
				}
				var resourceID int64
				if err := tx.QueryRowContext(ctx, `INSERT INTO carddav_resources (
				address_book_id, href, remote_uid, remote_etag, remote_body,
				remote_semantic_hash, local_hash, mapping_status, mapping_revision,
				governance, person_id, person_revision_at_bind
			) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, 1, ?, ?, ?) RETURNING id`,
					current.AddressBookID, input.Remote.Href, input.Remote.RemoteUID,
					input.Remote.RemoteETag, input.Remote.RemoteBody, input.Remote.SemanticHash,
					current.LocalHash, CardDAVMappingMapped, CardDAVGovernanceLocal,
					current.PersonID, personRevision).Scan(&resourceID); err != nil {
					return fmt.Errorf("commit CardDAV remote create: %w", err)
				}
			} else {
				if err != nil {
					return err
				}
				if resource.Href != current.Href || resource.MappingRevision != current.MappingRevision {
					return ErrCardDAVStalePlan
				}
				_, err = tx.ExecContext(ctx, `UPDATE carddav_resources SET
					remote_uid = NULLIF(?, ''), remote_etag = ?, remote_body = ?,
					remote_semantic_hash = ?, local_hash = ?, mapping_status = ?,
					governance = ?, updated_at = `+s.dialect.Now()+` WHERE id = ?`,
					input.Remote.RemoteUID, input.Remote.RemoteETag, input.Remote.RemoteBody,
					input.Remote.SemanticHash, current.LocalHash, CardDAVMappingMapped,
					CardDAVGovernanceLocal, resource.ID)
				if err != nil {
					return err
				}
			}
		} else {
			resource, err = findCardDAVResourceForPersonTx(ctx, tx, current.AddressBookID, current.PersonID, s.dialect.SelectForUpdate())
			if err != nil {
				return err
			}
			if resource.MappingRevision != current.MappingRevision || resource.Href != current.Href {
				return ErrCardDAVStalePlan
			}
			_, err = tx.ExecContext(ctx, `UPDATE carddav_resources SET
				remote_uid = NULLIF(?, ''), remote_etag = ?, remote_body = ?,
				remote_semantic_hash = ?, local_hash = ?, mapping_status = ?,
				governance = ?, updated_at = `+s.dialect.Now()+` WHERE id = ?`,
				input.Remote.RemoteUID, input.Remote.RemoteETag, input.Remote.RemoteBody,
				input.Remote.SemanticHash, current.LocalHash, CardDAVMappingMapped,
				CardDAVGovernanceLocal, resource.ID)
			if err != nil {
				return err
			}
		}
		if len(current.OutgoingEnvelopeMetadata) > 0 {
			if err := s.putCardDAVPublicationEnvelopeTx(ctx, tx, current.AddressBookID, current.PersonID, input.Remote.Href,
				current.OutgoingBody, current.OutgoingEnvelopeMetadata, input.Remote.RemoteBody); err != nil {
				return err
			}
		} else if err := s.putCardDAVEnvelopeTx(ctx, tx, current.AddressBookID, current.PersonID, input.Remote); err != nil {
			return err
		}

		result, err := tx.ExecContext(ctx, `UPDATE carddav_publications SET desired = TRUE,
			pending_operation = NULL, outgoing_body = NULL,
			approved_body_sha256 = NULL, approved_inference_revision = NULL,
			approved_mutation_revision = NULL, outgoing_envelope_metadata = NULL,
			outgoing_semantic_hash = NULL, local_hash = NULL, remote_etag = NULL,
			connection_generation = NULL, book_sync_revision = NULL,
			mapping_revision = NULL, previous_mapping_revision = NULL,
			create_recovery_used = FALSE, pending_started_at = NULL,
			updated_at = `+s.dialect.Now()+` WHERE person_id = ? AND mutation_revision = ?`,
			current.PersonID, current.MutationRevision)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrCardDAVStalePlan
		}
		return s.resolvePublicationConflictAuditTx(ctx, tx, pending)
	})
}

func (s *Store) resolvePublicationConflictAuditTx(
	ctx context.Context, tx *loggedTx, pending CardDAVPublication,
) error {
	if pending.ResolutionConflictID == 0 {
		return nil
	}
	var writeTarget bool
	if err := tx.QueryRowContext(ctx, `SELECT is_write_target FROM carddav_address_books WHERE id=?`, pending.AddressBookID).Scan(&writeTarget); err != nil {
		return err
	}
	if !writeTarget {
		if _, err := tx.ExecContext(ctx, `DELETE FROM carddav_publications WHERE person_id=? AND address_book_id=? AND href=? AND mutation_revision=?`, pending.PersonID, pending.AddressBookID, pending.Href, pending.MutationRevision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_carddav_inference_state SET approved_revision=0,approved_connection_generation=NULL,approved_address_book_id=NULL WHERE person_id=? AND approved_address_book_id=?`, pending.PersonID, pending.AddressBookID); err != nil {
			return err
		}
	}
	_, err := resolveCardDAVConflictAuditTx(ctx, tx, s.dialect,
		pending.ResolutionConflictID, CardDAVResolutionKeepLocal)
	return err
}

func (s *Store) RollbackCardDAVPublicationThrottleContext(
	ctx context.Context, pending *CardDAVPublication, retryAfter time.Time,
) error {
	return s.rollbackCardDAVPublicationContext(ctx, pending, &retryAfter)
}

// RollbackCardDAVPublicationContext clears a definitively rejected remote
// mutation and restores the mapping fence advanced while preparing it.
func (s *Store) RollbackCardDAVPublicationContext(
	ctx context.Context, pending *CardDAVPublication,
) error {
	return s.rollbackCardDAVPublicationContext(ctx, pending, nil)
}

func (s *Store) rollbackCardDAVPublicationContext(
	ctx context.Context, pending *CardDAVPublication, retryAfter *time.Time,
) error {
	if pending == nil {
		return ErrCardDAVInvalidPlan
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		current, err := s.lockCardDAVPublicationOperationTx(ctx, tx, pending.PersonID, pending.AddressBookID)
		if err != nil {
			return err
		}
		if current.MutationRevision != pending.MutationRevision || current.PendingOperation != pending.PendingOperation {
			return ErrCardDAVStalePlan
		}
		if current.PendingOperation != CardDAVMutationCreate {
			result, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET
				mapping_revision = ?, updated_at = `+s.dialect.Now()+`
				WHERE address_book_id = ? AND person_id = ? AND href = ? AND mapping_revision = ?`,
				current.PreviousMappingRevision, current.AddressBookID, current.PersonID,
				current.Href, current.MappingRevision)
			if err != nil {
				return err
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return ErrCardDAVStalePlan
			}
		}
		if retryAfter == nil && current.PendingOperation == CardDAVMutationCreate &&
			current.PreviousMappingRevision == 0 && pending.ResolutionConflictID == 0 {
			result, err := tx.ExecContext(ctx, `DELETE FROM carddav_publications
				WHERE person_id = ? AND mutation_revision = ?`, current.PersonID, current.MutationRevision)
			if err != nil {
				return err
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return ErrCardDAVStalePlan
			}
			return nil
		}
		desired := true
		if retryAfter != nil {
			desired = current.Desired
		}
		result, err := tx.ExecContext(ctx, `UPDATE carddav_publications SET
			desired = ?,
			pending_operation = NULL, outgoing_body = NULL,
			approved_body_sha256 = NULL, approved_inference_revision = NULL,
			approved_mutation_revision = NULL, outgoing_envelope_metadata = NULL,
			outgoing_semantic_hash = NULL, local_hash = NULL, remote_etag = NULL,
			connection_generation = NULL, book_sync_revision = NULL,
			mapping_revision = NULL, previous_mapping_revision = NULL,
			create_recovery_used = FALSE, pending_started_at = NULL,
			updated_at = `+s.dialect.Now()+`
			WHERE person_id = ? AND mutation_revision = ?`, desired, current.PersonID, current.MutationRevision)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrCardDAVStalePlan
		}
		if retryAfter != nil {
			return s.setCardDAVRetryAfterFrom(ctx, tx, *retryAfter)
		}
		return nil
	})
}

func (s *Store) GetCardDAVRetryAfterContext(ctx context.Context) (*time.Time, error) {
	return getCardDAVRetryAfterFrom(ctx, s.db)
}

func (s *Store) CheckCardDAVRetryAfterContext(ctx context.Context) error {
	gate, err := s.GetCardDAVRetryAfterContext(ctx)
	if err != nil {
		return err
	}
	if gate != nil && gate.After(time.Now()) {
		return ErrCardDAVRetryAfter
	}
	return nil
}

func (s *Store) SetCardDAVRetryAfterContext(ctx context.Context, retryAfter time.Time) error {
	err := s.setCardDAVRetryAfterFrom(ctx, s.db, retryAfter)
	if err != nil {
		return fmt.Errorf("set CardDAV retry gate: %w", err)
	}
	return nil
}

func (s *Store) setCardDAVRetryAfterFrom(
	ctx context.Context, execer contextQuerier, retryAfter time.Time,
) error {
	gate := retryAfter.UTC()
	_, err := execer.ExecContext(ctx, `INSERT INTO carddav_retry_gate (account_id, retry_after_at, updated_at)
		VALUES (1, ?, `+s.dialect.Now()+`) ON CONFLICT(account_id) DO UPDATE SET
		retry_after_at = CASE
			WHEN carddav_retry_gate.retry_after_at < excluded.retry_after_at THEN excluded.retry_after_at
			ELSE carddav_retry_gate.retry_after_at
		END,
		updated_at = `+s.dialect.Now(), timeValue(&gate))
	return err
}

func (s *Store) ListCardDAVPublicationPersonIDsContext(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT person_id FROM carddav_publications ORDER BY person_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func getCardDAVRetryAfterFrom(ctx context.Context, queryer contextRowQuerier) (*time.Time, error) {
	var value sql.NullTime
	err := queryer.QueryRowContext(ctx, `SELECT retry_after_at FROM carddav_retry_gate WHERE account_id = 1`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // No retry-gate row means synchronization is not throttled.
	}
	if err != nil {
		return nil, fmt.Errorf("get CardDAV retry gate: %w", err)
	}
	if !value.Valid {
		return nil, nil //nolint:nilnil // A cleared nullable gate is equivalent to no throttle.
	}
	gate := value.Time.UTC()
	return &gate, nil
}

func getCardDAVPublicationFrom(
	ctx context.Context, queryer contextRowQuerier, personID int64, suffix string,
) (*CardDAVPublication, error) {
	var result CardDAVPublication
	var bookID, generation, bookRevision, mappingRevision, previousMapping sql.NullInt64
	var href, operation, outgoingHash, localHash, remoteETag sql.NullString
	var approvedBody sql.NullString
	var approvedInference, approvedMutation sql.NullInt64
	var outgoing []byte
	var pendingAt sql.NullTime
	err := queryer.QueryRowContext(ctx, `SELECT person_id, desired, address_book_id, href,
		pending_operation, outgoing_body, outgoing_semantic_hash, local_hash,
		remote_etag, connection_generation, book_sync_revision, mapping_revision,
		previous_mapping_revision, create_recovery_used, mutation_revision,
		pending_started_at, approved_body_sha256, approved_inference_revision, approved_mutation_revision, outgoing_envelope_metadata FROM carddav_publications WHERE person_id = ?`+suffix, personID).Scan(
		&result.PersonID, &result.Desired, &bookID, &href, &operation, &outgoing,
		&outgoingHash, &localHash, &remoteETag, &generation, &bookRevision,
		&mappingRevision, &previousMapping, &result.CreateRecoveryUsed,
		&result.MutationRevision, &pendingAt, &approvedBody, &approvedInference, &approvedMutation, &result.OutgoingEnvelopeMetadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCardDAVPublicationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get CardDAV publication: %w", err)
	}
	if approvedBody.Valid {
		result.ApprovedBodySHA256 = &approvedBody.String
	}
	result.ApprovedInferenceRevision = cardDAVInferenceNullInt64Ptr(approvedInference)
	result.ApprovedMutationRevision = cardDAVInferenceNullInt64Ptr(approvedMutation)
	result.AddressBookID, result.Href = bookID.Int64, href.String
	result.PendingOperation = CardDAVMutationOperation(operation.String)
	result.OutgoingBody = append([]byte(nil), outgoing...)
	result.OutgoingSemanticHash, result.LocalHash, result.RemoteETag = outgoingHash.String, localHash.String, remoteETag.String
	result.ConnectionGeneration, result.BookSyncRevision = generation.Int64, bookRevision.Int64
	result.MappingRevision, result.PreviousMappingRevision = mappingRevision.Int64, previousMapping.Int64
	if pendingAt.Valid {
		value := pendingAt.Time.UTC()
		result.PendingStartedAt = &value
	}
	return &result, nil
}

func findCardDAVResourceForPersonTx(
	ctx context.Context, queryer contextRowQuerier,
	bookID, personID int64, suffix string,
) (*CardDAVResource, error) {
	var count int
	if err := queryer.QueryRowContext(ctx, `SELECT COUNT(*) FROM carddav_resources
		WHERE address_book_id = ? AND person_id = ?`, bookID, personID).Scan(&count); err != nil {
		return nil, fmt.Errorf("count CardDAV resources for person: %w", err)
	}
	if count > 1 {
		return nil, ErrCardDAVResourceAmbiguous
	}
	resource, err := scanCardDAVResource(queryer.QueryRowContext(ctx,
		cardDAVResourceSelect+` WHERE address_book_id = ? AND person_id = ?`+suffix,
		bookID, personID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCardDAVResourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find CardDAV resource for person: %w", err)
	}
	return resource, nil
}
