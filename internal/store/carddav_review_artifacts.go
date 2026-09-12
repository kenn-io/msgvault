package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type CardDAVPendingCreateApprovalPlan struct {
	Fence         CardDAVReviewArtifactFence
	Body          []byte
	ApprovalToken string
}

func (p *CardDAVPublication) HasExactBodyApproval() bool {
	return p != nil && p.ApprovedBodySHA256 != nil && p.ApprovedInferenceRevision != nil && p.ApprovedMutationRevision != nil &&
		*p.ApprovedMutationRevision == p.MutationRevision && *p.ApprovedBodySHA256 == CardDAVBodySHA256(p.OutgoingBody)
}

func CardDAVPendingReviewFence(source *CardDAVPublicationReviewSource) CardDAVReviewArtifactFence {
	p := source.Publication
	fence := CardDAVCurrentReviewFence(source, p.OutgoingBody, p.Href)
	fence.Kind = CardDAVReviewPending
	fence.Href = p.Href
	return fence
}

func (s *Store) reviewArtifactError(err error) error {
	if s.dialect.IsSerializationFailureError(err) || errors.Is(err, ErrVCardProjectionConflict) || errors.Is(err, ErrCardDAVStalePlan) || errors.Is(err, ErrCardDAVPublicationNotFound) || errors.Is(err, ErrCardDAVConflictStale) {
		return ErrCardDAVReviewStale
	}
	return err
}

func (s *Store) validatePendingCreateTx(ctx context.Context, tx *loggedTx, pending CardDAVPublication) (*CardDAVPublication, error) {
	current, err := s.lockCardDAVPublicationOperationTx(ctx, tx, pending.PersonID, pending.AddressBookID)
	if err != nil {
		return nil, err
	}
	if current.PendingOperation != CardDAVMutationCreate || current.MutationRevision != pending.MutationRevision || current.Href != pending.Href ||
		current.ConnectionGeneration != pending.ConnectionGeneration || current.BookSyncRevision != pending.BookSyncRevision || current.MappingRevision != pending.MappingRevision || !bytes.Equal(current.OutgoingBody, pending.OutgoingBody) {
		return nil, ErrCardDAVStalePlan
	}
	var generation, revision int64
	if err := tx.QueryRowContext(ctx, `SELECT connection_generation FROM carddav_accounts WHERE id=1`).Scan(&generation); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT sync_revision FROM carddav_address_books WHERE id=?`, current.AddressBookID).Scan(&revision); err != nil {
		return nil, err
	}
	if generation != current.ConnectionGeneration || revision != current.BookSyncRevision {
		return nil, ErrCardDAVStalePlan
	}
	resource, err := s.findCardDAVResourceTx(ctx, tx, current.AddressBookID, current.Href)
	if err != nil && !errors.Is(err, ErrCardDAVResourceNotFound) {
		return nil, err
	}
	if resource != nil && (resource.PersonID == nil || *resource.PersonID != current.PersonID || resource.MappingRevision != current.MappingRevision) {
		return nil, ErrCardDAVStalePlan
	}
	if resource == nil && current.MappingRevision != 0 {
		return nil, ErrCardDAVStalePlan
	}
	return current, nil
}

// ValidateCardDAVPendingCreateRetryContext checks immutable authorization after
// canonical absence has been observed. The service holds the person operation guard.
func (s *Store) ValidateCardDAVPendingCreateRetryContext(ctx context.Context, pending CardDAVPublication) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		current, err := s.validatePendingCreateTx(ctx, tx, pending)
		if err != nil {
			return err
		}
		if !current.HasExactBodyApproval() {
			return ErrCardDAVInferenceReviewRequired
		}
		return nil
	})
}

func (s *Store) ApprovePendingCardDAVCreateContext(ctx context.Context, plan CardDAVPendingCreateApprovalPlan) (*CardDAVPublication, error) {
	var approved *CardDAVPublication
	err := s.withTxOptionsContext(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(tx *loggedTx) error {
		identity, err := getCardDAVPublicationFrom(ctx, tx, plan.Fence.PersonID, "")
		if err != nil {
			return err
		}
		if identity.AddressBookID != plan.Fence.AddressBookID {
			return ErrCardDAVReviewStale
		}
		current, err := s.validatePendingCreateTx(ctx, tx, *identity)
		if err != nil {
			return err
		}
		source, err := s.loadCardDAVPublicationReviewSourceTx(ctx, tx, current.PersonID)
		if err != nil {
			return err
		}
		if source.Conflict != nil || plan.Fence.Kind != CardDAVReviewPending || CardDAVReviewToken(plan.Fence) != plan.ApprovalToken || CardDAVReviewToken(CardDAVPendingReviewFence(source)) != plan.ApprovalToken || !bytes.Equal(current.OutgoingBody, plan.Body) {
			return ErrCardDAVReviewStale
		}
		// Older frozen bytes retain their captured inference revision.
		// Legacy intents without a captured revision retain zero inference approval.
		approvedInferenceRevision := int64(0)
		if current.ApprovedInferenceRevision != nil {
			approvedInferenceRevision = *current.ApprovedInferenceRevision
		}
		if current.LocalHash == source.Snapshot.Fingerprint {
			if err := s.approveCardDAVInferenceTx(ctx, tx, source.Inference, source.ConnectionGeneration, source.Book.ID); err != nil {
				return err
			}
			approvedInferenceRevision = source.Inference.InferenceRevision
		}
		_, err = tx.ExecContext(ctx, `UPDATE carddav_publications SET approved_body_sha256=?, approved_inference_revision=?, approved_mutation_revision=mutation_revision WHERE person_id=?`, CardDAVBodySHA256(current.OutgoingBody), approvedInferenceRevision, current.PersonID)
		if err != nil {
			return err
		}
		approved, err = getCardDAVPublicationFrom(ctx, tx, current.PersonID, "")
		return err
	})
	return approved, s.reviewArtifactError(err)
}

// CancelAbsentCardDAVCreateContext consumes canonical absence under the same
// durable fences as replay. Deleting the intent also removes all artifact metadata.
func (s *Store) CancelAbsentCardDAVCreateContext(ctx context.Context, pending CardDAVPublication) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		current, err := s.validatePendingCreateTx(ctx, tx, pending)
		if err != nil {
			return err
		}
		if current.MappingRevision != 0 {
			// Pull may have materialized this create before the remote card was
			// removed. Canonical absence permits removing that exact mapping,
			// while retaining the local person and its inference debt.
			if current.PreviousMappingRevision != 0 {
				return ErrCardDAVPublicationPending
			}
			resource, err := s.findCardDAVResourceTx(ctx, tx, current.AddressBookID, current.Href)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM identity_match_candidates WHERE (left_kind=? AND left_id=?) OR (right_kind=? AND right_id=?)`, IdentityMatchCardDAVResource, resource.ID, IdentityMatchCardDAVResource, resource.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM vcard_resource_envelopes WHERE source_ref=? AND source_resource_uid=?`, fmt.Sprintf("carddav:%d", current.AddressBookID), current.Href); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM carddav_resources WHERE id=?`, resource.ID); err != nil {
				return err
			}
		}
		if err := s.clearPersonCardDAVInferenceApprovalTx(ctx, tx, current.PersonID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM carddav_publications WHERE person_id=? AND mutation_revision=?`, current.PersonID, current.MutationRevision)
		return err
	})
}

type CardDAVConflictLocalApprovalPlan struct {
	Fence            CardDAVReviewArtifactFence
	Body             []byte
	EnvelopeMetadata []byte
	ApprovalToken    string
}

func (s *Store) LoadCardDAVConflictReviewSourceContext(ctx context.Context, conflictID int64) (*CardDAVPublicationReviewSource, error) {
	var source *CardDAVPublicationReviewSource
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		source, err = s.loadCardDAVConflictReviewSourceTx(ctx, tx, conflictID)
		return err
	})
	return source, err
}

func (s *Store) loadCardDAVConflictReviewSourceTx(ctx context.Context, tx *loggedTx, conflictID int64) (*CardDAVPublicationReviewSource, error) {
	conflict, err := getCardDAVConflictFrom(ctx, tx, conflictID, "")
	if err != nil {
		return nil, err
	}
	if conflict.Status != CardDAVConflictUnresolved || conflict.LocalTombstone {
		return nil, ErrCardDAVConflictStale
	}
	resource, err := scanCardDAVResource(tx.QueryRowContext(ctx, cardDAVResourceSelect+` WHERE address_book_id=? AND href=?`, conflict.AddressBookID, conflict.Href))
	if err != nil {
		return nil, err
	}
	if resource.PersonID == nil {
		return nil, ErrCardDAVConflictStale
	}
	snapshot, err := s.loadPersonVCardSnapshotTx(ctx, tx, *resource.PersonID)
	if err != nil {
		return nil, err
	}
	inference, err := s.getCardDAVInferenceExportStateTx(ctx, tx, *resource.PersonID)
	if err != nil {
		return nil, err
	}
	account, err := getCardDAVAccountFrom(ctx, tx.Tx, s.Rebind)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, ErrCardDAVConflictStale
	}
	books, err := listCardDAVBooksFrom(ctx, tx.Tx, s.Rebind)
	if err != nil {
		return nil, err
	}
	var book CardDAVAddressBook
	for _, candidate := range books {
		if candidate.ID == conflict.AddressBookID {
			book = candidate
			break
		}
	}
	if book.ID == 0 || !book.IsSubscribed {
		return nil, ErrCardDAVNoWriteTarget
	}
	publication, err := getCardDAVPublicationFrom(ctx, tx, *resource.PersonID, "")
	if err != nil && !errors.Is(err, ErrCardDAVPublicationNotFound) {
		return nil, err
	}
	envelope, err := s.findVCardResourceEnvelopeTx(ctx, tx, fmt.Sprintf("carddav:%d", book.ID), resource.Href)
	if err != nil {
		return nil, err
	}
	return &CardDAVPublicationReviewSource{Person: snapshot.Profile.Person, Snapshot: snapshot, Inference: inference, ConnectionGeneration: account.ConnectionGeneration, Book: book, Publication: publication, Resource: resource, Envelope: envelope, Conflict: conflict}, nil
}

func CardDAVConflictReviewFence(source *CardDAVPublicationReviewSource, body []byte) CardDAVReviewArtifactFence {
	fence := CardDAVCurrentReviewFence(source, body, source.Conflict.Href)
	fence.Kind = CardDAVReviewConflict
	id, revision, etag := source.Conflict.ID, source.Conflict.ReviewRevision, source.Conflict.RemoteETag
	fence.ConflictID, fence.ConflictRevision, fence.RemoteETag = &id, &revision, &etag
	return fence
}

// lockCardDAVConflictReviewTx follows account/book/person/publication/resource/conflict
// order even when the requested conflict belongs to a subscribed non-write book.
func (s *Store) lockCardDAVConflictReviewTx(ctx context.Context, tx *loggedTx, conflictID int64) (*CardDAVPublicationReviewSource, error) {
	identity, err := getCardDAVConflictFrom(ctx, tx, conflictID, "")
	if err != nil {
		return nil, err
	}
	if lock := s.dialect.RowWriterLockSQL("carddav_accounts", "connection_generation"); lock != "" {
		if _, err := tx.ExecContext(ctx, lock, 1); err != nil {
			return nil, err
		}
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM carddav_accounts WHERE id=1`+s.dialect.SelectForUpdate()).Scan(&id); err != nil {
		return nil, err
	}
	if _, err := s.lockCardDAVConflictResolutionBookTx(ctx, tx, identity.AddressBookID); err != nil {
		return nil, err
	}
	// The mapping probe only determines which person must be locked; its identity
	// is checked again after acquiring the resource lock.
	var personID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT person_id FROM carddav_resources WHERE address_book_id=? AND href=?`, identity.AddressBookID, identity.Href).Scan(&personID); err != nil {
		return nil, err
	}
	if !personID.Valid {
		return nil, ErrCardDAVConflictStale
	}
	if s.cardDAVReviewPersonLockHook != nil {
		s.cardDAVReviewPersonLockHook()
	}
	if err := s.lockPersonVCardProjectionTx(ctx, tx, personID.Int64, ""); err != nil {
		return nil, err
	}
	if _, err := getCardDAVPublicationFrom(ctx, tx, personID.Int64, s.dialect.SelectForUpdate()); err != nil && !errors.Is(err, ErrCardDAVPublicationNotFound) {
		return nil, err
	}
	resource, err := s.findCardDAVResourceTx(ctx, tx, identity.AddressBookID, identity.Href)
	if err != nil {
		return nil, err
	}
	if resource.PersonID == nil || *resource.PersonID != personID.Int64 {
		return nil, ErrCardDAVConflictStale
	}
	if _, err := getCardDAVConflictFrom(ctx, tx, conflictID, s.dialect.SelectForUpdate()); err != nil {
		return nil, err
	}
	return s.loadCardDAVConflictReviewSourceTx(ctx, tx, conflictID)
}

func (s *Store) ApproveCardDAVConflictLocalContext(ctx context.Context, plan CardDAVConflictLocalApprovalPlan) error {
	if plan.Fence.ConflictID == nil {
		return ErrCardDAVInvalidPlan
	}
	err := s.withTxOptionsContext(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(tx *loggedTx) error {
		source, err := s.lockCardDAVConflictReviewTx(ctx, tx, *plan.Fence.ConflictID)
		if err != nil {
			return err
		}
		if len(source.Conflict.LocalMutationIntent) > 0 || plan.Fence.Kind != CardDAVReviewConflict || CardDAVReviewToken(plan.Fence) != plan.ApprovalToken || CardDAVReviewToken(CardDAVConflictReviewFence(source, plan.Body)) != plan.ApprovalToken ||
			source.Conflict.MappingRevision != source.Resource.MappingRevision || (source.Publication != nil && source.Publication.PendingOperation != "") {
			return ErrCardDAVReviewStale
		}
		if len(plan.Body)+len(source.Conflict.RemoteBody) > MaxCardDAVConflictSnapshotBytes {
			return ErrCardDAVConflictTooLarge
		}
		if _, err := validateCardDAVPublicationMetadata(plan.Body, plan.EnvelopeMetadata, fmt.Sprintf("carddav:%d", source.Book.ID), source.Resource.Href, source.Person.VCardUID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE carddav_conflicts SET local_body=?, local_hash=?, local_inference_revision=?,
   review_revision=review_revision+1, approved_local_body_sha256=?, approved_local_inference_revision=?,
   approved_conflict_revision=review_revision+1, local_envelope_metadata=?, connection_generation=?, book_sync_revision=?, updated_at=`+s.dialect.Now()+` WHERE id=?`,
			plan.Body, source.Snapshot.Fingerprint, source.Inference.InferenceRevision, CardDAVBodySHA256(plan.Body), source.Inference.InferenceRevision, plan.EnvelopeMetadata, source.ConnectionGeneration, source.Book.SyncRevision, source.Conflict.ID)
		return err
	})
	return s.reviewArtifactError(err)
}

func (c *CardDAVConflict) HasExactLocalApproval(state CardDAVInferenceExportState, generation, bookRevision int64) bool {
	return c.ApprovedLocalBodySHA256 != nil && c.ApprovedLocalInferenceRevision != nil && c.ApprovedConflictRevision != nil && c.LocalInferenceRevision != nil &&
		*c.ApprovedLocalBodySHA256 == CardDAVBodySHA256(c.LocalBody) && *c.ApprovedLocalInferenceRevision == state.InferenceRevision && *c.LocalInferenceRevision == state.InferenceRevision && *c.ApprovedConflictRevision == c.ReviewRevision && c.ConnectionGeneration == generation && c.BookSyncRevision == bookRevision
}
