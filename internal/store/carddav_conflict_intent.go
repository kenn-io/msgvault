package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// cardDAVConflictIntentV1 is a persistence format, deliberately separate from
// the runtime publication plan. Approval scope never changes during recovery.
type cardDAVConflictIntentV1 struct {
	Version                   int                      `json:"version"`
	PersonID                  int64                    `json:"person_id"`
	AddressBookID             int64                    `json:"address_book_id"`
	Href                      string                   `json:"href"`
	Operation                 CardDAVMutationOperation `json:"operation"`
	Body                      []byte                   `json:"body"`
	SemanticHash              string                   `json:"semantic_hash"`
	LocalHash                 string                   `json:"local_hash"`
	EnvelopeMetadata          []byte                   `json:"envelope_metadata"`
	BodySHA256                string                   `json:"body_sha256"`
	InferenceRevision         int64                    `json:"inference_revision"`
	AuthorizationGeneration   int64                    `json:"authorization_generation"`
	AuthorizationBookRevision int64                    `json:"authorization_book_revision"`
	ConnectionGeneration      int64                    `json:"connection_generation"`
	BookSyncRevision          int64                    `json:"book_sync_revision"`
	MappingRevision           int64                    `json:"mapping_revision"`
	PreviousMappingRevision   int64                    `json:"previous_mapping_revision"`
	MutationRevision          int64                    `json:"mutation_revision"`
	RemoteETag                string                   `json:"remote_etag"`
	StartedAt                 time.Time                `json:"started_at"`
}

// LocalMutationPublication decodes the exact conflict-owned operation for
// execution or recovery; it never renders from the current person snapshot.
func (c *CardDAVConflict) LocalMutationPublication() (*CardDAVPublication, error) {
	if len(c.LocalMutationIntent) == 0 {
		return nil, ErrCardDAVPublicationNotFound
	}
	var intent cardDAVConflictIntentV1
	if err := json.Unmarshal(c.LocalMutationIntent, &intent); err != nil {
		return nil, fmt.Errorf("decode CardDAV conflict intent: %w", err)
	}
	if intent.Version != 1 ||
		intent.PersonID <= 0 ||
		intent.AddressBookID != c.AddressBookID ||
		intent.Href != c.Href ||
		(intent.Operation != CardDAVMutationCreate && intent.Operation != CardDAVMutationUpdate) ||
		len(intent.Body) == 0 ||
		intent.BodySHA256 != CardDAVBodySHA256(intent.Body) ||
		intent.MappingRevision <= 0 ||
		intent.MutationRevision <= 0 {
		return nil, ErrCardDAVInvalidPlan
	}
	return &CardDAVPublication{
		ConflictOwned:             true,
		ResolutionConflictID:      c.ID,
		PersonID:                  intent.PersonID,
		AddressBookID:             intent.AddressBookID,
		Href:                      intent.Href,
		PendingOperation:          intent.Operation,
		OutgoingBody:              intent.Body,
		OutgoingSemanticHash:      intent.SemanticHash,
		LocalHash:                 intent.LocalHash,
		OutgoingEnvelopeMetadata:  intent.EnvelopeMetadata,
		ApprovedBodySHA256:        &intent.BodySHA256,
		ApprovedInferenceRevision: &intent.InferenceRevision,
		ApprovedMutationRevision:  &intent.MutationRevision,
		ConnectionGeneration:      intent.ConnectionGeneration,
		BookSyncRevision:          intent.BookSyncRevision,
		MappingRevision:           intent.MappingRevision,
		PreviousMappingRevision:   intent.PreviousMappingRevision,
		MutationRevision:          intent.MutationRevision,
		RemoteETag:                intent.RemoteETag,
		PendingStartedAt:          &intent.StartedAt,
	}, nil
}

func (s *Store) prepareCardDAVConflictIntentTx(ctx context.Context, tx *loggedTx, source *CardDAVPublicationReviewSource, operation CardDAVMutationOperation, etag, semanticHash string) (*CardDAVPublication, error) {
	c := source.Conflict
	intent := cardDAVConflictIntentV1{
		Version:                   1,
		PersonID:                  source.Person.ID,
		AddressBookID:             c.AddressBookID,
		Href:                      c.Href,
		Operation:                 operation,
		Body:                      c.LocalBody,
		SemanticHash:              semanticHash,
		LocalHash:                 c.LocalHash,
		EnvelopeMetadata:          c.LocalEnvelopeMetadata,
		BodySHA256:                CardDAVBodySHA256(c.LocalBody),
		InferenceRevision:         source.Inference.InferenceRevision,
		AuthorizationGeneration:   source.ConnectionGeneration,
		AuthorizationBookRevision: source.Book.SyncRevision,
		ConnectionGeneration:      source.ConnectionGeneration,
		BookSyncRevision:          source.Book.SyncRevision,
		MappingRevision:           source.Resource.MappingRevision + 1,
		PreviousMappingRevision:   source.Resource.MappingRevision,
		MutationRevision:          c.ReviewRevision + 1,
		RemoteETag:                etag,
		StartedAt:                 time.Now().UTC(),
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE carddav_resources SET mapping_revision=? WHERE id=?`, intent.MappingRevision, source.Resource.ID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE carddav_conflicts SET local_mutation_intent=?,review_revision=review_revision+1 WHERE id=?`, encoded, c.ID); err != nil {
		return nil, err
	}
	c.LocalMutationIntent = encoded
	return c.LocalMutationPublication()
}

func (s *Store) lockCardDAVConflictIntentTx(ctx context.Context, tx *loggedTx, pending CardDAVPublication) (*CardDAVPublicationReviewSource, *CardDAVPublication, error) {
	source, err := s.lockCardDAVConflictReviewTx(ctx, tx, pending.ResolutionConflictID)
	if err != nil {
		return nil, nil, err
	}
	current, err := source.Conflict.LocalMutationPublication()
	if err != nil {
		return nil, nil, err
	}
	if source.Conflict.Status != CardDAVConflictUnresolved ||
		source.Person.ID != pending.PersonID ||
		current.PersonID != pending.PersonID ||
		current.AddressBookID != pending.AddressBookID ||
		current.Href != pending.Href ||
		current.MutationRevision != pending.MutationRevision ||
		source.Conflict.ReviewRevision != current.MutationRevision ||
		current.MappingRevision != pending.MappingRevision ||
		source.Resource.MappingRevision != current.MappingRevision ||
		current.ConnectionGeneration != pending.ConnectionGeneration ||
		current.BookSyncRevision != pending.BookSyncRevision ||
		current.RemoteETag != pending.RemoteETag ||
		current.LocalHash != pending.LocalHash ||
		current.OutgoingSemanticHash != pending.OutgoingSemanticHash ||
		!bytes.Equal(current.OutgoingEnvelopeMetadata, pending.OutgoingEnvelopeMetadata) ||
		current.PendingOperation != pending.PendingOperation ||
		!bytes.Equal(current.OutgoingBody, pending.OutgoingBody) {
		return nil, nil, ErrCardDAVStalePlan
	}
	return source, current, nil
}

// RefreshCardDAVConflictLocalIntentContext advances read-only recovery fences
// while preserving the original authorization scope for any conditional retry.
func (s *Store) RefreshCardDAVConflictLocalIntentContext(ctx context.Context, pending CardDAVPublication) (*CardDAVPublication, error) {
	var current *CardDAVPublication
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		source, p, err := s.lockCardDAVConflictIntentTx(ctx, tx, pending)
		if err != nil {
			return err
		}
		var intent cardDAVConflictIntentV1
		if err = json.Unmarshal(source.Conflict.LocalMutationIntent, &intent); err != nil {
			return err
		}
		intent.ConnectionGeneration, intent.BookSyncRevision = source.ConnectionGeneration, source.Book.SyncRevision
		encoded, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE carddav_conflicts SET local_mutation_intent=? WHERE id=?`, encoded, source.Conflict.ID); err != nil {
			return err
		}
		p.ConnectionGeneration, p.BookSyncRevision = source.ConnectionGeneration, source.Book.SyncRevision
		current = p
		return nil
	})
	return current, err
}

func (s *Store) ValidateCardDAVConflictCreateRetryContext(ctx context.Context, pending CardDAVPublication) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		source, current, err := s.lockCardDAVConflictIntentTx(ctx, tx, pending)
		if err != nil {
			return err
		}
		var intent cardDAVConflictIntentV1
		if err = json.Unmarshal(source.Conflict.LocalMutationIntent, &intent); err != nil {
			return err
		}
		// Current inference may have advanced; approval belongs to the captured
		// bytes and never advances the ordinary inference ledger here.
		if current.PendingOperation != CardDAVMutationCreate || !current.HasExactBodyApproval() {
			return ErrCardDAVInferenceReviewRequired
		}
		if intent.AuthorizationGeneration != source.ConnectionGeneration ||
			intent.AuthorizationBookRevision != source.Book.SyncRevision {
			return ErrCardDAVReviewStale
		}
		if !cardDAVConflictBookAllowsMutation(source.Book, CardDAVMutationCreate) {
			return ErrCardDAVNoWriteTarget
		}
		return nil
	})
}

// CommitCardDAVConflictLocalIntentContext records a matching canonical GET; it
// neither sends a write nor grants approval. Use the current recovery fences
// here so a later sync does not strand an already completed remote write.
// ValidateCardDAVConflictCreateRetryContext checks the immutable authorization
// fences before any retry after canonical absence.
func (s *Store) CommitCardDAVConflictLocalIntentContext(ctx context.Context, input CardDAVCanonicalMutation) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		source, current, err := s.lockCardDAVConflictIntentTx(ctx, tx, input.Publication)
		if err != nil {
			return err
		}
		if source.ConnectionGeneration != current.ConnectionGeneration || source.Book.SyncRevision != current.BookSyncRevision {
			return ErrCardDAVStalePlan
		}
		if input.Tombstone || input.Remote.Href != current.Href || input.Remote.SemanticHash != current.OutgoingSemanticHash || len(input.Remote.RemoteBody) == 0 || input.Remote.RemoteETag == "" {
			return ErrCardDAVPublicationMismatch
		}
		// Canonical confirmation settles only captured evidence, even if inference
		// advanced after the write. It never approves that newer inference or W.
		if len(current.OutgoingEnvelopeMetadata) > 0 {
			if err = s.putCardDAVPublicationEnvelopeTx(ctx, tx, current.AddressBookID, current.PersonID, current.Href, current.OutgoingBody, current.OutgoingEnvelopeMetadata, input.Remote.RemoteBody); err != nil {
				return err
			}
		} else if err = s.putCardDAVEnvelopeTx(ctx, tx, current.AddressBookID, current.PersonID, input.Remote); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE carddav_resources SET remote_uid=NULLIF(?,''), remote_etag=?, remote_body=?, remote_semantic_hash=?, local_hash=?, mapping_status=?, governance=?, updated_at=`+s.dialect.Now()+` WHERE id=?`, input.Remote.RemoteUID, input.Remote.RemoteETag, input.Remote.RemoteBody, input.Remote.SemanticHash, current.LocalHash, CardDAVMappingMapped, CardDAVGovernanceLocal, source.Resource.ID)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE carddav_conflicts SET local_mutation_intent=NULL WHERE id=?`, source.Conflict.ID); err != nil {
			return err
		}
		_, err = resolveCardDAVConflictAuditTx(ctx, tx, s.dialect, source.Conflict.ID, CardDAVResolutionKeepLocal)
		return err
	})
}

// RollbackCardDAVConflictLocalIntentContext clears only the fenced rejected intent. It restores the mapping's
// preflight revision so an explicit fresh preview/approval can replace it.
func (s *Store) RollbackCardDAVConflictLocalIntentContext(ctx context.Context, pending CardDAVPublication) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		source, current, err := s.lockCardDAVConflictIntentTx(ctx, tx, pending)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE carddav_resources SET mapping_revision=? WHERE id=?`, current.PreviousMappingRevision, source.Resource.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE carddav_conflicts SET local_mutation_intent=NULL WHERE id=?`, source.Conflict.ID)
		return err
	})
}

// ResetCardDAVConflictLocalIntentContext records a divergent canonical outcome
// under the exact intent fences, retaining historical local bytes for review.
func (s *Store) ResetCardDAVConflictLocalIntentContext(ctx context.Context, pending CardDAVPublication, remote CardDAVRemoteResource, tombstone bool) error {
	if len(remote.RemoteBody)+len(pending.OutgoingBody) > MaxCardDAVConflictSnapshotBytes {
		return ErrCardDAVConflictTooLarge
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		source, current, err := s.lockCardDAVConflictIntentTx(ctx, tx, pending)
		if err != nil {
			return err
		}
		if source.ConnectionGeneration != current.ConnectionGeneration || source.Book.SyncRevision != current.BookSyncRevision {
			return ErrCardDAVStalePlan
		}
		if !tombstone && (remote.Href != current.Href || remote.RemoteETag == "" || len(remote.RemoteBody) == 0) {
			return ErrCardDAVInvalidPlan
		}
		next := current.MappingRevision + 1
		if _, err = tx.ExecContext(ctx, `UPDATE carddav_resources SET mapping_revision=? WHERE id=?`, next, source.Resource.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE carddav_conflicts SET local_mutation_intent=NULL, mapping_revision=?, local_body=?,local_hash=?,local_inference_revision=?, remote_body=?,remote_etag=NULLIF(?,''),remote_tombstone=?, review_revision=review_revision+1, approved_local_body_sha256=NULL,approved_local_inference_revision=NULL,approved_conflict_revision=NULL,local_envelope_metadata=NULL,updated_at=`+s.dialect.Now()+` WHERE id=?`, next, current.OutgoingBody, current.LocalHash, current.ApprovedInferenceRevision, nullableCardDAVMetadata(remote.RemoteBody), remote.RemoteETag, tombstone, source.Conflict.ID)
		return err
	})
}
