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
	"slices"
	"strings"
	"time"
)

const maxPersonOperationIdempotencyKeyBytes = 128

var (
	ErrPersonMergeNotFound          = errors.New("person merge not found")
	ErrPersonSplitNotFound          = errors.New("person split not found")
	ErrPersonMergeInvalid           = errors.New("invalid person merge")
	ErrPersonMergeAlreadySplit      = errors.New("person merge lineage already split")
	ErrPersonMergeLineageConflict   = errors.New("participant consolidation crosses person merge lineage")
	ErrPersonMergeIdempotency       = errors.New("person merge idempotency conflict")
	ErrPersonMergeCandidateState    = errors.New("person merge candidate state conflict")
	ErrPersonMergeCandidateNotFound = errors.New("person merge candidate not found")
	ErrPersonSplitRevision          = errors.New("person split revision conflict")
	ErrPersonSplitIdempotency       = errors.New("person split idempotency conflict")
	ErrPersonSplitParticipants      = errors.New("invalid person split participants")
	ErrPersonSplitOwnership         = errors.New("person merge is not owned by source person")
	ErrPersonSplitReviewed          = errors.New("person merge has accepted review candidates")
)

// PersonMergeRequest identifies the surviving and absorbed profiles and the
// exact revisions the caller reviewed before requesting a merge.
type PersonMergeRequest struct {
	SurvivorID               int64
	AbsorbedID               int64
	ExpectedSurvivorRevision int64
	ExpectedAbsorbedRevision int64
	IdempotencyKey           string
	Actor                    string
}

func (r PersonMergeRequest) validate() error {
	switch {
	case r.SurvivorID <= 0:
		return fmt.Errorf("%w: survivor person ID must be positive", ErrPersonMergeInvalid)
	case r.AbsorbedID <= 0:
		return fmt.Errorf("%w: absorbed person ID must be positive", ErrPersonMergeInvalid)
	case r.SurvivorID == r.AbsorbedID:
		return fmt.Errorf("%w: survivor and absorbed person must differ", ErrPersonMergeInvalid)
	case r.ExpectedSurvivorRevision <= 0:
		return fmt.Errorf("%w: survivor revision must be positive", ErrPersonMergeInvalid)
	case r.ExpectedAbsorbedRevision <= 0:
		return fmt.Errorf("%w: absorbed revision must be positive", ErrPersonMergeInvalid)
	case strings.TrimSpace(r.IdempotencyKey) == "":
		return fmt.Errorf("%w: idempotency key is required", ErrPersonMergeInvalid)
	case len(r.IdempotencyKey) > maxPersonOperationIdempotencyKeyBytes:
		return fmt.Errorf("%w: idempotency key exceeds %d bytes",
			ErrPersonMergeInvalid, maxPersonOperationIdempotencyKeyBytes)
	case strings.TrimSpace(r.Actor) == "":
		return fmt.Errorf("%w: actor is required", ErrPersonMergeInvalid)
	default:
		return nil
	}
}

// PersonSplitRequest restores a merged profile into a newly created person.
// ParticipantIDs selects absorbed-origin lineages; it may be empty when the
// absorbed profile had no participants.
type PersonSplitRequest struct {
	SourcePersonID         int64
	MergeID                int64
	ParticipantIDs         []int64
	ExpectedSourceRevision int64
	IdempotencyKey         string
	Actor                  string
}

func (r PersonSplitRequest) validate() error {
	switch {
	case r.SourcePersonID <= 0:
		return fmt.Errorf("%w: source person ID must be positive", ErrPersonMergeInvalid)
	case r.MergeID <= 0:
		return fmt.Errorf("%w: merge ID must be positive", ErrPersonMergeInvalid)
	case r.ExpectedSourceRevision <= 0:
		return fmt.Errorf("%w: source revision must be positive", ErrPersonMergeInvalid)
	case strings.TrimSpace(r.IdempotencyKey) == "":
		return fmt.Errorf("%w: idempotency key is required", ErrPersonMergeInvalid)
	case len(r.IdempotencyKey) > maxPersonOperationIdempotencyKeyBytes:
		return fmt.Errorf("%w: idempotency key exceeds %d bytes",
			ErrPersonMergeInvalid, maxPersonOperationIdempotencyKeyBytes)
	case strings.TrimSpace(r.Actor) == "":
		return fmt.Errorf("%w: actor is required", ErrPersonMergeInvalid)
	}

	seen := make(map[int64]struct{}, len(r.ParticipantIDs))
	for _, participantID := range r.ParticipantIDs {
		if participantID <= 0 {
			return fmt.Errorf("%w: participant IDs must be positive", ErrPersonMergeInvalid)
		}
		if _, duplicate := seen[participantID]; duplicate {
			return fmt.Errorf("%w: duplicate participant ID %d", ErrPersonMergeInvalid, participantID)
		}
		seen[participantID] = struct{}{}
	}
	return nil
}

func (r PersonSplitRequest) canonicalParticipantIDs() []int64 {
	ids := append([]int64(nil), r.ParticipantIDs...)
	slices.Sort(ids)
	return ids
}

// PersonMerge is the immutable merge header plus its current live lineage
// owner. Historical IDs are retained even after their person roots disappear.
type PersonMerge struct {
	ID                     int64     `json:"id"`
	SurvivorPersonID       int64     `json:"survivor_person_id"`
	AbsorbedPersonID       int64     `json:"absorbed_person_id"`
	CurrentPersonID        *int64    `json:"current_person_id,omitempty"`
	SurvivorVCardUID       string    `json:"survivor_vcard_uid"`
	AbsorbedVCardUID       string    `json:"absorbed_vcard_uid"`
	SurvivorRevisionBefore int64     `json:"survivor_revision_before"`
	AbsorbedRevisionBefore int64     `json:"absorbed_revision_before"`
	SurvivorRevisionAfter  int64     `json:"survivor_revision_after"`
	Actor                  string    `json:"actor"`
	SnapshotVersion        int       `json:"snapshot_version"`
	SnapshotSHA256         string    `json:"snapshot_sha256"`
	CreatedAt              time.Time `json:"created_at"`
}

// PersonMergeReviewCandidate retains both sides of a conflicting
// single-cardinality attribute until a user accepts or rejects the candidate.
type PersonMergeReviewCandidate struct {
	ID                int64      `json:"id"`
	MergeID           int64      `json:"merge_id"`
	PersonID          int64      `json:"person_id"`
	DefinitionID      int64      `json:"definition_id"`
	SurvivorValueID   int64      `json:"survivor_value_id"`
	AbsorbedValueID   int64      `json:"absorbed_value_id"`
	State             string     `json:"state"`
	ResolutionValueID *int64     `json:"resolution_value_id,omitempty"`
	ReviewedBy        *string    `json:"reviewed_by,omitempty"`
	ReviewedAt        *time.Time `json:"reviewed_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

// PersonSplit is the append-only operation header for one lineage split.
type PersonSplit struct {
	ID                   int64     `json:"id"`
	MergeID              int64     `json:"merge_id"`
	SourcePersonID       int64     `json:"source_person_id"`
	NewPersonID          int64     `json:"new_person_id"`
	NewPersonUID         string    `json:"new_person_uid"`
	SourceRevisionBefore int64     `json:"source_revision_before"`
	SourceRevisionAfter  int64     `json:"source_revision_after"`
	Actor                string    `json:"actor"`
	ExactReversal        bool      `json:"exact_reversal"`
	CreatedAt            time.Time `json:"created_at"`
}

// PersonMergeRowRef identifies an operation-journal row without exposing its
// snapshot payload.
type PersonMergeRowRef struct {
	TableName     string `json:"table_name"`
	OriginalRowID *int64 `json:"original_row_id,omitempty"`
	OriginalKey   string `json:"original_row_key"`
	Action        string `json:"action"`
}

// PersonSplitResult is the committed split plus both resulting people and
// any aggregate rows deliberately left on the source by a partial split.
type PersonSplitResult struct {
	Split               PersonSplit         `json:"split"`
	SourcePerson        Person              `json:"source_person"`
	NewPerson           Person              `json:"new_person"`
	ExactReversal       bool                `json:"exact_reversal"`
	UIDAliasDisposition string              `json:"uid_alias_disposition"`
	AmbiguousRows       []PersonMergeRowRef `json:"ambiguous_rows"`
	UnrestoredRows      []PersonMergeRowRef `json:"unrestored_rows"`
	IdentityRevision    int64               `json:"identity_revision"`
	CacheState          string              `json:"cache_state" enum:"ready,stale"`
}

// PersonMergeResult is the committed survivor and its durable operation
// record. ReviewCandidates is empty unless single-cardinality facts conflicted.
type PersonMergeResult struct {
	Person           Person                       `json:"person"`
	Merge            PersonMerge                  `json:"merge"`
	ReviewCandidates []PersonMergeReviewCandidate `json:"review_candidates"`
	IdentityRevision int64                        `json:"identity_revision"`
	CacheState       string                       `json:"cache_state" enum:"ready,stale"`
}

type PersonMergeParticipant struct {
	MergeID       int64  `json:"merge_id"`
	ParticipantID int64  `json:"participant_id"`
	OriginSide    string `json:"origin_side"`
	SplitID       *int64 `json:"split_id,omitempty"`
}

type PersonMergeRow struct {
	MergeID        int64   `json:"merge_id"`
	TableName      string  `json:"table_name"`
	OriginalRowID  *int64  `json:"original_row_id,omitempty"`
	OriginalRowKey string  `json:"original_row_key"`
	CurrentRowID   *int64  `json:"current_row_id,omitempty"`
	CurrentRowKey  *string `json:"current_row_key,omitempty"`
	OriginSide     string  `json:"origin_side"`
	ProvenanceKind string  `json:"provenance_kind"`
	ParticipantID  *int64  `json:"participant_id,omitempty"`
	Action         string  `json:"action"`
	SnapshotPath   string  `json:"snapshot_path"`
	SplitID        *int64  `json:"split_id,omitempty"`
}

type PersonMergeSummary struct {
	Merge                 PersonMerge    `json:"merge"`
	ParticipantCount      int            `json:"participant_count"`
	RowCount              int            `json:"row_count"`
	SplitCount            int            `json:"split_count"`
	PendingCandidateCount int            `json:"pending_candidate_count"`
	RowActionCounts       map[string]int `json:"row_action_counts"`
}

type PersonMergeDetail struct {
	Merge            PersonMerge                  `json:"merge"`
	Participants     []PersonMergeParticipant     `json:"participants"`
	Rows             []PersonMergeRow             `json:"rows"`
	Splits           []PersonSplit                `json:"splits"`
	ReviewCandidates []PersonMergeReviewCandidate `json:"review_candidates"`
}

type PersonMergeSnapshotResponse struct {
	Version int             `json:"version"`
	SHA256  string          `json:"sha256"`
	JSON    json.RawMessage `json:"snapshot"`
}

type PersonMergeCandidateDecision string

const (
	PersonMergeCandidateAccept PersonMergeCandidateDecision = "accept"
	PersonMergeCandidateReject PersonMergeCandidateDecision = "reject"
)

type PersonMergeCandidateDecisionRequest struct {
	CandidateID            int64
	PersonID               int64
	ExpectedPersonRevision int64
	Decision               PersonMergeCandidateDecision
	Actor                  string
}

// PersonMergeCandidateDecisionResult returns the decided candidate together
// with the person revision committed by the same transaction.
type PersonMergeCandidateDecisionResult struct {
	PersonMergeReviewCandidate

	PersonRevision int64
}

func (s *Store) MergePersonsContext(
	ctx context.Context, request PersonMergeRequest,
) (*PersonMergeResult, error) {
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.Actor = strings.TrimSpace(request.Actor)
	if err := request.validate(); err != nil {
		return nil, err
	}
	return retryBusyWrite(ctx, s, "merge persons", func() (*PersonMergeResult, error) {
		return s.mergePersonsOnce(ctx, request)
	})
}

func (s *Store) mergePersonsOnce(
	ctx context.Context, request PersonMergeRequest,
) (*PersonMergeResult, error) {
	requestHash, err := personMergeRequestHash(request)
	if err != nil {
		return nil, err
	}
	var result *PersonMergeResult
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if s.personOperationBeforeIdentityLockHook != nil {
			s.personOperationBeforeIdentityLockHook()
		}
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		replayed, found, err := s.personMergeByIdempotencyKeyTx(
			ctx, tx, request.IdempotencyKey, requestHash,
		)
		if err != nil {
			return err
		}
		if found {
			result = replayed
			return nil
		}
		if err := s.lockMergePeopleTx(ctx, tx, request.SurvivorID, request.AbsorbedID); err != nil {
			return err
		}
		survivor, err := s.getPersonTx(ctx, tx, request.SurvivorID)
		if err != nil {
			return err
		}
		absorbed, err := s.getPersonTx(ctx, tx, request.AbsorbedID)
		if err != nil {
			return err
		}
		if survivor.Revision != request.ExpectedSurvivorRevision ||
			absorbed.Revision != request.ExpectedAbsorbedRevision {
			return ErrPersonRevisionConflict
		}
		if err := s.lockPersonMergeVCardEnvelopesTx(
			ctx, tx, survivor.ID, absorbed.ID,
		); err != nil {
			return err
		}
		if err := ensurePersonMergeCardDAVStateTx(ctx, tx, survivor.ID, absorbed.ID); err != nil {
			return err
		}

		snapshot, err := s.capturePersonMergeSnapshotTx(ctx, tx, survivor.ID, absorbed.ID)
		if err != nil {
			return err
		}
		if s.personMergeAfterSnapshotHook != nil {
			s.personMergeAfterSnapshotHook()
		}
		compressed, snapshotHash, err := encodePersonMergeSnapshot(snapshot)
		if err != nil {
			return err
		}
		mergeID, err := s.insertPersonMergeTx(
			ctx, tx, request, requestHash, *survivor, *absorbed, compressed, snapshotHash,
		)
		if err != nil {
			return err
		}
		if err := recordPersonMergeParticipantsTx(ctx, tx, mergeID, snapshot.Persons); err != nil {
			return err
		}
		if err := recordPersonMergeSnapshotRowsTx(ctx, tx, mergeID, snapshot.Rows); err != nil {
			return err
		}
		if err := s.moveCorePersonProfileTx(
			ctx, tx, mergeID, survivor.ID, absorbed.ID, survivor.VCardUID,
		); err != nil {
			return err
		}
		projectionIDs := []int64{}
		if err := s.reconcilePersonRelationshipsTx(
			ctx, tx, mergeID, survivor.ID, absorbed.ID, &projectionIDs,
		); err != nil {
			return err
		}
		if err := s.reconcilePersonRelationshipReviewsTx(
			ctx, tx, mergeID, survivor.ID, absorbed.ID, &projectionIDs,
		); err != nil {
			return err
		}
		if err := s.bumpPersonVCardProjectionsTx(ctx, tx, projectionIDs...); err != nil {
			return err
		}
		if err := s.reconcilePersonEmploymentsTx(
			ctx, tx, mergeID, survivor.ID, absorbed.ID,
		); err != nil {
			return err
		}
		if err := s.reconcilePersonIdentityCandidatesTx(
			ctx, tx, mergeID, survivor.ID, absorbed.ID,
		); err != nil {
			return err
		}
		if err := s.reconcilePersonDailyNotesTx(
			ctx, tx, mergeID, survivor.ID, absorbed.ID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE person_participants SET person_id = ? WHERE person_id = ?`,
			survivor.ID, absorbed.ID); err != nil {
			return fmt.Errorf("rebind absorbed person participants: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE carddav_resources SET person_id = ? WHERE person_id = ?`,
			survivor.ID, absorbed.ID); err != nil {
			return fmt.Errorf("rebind absorbed CardDAV resources: %w", err)
		}
		identityRevision, err := s.bumpIdentityRevisionContext(ctx, tx)
		if err != nil {
			return err
		}
		accountRevision, err := readAccountIdentityRevision(tx)
		if err != nil {
			return err
		}
		if err := s.reconcilePersonActivityStateTx(
			ctx, tx, survivor.ID, absorbed.ID, ContactRevisions{
				IdentityRevision:        identityRevision,
				AccountIdentityRevision: accountRevision,
			},
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE person_uid_aliases SET surviving_person_id = ? WHERE surviving_person_id = ?`,
			survivor.ID, absorbed.ID); err != nil {
			return fmt.Errorf("retarget absorbed person UID aliases: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE person_merges SET current_person_id = ? WHERE current_person_id = ? AND id <> ?`,
			survivor.ID, absorbed.ID, mergeID); err != nil {
			return fmt.Errorf("retarget prior person merge lineages: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE person_merge_review_candidates SET survivor_person_id = ?
			 WHERE survivor_person_id = ?`, survivor.ID, absorbed.ID); err != nil {
			return fmt.Errorf("retarget prior person merge candidates: %w", err)
		}
		if err := s.rebasePriorPersonMergeReferenceBaselinesTx(
			ctx, tx, mergeID, absorbed.ID, survivor.ID,
		); err != nil {
			return err
		}
		if err := s.markAbsorbedPersonFactPinEventsDeletedTx(
			ctx, tx, mergeID, absorbed.ID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM persons WHERE id = ?`, absorbed.ID); err != nil {
			return fmt.Errorf("delete absorbed person: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_uid_aliases
			(retired_uid, surviving_person_id, reason) VALUES (?, ?, 'merge')`,
			absorbed.VCardUID, survivor.ID); err != nil {
			return fmt.Errorf("retire absorbed person UID: %w", err)
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, survivor.ID); err != nil {
			return err
		}
		if err := s.invalidatePersonEnrichmentIdentitiesAfterRevisionTx(
			ctx, tx, survivor.ID,
		); err != nil {
			return err
		}
		if err := s.recordPersonMergePostRowsTx(
			ctx, tx, mergeID, absorbed.ID, snapshot,
		); err != nil {
			return err
		}
		merge, err := s.getPersonMergeTx(ctx, tx, mergeID)
		if err != nil {
			return err
		}
		result, err = s.personMergeResultTx(ctx, tx, merge)
		if err != nil {
			return err
		}
		result.IdentityRevision = identityRevision
		encodedResult, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("encode person merge result: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE person_merges SET result_json = ?, identity_revision = ? WHERE id = ?`,
			string(encodedResult), identityRevision, mergeID,
		); err != nil {
			return fmt.Errorf("store person merge result: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) markAbsorbedPersonFactPinEventsDeletedTx(
	ctx context.Context, tx *loggedTx, mergeID, absorbedID int64,
) error {
	eventIDs, err := personMergeRowIDsTx(ctx, tx, `SELECT id
		FROM person_fact_pin_events WHERE person_id = ? ORDER BY id`, absorbedID)
	if err != nil {
		return fmt.Errorf("load absorbed person fact pin events: %w", err)
	}
	for _, eventID := range eventIDs {
		if err := s.setPersonMergeRowDispositionTx(
			ctx, tx, mergeID, "person_fact_pin_events", eventID,
			"deleted_snapshot", nil,
		); err != nil {
			return err
		}
	}
	return nil
}

func personMergeRequestHash(request PersonMergeRequest) (string, error) {
	canonical, err := json.Marshal(struct {
		SurvivorID               int64  `json:"survivor_id"`
		AbsorbedID               int64  `json:"absorbed_id"`
		ExpectedSurvivorRevision int64  `json:"expected_survivor_revision"`
		ExpectedAbsorbedRevision int64  `json:"expected_absorbed_revision"`
		Actor                    string `json:"actor"`
	}{
		SurvivorID: request.SurvivorID, AbsorbedID: request.AbsorbedID,
		ExpectedSurvivorRevision: request.ExpectedSurvivorRevision,
		ExpectedAbsorbedRevision: request.ExpectedAbsorbedRevision,
		Actor:                    request.Actor,
	})
	if err != nil {
		return "", fmt.Errorf("encode person merge request: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) personMergeByIdempotencyKeyTx(
	ctx context.Context, tx *loggedTx, key, requestHash string,
) (*PersonMergeResult, bool, error) {
	var mergeID int64
	var storedHash string
	var storedResult sql.NullString
	var storedIdentityRevision sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT id, request_hash, result_json, identity_revision
		FROM person_merges WHERE idempotency_key = ?`, key).Scan(
		&mergeID, &storedHash, &storedResult, &storedIdentityRevision,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load person merge idempotency key: %w", err)
	}
	if storedHash != requestHash {
		return nil, false, ErrPersonMergeIdempotency
	}
	if !storedResult.Valid || storedResult.String == "" {
		return nil, false, errors.New("person merge idempotency result is missing")
	}
	var result PersonMergeResult
	if err := json.Unmarshal([]byte(storedResult.String), &result); err != nil {
		return nil, false, fmt.Errorf("decode person merge idempotency result: %w", err)
	}
	if !storedIdentityRevision.Valid || storedIdentityRevision.Int64 <= 0 ||
		result.IdentityRevision != storedIdentityRevision.Int64 {
		return nil, false, errors.New("person merge idempotency revision is missing or inconsistent")
	}
	return &result, true, nil
}

func (s *Store) lockMergePeopleTx(
	ctx context.Context, tx *loggedTx, survivorID, absorbedID int64,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM persons
		WHERE id IN (?, ?) ORDER BY id`+s.dialect.SelectForUpdate(), survivorID, absorbedID)
	if err != nil {
		return fmt.Errorf("lock merge people: %w", err)
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan locked merge person: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate locked merge people: %w", err)
	}
	if count != 2 {
		return ErrPersonNotFound
	}
	return nil
}

func (s *Store) lockPersonMergeVCardEnvelopesTx(
	ctx context.Context, tx *loggedTx, survivorID, absorbedID int64,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM vcard_resource_envelopes
		WHERE person_id IN (?, ?) ORDER BY id`+s.dialect.SelectForUpdate(),
		survivorID, absorbedID)
	if err != nil {
		return fmt.Errorf("lock person merge vCard envelopes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan locked person merge vCard envelope: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate locked person merge vCard envelopes: %w", err)
	}
	return nil
}

func ensurePersonMergeCardDAVStateTx(
	ctx context.Context, tx *loggedTx, survivorID, absorbedID int64,
) error {
	var published bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM carddav_publications WHERE person_id IN (?, ?)
	)`, survivorID, absorbedID).Scan(&published); err != nil {
		return fmt.Errorf("check person merge CardDAV publications: %w", err)
	}
	if published {
		return ErrPersonCardDAVPublished
	}
	return nil
}

func (s *Store) insertPersonMergeTx(
	ctx context.Context,
	tx *loggedTx,
	request PersonMergeRequest,
	requestHash string,
	survivor, absorbed Person,
	snapshot []byte,
	snapshotHash string,
) (int64, error) {
	var mergeID int64
	err := tx.QueryRowContext(ctx, `INSERT INTO person_merges (
		idempotency_key, request_hash, survivor_person_id_at_merge,
		absorbed_person_id, current_person_id, survivor_uid, absorbed_uid,
		survivor_revision_before, absorbed_revision_before, survivor_revision_after,
		actor, snapshot_version, snapshot_blob, snapshot_sha256
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		request.IdempotencyKey, requestHash, survivor.ID, absorbed.ID, survivor.ID,
		survivor.VCardUID, absorbed.VCardUID, survivor.Revision, absorbed.Revision,
		survivor.Revision+1, request.Actor, personMergeSnapshotVersion, snapshot, snapshotHash,
	).Scan(&mergeID)
	if err != nil {
		if s.dialect.IsConflictError(err) {
			return 0, ErrPersonMergeIdempotency
		}
		return 0, fmt.Errorf("insert person merge: %w", err)
	}
	return mergeID, nil
}

func recordPersonMergeParticipantsTx(
	ctx context.Context,
	tx *loggedTx,
	mergeID int64,
	persons []personMergeSnapshotPerson,
) error {
	for i, person := range persons {
		origin := personMergeOriginSurvivor
		if i == 1 {
			origin = personMergeOriginAbsorbed
		}
		for _, participantID := range person.ParticipantIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO person_merge_participants
				(merge_id, participant_id, origin_side) VALUES (?, ?, ?)`,
				mergeID, participantID, origin); err != nil {
				return fmt.Errorf("record person merge %d participant lineage: %w", mergeID, err)
			}
		}
	}
	return nil
}

func recordPersonMergeSnapshotRowsTx(
	ctx context.Context, tx *loggedTx, mergeID int64, rows []personMergeSnapshotRow,
) error {
	for index, row := range rows {
		if row.OriginSide == personMergeOriginSurvivor &&
			(row.ProvenanceKind == personMergeProvenanceDerived ||
				row.TableName == "activity_event_persons") {
			continue
		}
		var rowID any
		if row.RowID > 0 {
			rowID = row.RowID
		}
		action := "moved"
		switch row.ProvenanceKind {
		case personMergeProvenanceParticipantExact, personMergeProvenanceAbsorbedProfile:
		case personMergeProvenanceInboundReference:
			action = "repointed"
		case personMergeProvenanceDerived:
			action = "recomputed"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_merge_rows (
			merge_id, table_name, original_row_id, original_row_key,
			current_row_id, current_row_key, origin_side, provenance_kind,
			participant_id, action, snapshot_path
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			mergeID, row.TableName, rowID, row.RowKey, rowID, row.RowKey,
			row.OriginSide, row.ProvenanceKind, row.ParticipantID, action,
			fmt.Sprintf("rows/%d", index)); err != nil {
			return fmt.Errorf("record person merge %d row table %s: %w", mergeID, row.TableName, err)
		}
	}
	return nil
}

// recordPersonMergePostRowsTx closes the row journal after reconciliation.
// Absorbed-origin rows always remain in the journal. Survivor-origin rows are
// retained only when the merge changed them, which captures dependency remaps
// without making an exact split rewrite unrelated survivor state.
func (s *Store) recordPersonMergePostRowsTx(
	ctx context.Context,
	tx *loggedTx,
	mergeID, absorbedID int64,
	snapshot personMergeSnapshot,
) error {
	rowsByPath := make(map[string]personMergeSnapshotRow, len(snapshot.Rows))
	for index, row := range snapshot.Rows {
		rowsByPath[fmt.Sprintf("rows/%d", index)] = row
	}
	journalRows, err := tx.QueryContext(ctx, `SELECT table_name, original_row_id, original_row_key,
		current_row_id, current_row_key, origin_side, provenance_kind, action, snapshot_path
		FROM person_merge_rows WHERE merge_id = ?
		ORDER BY table_name, original_row_key`, mergeID)
	if err != nil {
		return fmt.Errorf("load person merge post-state journal: %w", err)
	}
	type pendingRow struct {
		table, originalKey, origin, provenance, action, snapshotPath string
		originalID                                                   sql.NullInt64
		currentID                                                    sql.NullInt64
		currentKey                                                   sql.NullString
	}
	pending := []pendingRow{}
	for journalRows.Next() {
		var row pendingRow
		if err := journalRows.Scan(
			&row.table, &row.originalID, &row.originalKey, &row.currentID, &row.currentKey,
			&row.origin, &row.provenance, &row.action, &row.snapshotPath,
		); err != nil {
			_ = journalRows.Close()
			return fmt.Errorf("scan person merge post-state journal: %w", err)
		}
		pending = append(pending, row)
	}
	if err := journalRows.Err(); err != nil {
		_ = journalRows.Close()
		return fmt.Errorf("iterate person merge post-state journal: %w", err)
	}
	if err := journalRows.Close(); err != nil {
		return fmt.Errorf("close person merge post-state journal: %w", err)
	}

	for _, entry := range pending {
		if entry.action == "deleted_snapshot" || entry.action == "recomputed" ||
			entry.provenance == string(personMergeProvenanceDerived) {
			continue
		}
		original, ok := rowsByPath[entry.snapshotPath]
		if !ok {
			return fmt.Errorf("%w: missing merge snapshot path %q",
				ErrPersonMergeSnapshotCorrupt, entry.snapshotPath)
		}
		spec, ok := personMergeTableRegistry[entry.table]
		if !ok {
			return fmt.Errorf("%w: unregistered merge table %q", ErrPersonMergeInvalid, entry.table)
		}
		where, args, err := personSplitCurrentRowWhere(spec, personSplitJournalRow{
			currentRowID: entry.currentID, currentKey: entry.currentKey,
		})
		if err != nil {
			return err
		}
		selectColumns := "*"
		switch entry.table {
		case "person_merges":
			// The immutable audit payload can be very large. Only the key and
			// mutable person reference participate in later lineage rebasing.
			selectColumns = "id, current_person_id"
		case personMergeReviewCandidatesTableName:
			selectColumns = "id, survivor_person_id, state, reviewed_at"
		}
		currentRows, err := s.capturePersonMergeQueryTx(ctx, tx, spec,
			`SELECT `+selectColumns+` FROM `+personSplitIdentifier(entry.table)+` WHERE `+where,
			args, absorbedID)
		if err != nil {
			return err
		}
		if len(currentRows) != 1 {
			return fmt.Errorf("%w: current %s row is missing", ErrPersonMergeInvalid, entry.table)
		}
		postJSON, err := json.Marshal(currentRows[0])
		if err != nil {
			return fmt.Errorf("encode %s merge post-state: %w", entry.table, err)
		}
		originalColumns, err := json.Marshal(original.Columns)
		if err != nil {
			return fmt.Errorf("encode %s merge original state: %w", entry.table, err)
		}
		postColumns, err := json.Marshal(currentRows[0].Columns)
		if err != nil {
			return fmt.Errorf("encode %s merge current state: %w", entry.table, err)
		}
		if entry.origin == string(personMergeOriginSurvivor) &&
			bytes.Equal(originalColumns, postColumns) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM person_merge_rows
				WHERE merge_id = ? AND table_name = ? AND original_row_key = ?`,
				mergeID, entry.table, entry.originalKey); err != nil {
				return fmt.Errorf("prune unchanged survivor merge row: %w", err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_merge_rows
			SET post_merge_row_json = ?
			WHERE merge_id = ? AND table_name = ? AND original_row_key = ?`,
			string(postJSON), mergeID, entry.table, entry.originalKey); err != nil {
			return fmt.Errorf("record %s merge post-state: %w", entry.table, err)
		}
		if err := syncPersonMergeRowPersonRefsTx(
			ctx, tx, mergeID, entry.table, entry.originalKey, currentRows[0], spec,
		); err != nil {
			return err
		}
	}
	return nil
}

func syncPersonMergeRowPersonRefsTx(
	ctx context.Context, tx *loggedTx, mergeID int64, table, originalKey string,
	row personMergeSnapshotRow, spec personMergeTableSpec,
) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_merge_row_person_refs
		WHERE merge_id = ? AND table_name = ? AND original_row_key = ?`,
		mergeID, table, originalKey); err != nil {
		return fmt.Errorf("clear %s merge person references: %w", table, err)
	}
	for _, reference := range spec.PersonReferences {
		if reference.Kind == personMergeReferencePolymorphic &&
			personSplitSnapshotRowText(row, reference.KindColumn) != reference.KindValue {
			continue
		}
		personID, ok := personMergeSnapshotRowInteger(row, reference.IDColumn)
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_merge_row_person_refs
			(merge_id, table_name, original_row_key, column_name, person_id)
			VALUES (?, ?, ?, ?, ?)`, mergeID, table, originalKey,
			reference.IDColumn, personID); err != nil {
			return fmt.Errorf("record %s merge person reference: %w", table, err)
		}
	}
	return nil
}

func (s *Store) moveCorePersonProfileTx(
	ctx context.Context,
	tx *loggedTx,
	mergeID, survivorID, absorbedID int64,
	survivorUID string,
) error {
	for _, table := range []string{
		"person_names", personContactPointsTableName, "person_addresses", "person_dates", "person_media",
	} {
		if err := s.moveStructuredPersonRowsTx(ctx, tx, mergeID, table, survivorID, absorbedID); err != nil {
			return err
		}
	}
	if err := s.movePersonCategoriesTx(ctx, tx, mergeID, survivorID, absorbedID); err != nil {
		return err
	}
	if err := s.movePersonAttributesTx(ctx, tx, mergeID, survivorID, absorbedID); err != nil {
		return err
	}
	if err := s.reconcilePersonTrackingTx(ctx, tx, mergeID, survivorID, absorbedID); err != nil {
		return err
	}
	projectionPersonIDs, err := personMergeRowIDsTx(ctx, tx, `SELECT person_id
		FROM person_attribute_values
		WHERE value_record_type = 'person' AND value_record_id = ?
		  AND person_id NOT IN (?, ?)
		GROUP BY person_id
		ORDER BY person_id`, absorbedID, survivorID, absorbedID)
	if err != nil {
		return fmt.Errorf("load inbound person attribute owners: %w", err)
	}
	employedPersonIDs, err := personMergeRowIDsTx(ctx, tx, `SELECT employment.person_id
		FROM employments employment
		WHERE employment.person_id NOT IN (?, ?)
		  AND EXISTS (
			SELECT 1 FROM organization_attribute_values value
			WHERE value.organization_id = employment.organization_id
			  AND value.value_record_type = 'person' AND value.value_record_id = ?
		  )
		GROUP BY employment.person_id
		ORDER BY employment.person_id`, absorbedID, survivorID, absorbedID)
	if err != nil {
		return fmt.Errorf("load inbound organization attribute projections: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_attribute_values
		SET value_record_id = ? WHERE value_record_type = 'person' AND value_record_id = ?`,
		survivorID, absorbedID); err != nil {
		return fmt.Errorf("repoint person attribute references: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE organization_attribute_values
		SET value_record_id = ? WHERE value_record_type = 'person' AND value_record_id = ?`,
		survivorID, absorbedID); err != nil {
		return fmt.Errorf("repoint organization attribute references: %w", err)
	}
	if err := s.bumpPersonVCardProjectionsTx(
		ctx, tx, append(projectionPersonIDs, employedPersonIDs...)...,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE vcard_resource_envelopes
		SET person_id = ?, canonical_person_uid = ?, revision = revision + 1,
		    updated_at = `+s.dialect.Now()+`
		WHERE person_id = ?`, survivorID, survivorUID, absorbedID); err != nil {
		return fmt.Errorf("move absorbed vCard resources: %w", err)
	}
	return nil
}

func (s *Store) reconcilePersonTrackingTx(
	ctx context.Context, tx *loggedTx, mergeID, survivorID, absorbedID int64,
) error {
	var survivorTracked, absorbedTracked bool
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM person_tracking WHERE person_id = ?),
		EXISTS (SELECT 1 FROM person_tracking WHERE person_id = ?)`,
		survivorID, absorbedID,
	).Scan(&survivorTracked, &absorbedTracked); err != nil {
		return fmt.Errorf("inspect person tracking before merge: %w", err)
	}
	if !absorbedTracked {
		return nil
	}
	if survivorTracked {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM person_tracking WHERE person_id = ?`, absorbedID); err != nil {
			return fmt.Errorf("deduplicate absorbed person tracking: %w", err)
		}
		return s.setPersonMergeRowDispositionTx(
			ctx, tx, mergeID, "person_tracking", absorbedID, personMergeActionDeduplicated, &survivorID,
		)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_tracking
		SET person_id = ? WHERE person_id = ?`, survivorID, absorbedID); err != nil {
		return fmt.Errorf("move absorbed person tracking: %w", err)
	}
	return s.setPersonMergeRowDispositionTx(
		ctx, tx, mergeID, "person_tracking", absorbedID, "moved", &survivorID,
	)
}

func (s *Store) moveStructuredPersonRowsTx(
	ctx context.Context, tx *loggedTx, mergeID int64, table string, survivorID, absorbedID int64,
) error {
	if _, ok := map[string]struct{}{
		"person_names": {}, personContactPointsTableName: {}, "person_addresses": {},
		"person_dates": {}, "person_media": {},
	}[table]; !ok {
		return fmt.Errorf("%w: unregistered structured table %q", ErrPersonMergeInvalid, table)
	}
	duplicatePredicate := fmt.Sprintf(`person_id = ? AND superseded_at IS NULL
		  AND source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL
		  AND EXISTS (SELECT 1 FROM %s survivor
		      WHERE survivor.person_id = ?
		        AND survivor.superseded_at IS NULL
		        AND survivor.source = %s.source
		        AND survivor.source_ref IS NOT DISTINCT FROM %s.source_ref
		        AND COALESCE(survivor.source_resource_uid, '') =
		            COALESCE(%s.source_resource_uid, '')
		        AND survivor.vcard_property IS NOT DISTINCT FROM %s.vcard_property
		        AND survivor.vcard_prop_id IS NOT DISTINCT FROM %s.vcard_prop_id)`,
		table, table, table, table, table, table)
	duplicateIDs, err := personMergeRowIDsTx(ctx, tx,
		fmt.Sprintf(`SELECT id FROM %s WHERE %s ORDER BY id`, table, duplicatePredicate),
		absorbedID, survivorID)
	if err != nil {
		return fmt.Errorf("load duplicate %s rows: %w", table, err)
	}
	query := fmt.Sprintf(`UPDATE %s SET
		active_until = COALESCE(active_until,
			CASE WHEN %s < active_from THEN active_from ELSE %s END),
		superseded_at = COALESCE(superseded_at, %s)
		WHERE %s`, table, s.dialect.Now(), s.dialect.Now(), s.dialect.Now(), duplicatePredicate)
	if _, err := tx.ExecContext(ctx, query, absorbedID, survivorID); err != nil {
		return fmt.Errorf("supersede duplicate %s rows: %w", table, err)
	}
	if err := markPersonMergeRowsDeduplicatedTx(ctx, tx, mergeID, table, duplicateIDs); err != nil {
		return err
	}
	var maxOrdinal int64
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COALESCE(MAX(ordinal), -1)
		FROM %s WHERE person_id = ? AND active_until IS NULL AND superseded_at IS NULL`, table),
		survivorID).Scan(&maxOrdinal)
	if err != nil {
		return fmt.Errorf("read survivor %s ordinal: %w", table, err)
	}
	if maxOrdinal >= 0 {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET ordinal = ordinal + ?
			WHERE person_id = ? AND active_until IS NULL AND superseded_at IS NULL`, table),
			maxOrdinal+1, absorbedID); err != nil {
			return fmt.Errorf("reordinal absorbed %s rows: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET person_id = ? WHERE person_id = ?`, table),
		survivorID, absorbedID); err != nil {
		return fmt.Errorf("move absorbed %s rows: %w", table, err)
	}
	return nil
}

func (s *Store) movePersonCategoriesTx(
	ctx context.Context, tx *loggedTx, mergeID, survivorID, absorbedID int64,
) error {
	duplicateIDs, err := personMergeRowIDsTx(ctx, tx, `SELECT id FROM person_categories
		WHERE person_id = ? AND active_until IS NULL AND superseded_at IS NULL
		  AND EXISTS (SELECT 1 FROM person_categories survivor
		      WHERE survivor.person_id = ?
		        AND survivor.normalized_value = person_categories.normalized_value
		        AND survivor.active_until IS NULL AND survivor.superseded_at IS NULL)
		ORDER BY id`, absorbedID, survivorID)
	if err != nil {
		return fmt.Errorf("load duplicate absorbed categories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_categories SET
		active_until = COALESCE(active_until,
			CASE WHEN `+s.dialect.Now()+` < active_from THEN active_from ELSE `+s.dialect.Now()+` END),
		superseded_at = COALESCE(superseded_at, `+s.dialect.Now()+`)
		WHERE person_id = ? AND active_until IS NULL AND superseded_at IS NULL
		  AND EXISTS (SELECT 1 FROM person_categories survivor
		      WHERE survivor.person_id = ?
		        AND survivor.normalized_value = person_categories.normalized_value
		        AND survivor.active_until IS NULL AND survivor.superseded_at IS NULL)`,
		absorbedID, survivorID); err != nil {
		return fmt.Errorf("supersede duplicate absorbed categories: %w", err)
	}
	if err := markPersonMergeRowsDeduplicatedTx(
		ctx, tx, mergeID, "person_categories", duplicateIDs,
	); err != nil {
		return err
	}
	var maxOrdinal int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal), -1)
		FROM person_categories
		WHERE person_id = ? AND active_until IS NULL AND superseded_at IS NULL`,
		survivorID).Scan(&maxOrdinal); err != nil {
		return fmt.Errorf("read survivor category ordinal: %w", err)
	}
	if maxOrdinal >= 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE person_categories SET ordinal = ordinal + ?
			WHERE person_id = ? AND active_until IS NULL AND superseded_at IS NULL`,
			maxOrdinal+1, absorbedID); err != nil {
			return fmt.Errorf("reordinal absorbed categories: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE person_categories SET person_id = ? WHERE person_id = ?`,
		survivorID, absorbedID); err != nil {
		return fmt.Errorf("move absorbed categories: %w", err)
	}
	return nil
}

func (s *Store) movePersonAttributesTx(
	ctx context.Context, tx *loggedTx, mergeID, survivorID, absorbedID int64,
) error {
	singleValueLockClause := s.dialect.SelectForUpdate()
	if singleValueLockClause != "" {
		singleValueLockClause += " OF a"
	}
	rows, err := tx.QueryContext(ctx, `SELECT
		a.id, a.definition_id, survivor.id,
		CASE WHEN survivor.id IS NULL THEN FALSE ELSE
		  a.value_text IS NOT DISTINCT FROM survivor.value_text AND
		  a.value_integer IS NOT DISTINCT FROM survivor.value_integer AND
		  a.value_real IS NOT DISTINCT FROM survivor.value_real AND
		  a.value_boolean IS NOT DISTINCT FROM survivor.value_boolean AND
		  a.value_date IS NOT DISTINCT FROM survivor.value_date AND
		  a.value_timestamp IS NOT DISTINCT FROM survivor.value_timestamp AND
		  a.value_record_type IS NOT DISTINCT FROM survivor.value_record_type AND
		  a.value_record_id IS NOT DISTINCT FROM survivor.value_record_id
		END,
		a.value_json, survivor.value_json
		FROM person_attribute_values a
		JOIN attribute_definitions definition ON definition.id = a.definition_id
		LEFT JOIN person_attribute_values survivor
		  ON survivor.person_id = ? AND survivor.definition_id = a.definition_id
		 AND survivor.ordinal = a.ordinal
		 AND survivor.active_until IS NULL AND survivor.superseded_at IS NULL
		WHERE a.person_id = ? AND definition.cardinality = 'single'
		  AND a.active_until IS NULL AND a.superseded_at IS NULL
		ORDER BY a.definition_id, a.id`+singleValueLockClause, survivorID, absorbedID)
	if err != nil {
		return fmt.Errorf("load absorbed single attributes: %w", err)
	}
	type conflict struct {
		absorbedValueID int64
		definitionID    int64
		survivorValueID sql.NullInt64
		equal           bool
		absorbedJSON    sql.NullString
		survivorJSON    sql.NullString
	}
	conflicts := []conflict{}
	for rows.Next() {
		var value conflict
		if err := rows.Scan(
			&value.absorbedValueID, &value.definitionID,
			&value.survivorValueID, &value.equal,
			&value.absorbedJSON, &value.survivorJSON,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan absorbed single attribute: %w", err)
		}
		jsonEqual, err := personMergeJSONValuesEqual(value.absorbedJSON, value.survivorJSON)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("compare single attribute JSON: %w", err)
		}
		value.equal = value.equal && jsonEqual
		conflicts = append(conflicts, value)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate absorbed single attributes: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close absorbed single attributes: %w", err)
	}
	equalValueIDs := []int64{}
	for _, value := range conflicts {
		if !value.survivorValueID.Valid {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_attribute_values SET
			active_until = COALESCE(active_until,
				CASE WHEN `+s.dialect.Now()+` < active_from THEN active_from ELSE `+s.dialect.Now()+` END),
			superseded_at = COALESCE(superseded_at, `+s.dialect.Now()+`)
			WHERE id = ?`, value.absorbedValueID); err != nil {
			return fmt.Errorf("retain absorbed single attribute history: %w", err)
		}
		if value.equal {
			equalValueIDs = append(equalValueIDs, value.absorbedValueID)
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_merge_review_candidates (
			merge_id, survivor_person_id, definition_id,
			survivor_value_id, absorbed_value_id
		) VALUES (?, ?, ?, ?, ?)`, mergeID, survivorID, value.definitionID,
			value.survivorValueID.Int64, value.absorbedValueID); err != nil {
			return fmt.Errorf("record person merge attribute candidate: %w", err)
		}
	}
	if err := markPersonMergeRowsDeduplicatedTx(
		ctx, tx, mergeID, personAttributeValuesTableName, equalValueIDs,
	); err != nil {
		return err
	}

	multiValueLockClause := s.dialect.SelectForUpdate()
	if multiValueLockClause != "" {
		multiValueLockClause += " OF absorbed, survivor"
	}
	duplicateRows, err := tx.QueryContext(ctx, `SELECT absorbed.id, survivor.id,
		absorbed.value_json, survivor.value_json
		FROM person_attribute_values absorbed
		JOIN attribute_definitions definition
		  ON definition.id = absorbed.definition_id AND definition.cardinality = 'multi'
		JOIN person_attribute_values survivor
		  ON survivor.person_id = ? AND survivor.definition_id = absorbed.definition_id
		 AND survivor.active_until IS NULL AND survivor.superseded_at IS NULL
		 AND absorbed.value_text IS NOT DISTINCT FROM survivor.value_text
		 AND absorbed.value_integer IS NOT DISTINCT FROM survivor.value_integer
		 AND absorbed.value_real IS NOT DISTINCT FROM survivor.value_real
		 AND absorbed.value_boolean IS NOT DISTINCT FROM survivor.value_boolean
		 AND absorbed.value_date IS NOT DISTINCT FROM survivor.value_date
		 AND absorbed.value_timestamp IS NOT DISTINCT FROM survivor.value_timestamp
		 AND absorbed.value_record_type IS NOT DISTINCT FROM survivor.value_record_type
		 AND absorbed.value_record_id IS NOT DISTINCT FROM survivor.value_record_id
		WHERE absorbed.person_id = ?
		  AND absorbed.active_until IS NULL AND absorbed.superseded_at IS NULL
		ORDER BY absorbed.id, survivor.id`+multiValueLockClause, survivorID, absorbedID)
	if err != nil {
		return fmt.Errorf("load duplicate absorbed multi attributes: %w", err)
	}
	duplicateValueIDs := []int64{}
	seenDuplicate := map[int64]struct{}{}
	for duplicateRows.Next() {
		var absorbedValueID, survivorValueID int64
		var absorbedJSON, survivorJSON sql.NullString
		if err := duplicateRows.Scan(
			&absorbedValueID, &survivorValueID, &absorbedJSON, &survivorJSON,
		); err != nil {
			_ = duplicateRows.Close()
			return fmt.Errorf("scan duplicate absorbed multi attribute: %w", err)
		}
		if _, seen := seenDuplicate[absorbedValueID]; seen {
			continue
		}
		equal, err := personMergeJSONValuesEqual(absorbedJSON, survivorJSON)
		if err != nil {
			_ = duplicateRows.Close()
			return fmt.Errorf("compare multi attribute JSON: %w", err)
		}
		if equal {
			seenDuplicate[absorbedValueID] = struct{}{}
			duplicateValueIDs = append(duplicateValueIDs, absorbedValueID)
		}
	}
	if err := duplicateRows.Err(); err != nil {
		_ = duplicateRows.Close()
		return fmt.Errorf("iterate duplicate absorbed multi attributes: %w", err)
	}
	if err := duplicateRows.Close(); err != nil {
		return fmt.Errorf("close duplicate absorbed multi attributes: %w", err)
	}
	for _, valueID := range duplicateValueIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE person_attribute_values SET
			active_until = COALESCE(active_until,
				CASE WHEN `+s.dialect.Now()+` < active_from THEN active_from ELSE `+s.dialect.Now()+` END),
			superseded_at = COALESCE(superseded_at, `+s.dialect.Now()+`)
			WHERE id = ?`, valueID); err != nil {
			return fmt.Errorf("retain duplicate absorbed multi attribute history: %w", err)
		}
	}
	if err := markPersonMergeRowsDeduplicatedTx(
		ctx, tx, mergeID, personAttributeValuesTableName,
		duplicateValueIDs,
	); err != nil {
		return err
	}

	multiRows, err := tx.QueryContext(ctx, `SELECT a.definition_id,
		COALESCE((SELECT MAX(survivor.ordinal) FROM person_attribute_values survivor
		  WHERE survivor.person_id = ? AND survivor.definition_id = a.definition_id
		    AND survivor.active_until IS NULL AND survivor.superseded_at IS NULL), -1)
		FROM person_attribute_values a
		WHERE a.person_id = ?
		  AND EXISTS (SELECT 1 FROM attribute_definitions definition
			WHERE definition.id = a.definition_id AND definition.cardinality = 'multi')
		  AND a.active_until IS NULL AND a.superseded_at IS NULL
		GROUP BY a.definition_id
		ORDER BY a.definition_id`, survivorID, absorbedID)
	if err != nil {
		return fmt.Errorf("load absorbed multi attribute ordinals: %w", err)
	}
	type offset struct{ definitionID, maxOrdinal int64 }
	offsets := []offset{}
	for multiRows.Next() {
		var value offset
		if err := multiRows.Scan(&value.definitionID, &value.maxOrdinal); err != nil {
			_ = multiRows.Close()
			return fmt.Errorf("scan absorbed multi attribute ordinal: %w", err)
		}
		offsets = append(offsets, value)
	}
	if err := multiRows.Err(); err != nil {
		_ = multiRows.Close()
		return fmt.Errorf("iterate absorbed multi attribute ordinals: %w", err)
	}
	if err := multiRows.Close(); err != nil {
		return fmt.Errorf("close absorbed multi attribute ordinals: %w", err)
	}
	for _, value := range offsets {
		if value.maxOrdinal < 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_attribute_values
			SET ordinal = ordinal + ?
			WHERE person_id = ? AND definition_id = ?
			  AND active_until IS NULL AND superseded_at IS NULL`,
			value.maxOrdinal+1, absorbedID, value.definitionID); err != nil {
			return fmt.Errorf("reordinal absorbed multi attributes: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE person_attribute_values SET person_id = ? WHERE person_id = ?`,
		survivorID, absorbedID); err != nil {
		return fmt.Errorf("move absorbed person attributes: %w", err)
	}
	return nil
}

func personMergeJSONValuesEqual(left, right sql.NullString) (bool, error) {
	if left.Valid != right.Valid {
		return false, nil
	}
	if !left.Valid {
		return true, nil
	}
	leftValue, err := normalizePersonMergeSnapshotValue(left.String, "JSON")
	if err != nil {
		return false, err
	}
	rightValue, err := normalizePersonMergeSnapshotValue(right.String, "JSON")
	if err != nil {
		return false, err
	}
	return leftValue.Text != nil && rightValue.Text != nil && *leftValue.Text == *rightValue.Text, nil
}

func personMergeRowIDsTx(
	ctx context.Context, tx *loggedTx, query string, args ...any,
) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func markPersonMergeRowsDeduplicatedTx(
	ctx context.Context,
	tx *loggedTx,
	mergeID int64,
	table string,
	rowIDs []int64,
) error {
	if len(rowIDs) == 0 {
		return nil
	}
	args := []any{personMergeActionDeduplicated, mergeID, table}
	for _, rowID := range rowIDs {
		args = append(args, rowID)
	}
	_, err := tx.ExecContext(ctx, `UPDATE person_merge_rows SET action = ?
		WHERE merge_id = ? AND table_name = ? AND original_row_id IN (`+
		personMergeSnapshotPlaceholders(len(rowIDs))+`)`, args...)
	if err != nil {
		return fmt.Errorf("mark %s merge rows deduplicated: %w", table, err)
	}
	return nil
}

func (s *Store) rebasePriorPersonMergeReferenceBaselinesTx(
	ctx context.Context,
	tx *loggedTx,
	mergeID, absorbedID, survivorID int64,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT merge_id, table_name, original_row_key,
		current_row_id, current_row_key, post_merge_row_json
		FROM person_merge_rows
		WHERE merge_id <> ? AND split_id IS NULL AND post_merge_row_json IS NOT NULL
		  AND EXISTS (SELECT 1 FROM person_merge_row_person_refs reference
			WHERE reference.merge_id = person_merge_rows.merge_id
			  AND reference.table_name = person_merge_rows.table_name
			  AND reference.original_row_key = person_merge_rows.original_row_key
			  AND reference.person_id = ?)
		ORDER BY merge_id, table_name, original_row_key`, mergeID, absorbedID)
	if err != nil {
		return fmt.Errorf("load prior person-reference merge journals: %w", err)
	}
	type priorRow struct {
		mergeID       int64
		table         string
		originalKey   string
		currentRowID  sql.NullInt64
		currentRowKey sql.NullString
		postMergeJSON sql.NullString
	}
	prior := []priorRow{}
	for rows.Next() {
		var row priorRow
		if err := rows.Scan(&row.mergeID, &row.table, &row.originalKey,
			&row.currentRowID, &row.currentRowKey, &row.postMergeJSON); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan prior person-reference merge journal: %w", err)
		}
		prior = append(prior, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate prior person-reference merge journals: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close prior person-reference merge journals: %w", err)
	}

	for _, row := range prior {
		spec, ok := personMergeTableRegistry[row.table]
		if !ok {
			return fmt.Errorf("%w: unregistered merge table %q", ErrPersonMergeInvalid, row.table)
		}
		if !row.currentRowID.Valid && !row.currentRowKey.Valid {
			continue
		}
		where, args, err := personSplitCurrentRowWhere(spec, personSplitJournalRow{
			currentRowID: row.currentRowID, currentKey: row.currentRowKey,
		})
		if err != nil {
			return err
		}
		current, err := s.capturePersonMergeQueryTx(ctx, tx, spec,
			`SELECT * FROM `+personSplitIdentifier(row.table)+` WHERE `+where, args, absorbedID)
		if err != nil {
			return err
		}
		if len(current) == 0 {
			continue
		}
		if len(current) != 1 {
			return fmt.Errorf("%w: current %s row is ambiguous", ErrPersonMergeInvalid, row.table)
		}
		rebased, changed, err := rebasePersonMergePostRowReferences(
			row.postMergeJSON, current[0], spec, map[int64]int64{absorbedID: survivorID},
		)
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_merge_rows
			SET post_merge_row_json = ?
			WHERE merge_id = ? AND table_name = ? AND original_row_key = ?
			  AND split_id IS NULL`, rebased, row.mergeID, row.table, row.originalKey); err != nil {
			return fmt.Errorf("rebase prior %s person-reference journal: %w", row.table, err)
		}
		if err := syncPersonMergeRowPersonRefsTx(
			ctx, tx, row.mergeID, row.table, row.originalKey, current[0], spec,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) setPersonMergeRowDispositionTx(
	ctx context.Context,
	tx *loggedTx,
	mergeID int64,
	table string,
	originalRowID int64,
	action string,
	currentRowID *int64,
) error {
	var currentRowKey any
	if currentRowID != nil {
		spec, ok := personMergeTableRegistry[table]
		if !ok || len(spec.keyColumns()) != 1 {
			return fmt.Errorf("%w: invalid %s integer row key", ErrPersonMergeInvalid, table)
		}
		keyColumn := spec.keyColumns()[0]
		key, err := canonicalPersonMergeSnapshotRowKey([]personMergeSnapshotColumn{{
			Name: keyColumn,
			Value: personMergeSnapshotValue{
				Kind: personMergeSnapshotInteger, Integer: currentRowID,
			},
		}}, []string{keyColumn})
		if err != nil {
			return fmt.Errorf("encode %s current merge row key: %w", table, err)
		}
		currentRowKey = key
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_merge_rows
		SET action = CASE
			WHEN ? IN ('deduplicated', 'deleted_snapshot') THEN ?
			ELSE action
		END,
		current_row_id = ?, current_row_key = ?
		WHERE merge_id <> ? AND table_name = ? AND current_row_id = ?
		  AND split_id IS NULL`,
		action, action, currentRowID, currentRowKey, mergeID, table, originalRowID); err != nil {
		return fmt.Errorf("rebase prior %s merge row journal: %w", table, err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE person_merge_rows
		SET action = ?, current_row_id = ?, current_row_key = ?
		WHERE merge_id = ? AND table_name = ? AND original_row_id = ?`,
		action, currentRowID, currentRowKey, mergeID, table, originalRowID)
	if err != nil {
		return fmt.Errorf("set %s merge row disposition: %w", table, err)
	}
	if affected, affectedErr := result.RowsAffected(); affectedErr != nil {
		return fmt.Errorf("count %s merge row disposition: %w", table, affectedErr)
	} else if affected != 1 {
		return fmt.Errorf("%w: missing %s merge row journal", ErrPersonMergeInvalid, table)
	}
	return nil
}

func (s *Store) setPersonMergeRowKeyDispositionTx(
	ctx context.Context,
	tx *loggedTx,
	mergeID int64,
	table, originalRowKey, action string,
	currentRowKey *string,
) error {
	if _, err := tx.ExecContext(ctx, `UPDATE person_merge_rows
		SET action = CASE
			WHEN ? IN ('deduplicated', 'deleted_snapshot') THEN ?
			ELSE action
		END,
		current_row_id = NULL, current_row_key = ?
		WHERE merge_id <> ? AND table_name = ? AND current_row_key = ?
		  AND split_id IS NULL`,
		action, action, currentRowKey, mergeID, table, originalRowKey); err != nil {
		return fmt.Errorf("rebase prior %s merge row key journal: %w", table, err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE person_merge_rows
		SET action = ?, current_row_id = NULL, current_row_key = ?
		WHERE merge_id = ? AND table_name = ? AND original_row_key = ?`,
		action, currentRowKey, mergeID, table, originalRowKey)
	if err != nil {
		return fmt.Errorf("set %s merge row key disposition: %w", table, err)
	}
	if affected, affectedErr := result.RowsAffected(); affectedErr != nil {
		return fmt.Errorf("count %s merge row key disposition: %w", table, affectedErr)
	} else if affected != 1 {
		return fmt.Errorf("%w: missing %s merge row key journal", ErrPersonMergeInvalid, table)
	}
	return nil
}

func personMergeIntegerRowKey(names []string, values ...int64) (string, error) {
	if len(names) != len(values) || len(names) == 0 {
		return "", fmt.Errorf("%w: invalid integer row key", ErrPersonMergeInvalid)
	}
	columns := make([]personMergeSnapshotColumn, len(names))
	for index := range names {
		value := values[index]
		columns[index] = personMergeSnapshotColumn{
			Name: names[index],
			Value: personMergeSnapshotValue{
				Kind: personMergeSnapshotInteger, Integer: &value,
			},
		}
	}
	return canonicalPersonMergeSnapshotRowKey(columns, names)
}

func (s *Store) reconcilePersonRelationshipsTx(
	ctx context.Context, tx *loggedTx, mergeID, survivorID, absorbedID int64,
	projectionIDs *[]int64,
) error {
	reviewOwnerIDs, err := personMergeRowIDsTx(ctx, tx, `SELECT review.person_id
		FROM person_relationship_reviews review
		WHERE EXISTS (SELECT 1 FROM person_relationships relationship
			WHERE relationship.id = review.accepted_relationship_id
			  AND (relationship.source_person_id = ? OR relationship.target_person_id = ?))
		  AND review.person_id NOT IN (?, ?)
		GROUP BY review.person_id
		ORDER BY review.person_id`, absorbedID, absorbedID, survivorID, absorbedID)
	if err != nil {
		return fmt.Errorf("load relationship review projection owners: %w", err)
	}
	*projectionIDs = append(*projectionIDs, reviewOwnerIDs...)
	rows, err := tx.QueryContext(ctx, `SELECT
		r.id, r.source_person_id, r.target_person_id, r.relationship_type_id,
		r.end_year, t.is_symmetric
		FROM person_relationships r
		JOIN relationship_types t ON t.id = r.relationship_type_id
		WHERE r.source_person_id = ? OR r.target_person_id = ?
		ORDER BY r.id`, absorbedID, absorbedID)
	if err != nil {
		return fmt.Errorf("load absorbed person relationships: %w", err)
	}
	type relationshipMove struct {
		id, sourceID, targetID, typeID int64
		endYear                        sql.NullInt64
		symmetric                      bool
	}
	moves := []relationshipMove{}
	for rows.Next() {
		var move relationshipMove
		if err := rows.Scan(
			&move.id, &move.sourceID, &move.targetID, &move.typeID,
			&move.endYear, &move.symmetric,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan absorbed person relationship: %w", err)
		}
		moves = append(moves, move)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate absorbed person relationships: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close absorbed person relationships: %w", err)
	}

	for _, move := range moves {
		sourceID, targetID := move.sourceID, move.targetID
		if sourceID == absorbedID {
			sourceID = survivorID
		}
		if targetID == absorbedID {
			targetID = survivorID
		}
		if move.symmetric && sourceID > targetID {
			sourceID, targetID = targetID, sourceID
		}
		for _, personID := range []int64{move.sourceID, move.targetID, sourceID, targetID} {
			if personID != survivorID && personID != absorbedID {
				*projectionIDs = append(*projectionIDs, personID)
			}
		}

		if sourceID == targetID {
			if _, err := tx.ExecContext(ctx, `UPDATE person_relationship_reviews
				SET accepted_relationship_id = NULL, status = 'rejected',
					reviewed_by = COALESCE(reviewed_by, 'system'),
					reviewed_at = COALESCE(reviewed_at, `+s.dialect.Now()+`),
					updated_at = `+s.dialect.Now()+`
				WHERE accepted_relationship_id = ? AND status = 'accepted'`, move.id); err != nil {
				return fmt.Errorf("reject review for relationship collapsed to self-edge: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM person_relationships WHERE id = ?`, move.id,
			); err != nil {
				return fmt.Errorf("delete relationship collapsed to self-edge: %w", err)
			}
			if err := s.setPersonMergeRowDispositionTx(
				ctx, tx, mergeID, personRelationshipsTableName, move.id,
				"deleted_snapshot", nil,
			); err != nil {
				return err
			}
			continue
		}

		var duplicateID int64
		if !move.endYear.Valid {
			err = tx.QueryRowContext(ctx, `SELECT id FROM person_relationships
				WHERE id <> ? AND source_person_id = ? AND target_person_id = ?
				  AND relationship_type_id = ? AND end_year IS NULL
				ORDER BY id LIMIT 1`, move.id, sourceID, targetID, move.typeID).Scan(&duplicateID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("find duplicate person relationship: %w", err)
			}
		}
		if duplicateID > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE person_relationship_reviews
				SET accepted_relationship_id = ?, updated_at = `+s.dialect.Now()+`
				WHERE accepted_relationship_id = ?`, duplicateID, move.id); err != nil {
				return fmt.Errorf("repoint deduplicated relationship reviews: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM person_relationships WHERE id = ?`, move.id,
			); err != nil {
				return fmt.Errorf("delete duplicate person relationship: %w", err)
			}
			if err := s.setPersonMergeRowDispositionTx(
				ctx, tx, mergeID, personRelationshipsTableName, move.id,
				personMergeActionDeduplicated, &duplicateID,
			); err != nil {
				return err
			}
			continue
		}

		if _, err := tx.ExecContext(ctx, `UPDATE person_relationships
			SET source_person_id = ?, target_person_id = ?, revision = revision + 1,
			    updated_at = `+s.dialect.Now()+`
			WHERE id = ?`, sourceID, targetID, move.id); err != nil {
			return fmt.Errorf("repoint person relationship: %w", err)
		}
	}
	return nil
}

func (s *Store) reconcilePersonRelationshipReviewsTx(
	ctx context.Context, tx *loggedTx, mergeID, survivorID, absorbedID int64,
	projectionIDs *[]int64,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, person_id, matched_person_id
		FROM person_relationship_reviews
		WHERE person_id = ? OR matched_person_id = ?
		ORDER BY CASE WHEN person_id = ? THEN 1 ELSE 0 END, id`,
		absorbedID, absorbedID, absorbedID)
	if err != nil {
		return fmt.Errorf("load absorbed relationship reviews: %w", err)
	}
	type reviewMove struct {
		id, personID int64
		matchedID    sql.NullInt64
	}
	moves := []reviewMove{}
	for rows.Next() {
		var move reviewMove
		if err := rows.Scan(&move.id, &move.personID, &move.matchedID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan absorbed relationship review: %w", err)
		}
		moves = append(moves, move)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate absorbed relationship reviews: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close absorbed relationship reviews: %w", err)
	}

	for _, move := range moves {
		if move.personID != survivorID && move.personID != absorbedID {
			*projectionIDs = append(*projectionIDs, move.personID)
		}
		personID := move.personID
		if personID == absorbedID {
			personID = survivorID
		}
		matchedID := move.matchedID
		if matchedID.Valid && matchedID.Int64 == absorbedID {
			matchedID.Int64 = survivorID
		}
		if matchedID.Valid && matchedID.Int64 == personID {
			matchedID.Valid = false
		}

		var duplicateID int64
		err := tx.QueryRowContext(ctx, `SELECT existing.id
			FROM person_relationship_reviews candidate
			JOIN person_relationship_reviews existing
			  ON existing.id <> candidate.id
			 AND existing.person_id = ?
			 AND existing.raw_related_type = candidate.raw_related_type
			 AND existing.raw_related_value = candidate.raw_related_value
			 AND existing.source = candidate.source
			 AND COALESCE(existing.source_ref, '') = COALESCE(candidate.source_ref, '')
			 AND COALESCE(existing.source_resource_uid, '') =
			     COALESCE(candidate.source_resource_uid, '')
			 AND COALESCE(existing.vcard_property, '') = COALESCE(candidate.vcard_property, '')
			 AND COALESCE(existing.vcard_group, '') = COALESCE(candidate.vcard_group, '')
			 AND COALESCE(existing.vcard_prop_id, '') = COALESCE(candidate.vcard_prop_id, '')
			 AND COALESCE(existing.vcard_pid, '') = COALESCE(candidate.vcard_pid, '')
			 AND COALESCE(existing.vcard_altid, '') = COALESCE(candidate.vcard_altid, '')
			WHERE candidate.id = ?
			ORDER BY existing.id LIMIT 1`, personID, move.id).Scan(&duplicateID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("find duplicate relationship review: %w", err)
		}
		if duplicateID > 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM person_relationship_reviews WHERE id = ?`, move.id,
			); err != nil {
				return fmt.Errorf("delete duplicate relationship review: %w", err)
			}
			if err := s.setPersonMergeRowDispositionTx(
				ctx, tx, mergeID, "person_relationship_reviews", move.id,
				personMergeActionDeduplicated, &duplicateID,
			); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_relationship_reviews
			SET person_id = ?, matched_person_id = ?, updated_at = `+s.dialect.Now()+`
			WHERE id = ?`, personID, matchedID, move.id); err != nil {
			return fmt.Errorf("repoint relationship review: %w", err)
		}
	}
	return nil
}

func (s *Store) reconcilePersonEmploymentsTx(
	ctx context.Context, tx *loggedTx, mergeID, survivorID, absorbedID int64,
) error {
	var survivorHasPrimary bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM employments WHERE person_id = ?
		  AND `+s.dialect.BoolTrueExpr("is_current")+`
		  AND `+s.dialect.BoolTrueExpr("is_primary")+`
	)`, survivorID).Scan(&survivorHasPrimary); err != nil {
		return fmt.Errorf("load survivor primary employment: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT
		id, organization_id, title_normalized, is_current, is_primary
		FROM employments WHERE person_id = ? ORDER BY id`, absorbedID)
	if err != nil {
		return fmt.Errorf("load absorbed employments: %w", err)
	}
	type employmentMove struct {
		id, organizationID int64
		titleNormalized    string
		current, primary   bool
	}
	moves := []employmentMove{}
	for rows.Next() {
		var move employmentMove
		if err := rows.Scan(
			&move.id, &move.organizationID, &move.titleNormalized,
			&move.current, &move.primary,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan absorbed employment: %w", err)
		}
		moves = append(moves, move)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate absorbed employments: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close absorbed employments: %w", err)
	}

	for _, move := range moves {
		var duplicateID int64
		if move.current {
			err := tx.QueryRowContext(ctx, `SELECT id FROM employments
				WHERE person_id = ? AND organization_id = ? AND title_normalized = ?
				  AND `+s.dialect.BoolTrueExpr("is_current")+`
				ORDER BY id LIMIT 1`, survivorID, move.organizationID, move.titleNormalized).Scan(&duplicateID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("find duplicate employment: %w", err)
			}
		}
		if duplicateID > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM employments WHERE id = ?`, move.id); err != nil {
				return fmt.Errorf("delete duplicate employment: %w", err)
			}
			if err := s.setPersonMergeRowDispositionTx(
				ctx, tx, mergeID, "employments", move.id, personMergeActionDeduplicated, &duplicateID,
			); err != nil {
				return err
			}
			continue
		}
		primary := move.primary
		if move.current && primary && survivorHasPrimary {
			primary = false
		}
		if _, err := tx.ExecContext(ctx, `UPDATE employments
			SET person_id = ?, is_primary = ?, revision = revision + 1,
			    updated_at = `+s.dialect.Now()+`
			WHERE id = ?`, survivorID, primary, move.id); err != nil {
			return fmt.Errorf("move absorbed employment: %w", err)
		}
		if move.current && primary {
			survivorHasPrimary = true
		}
	}
	return nil
}

func (s *Store) reconcilePersonIdentityCandidatesTx(
	ctx context.Context, tx *loggedTx, mergeID, survivorID, absorbedID int64,
) error {
	candidateIDs, err := personMergeRowIDsTx(ctx, tx, `SELECT id
		FROM identity_match_candidates
		WHERE (left_kind = 'person' AND left_id = ?)
		   OR (right_kind = 'person' AND right_id = ?)
		ORDER BY id`, absorbedID, absorbedID)
	if err != nil {
		return fmt.Errorf("load absorbed identity candidates: %w", err)
	}
	type candidateSource struct {
		candidateID, sourceID int64
		originalKey           string
	}
	sources := []candidateSource{}
	for _, candidateID := range candidateIDs {
		rows, err := tx.QueryContext(ctx, `SELECT source_id
			FROM identity_match_candidate_sources
			WHERE candidate_id = ? ORDER BY source_id`, candidateID)
		if err != nil {
			return fmt.Errorf("load absorbed candidate sources: %w", err)
		}
		for rows.Next() {
			var sourceID int64
			if err := rows.Scan(&sourceID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan absorbed candidate source: %w", err)
			}
			key, err := personMergeIntegerRowKey(
				[]string{"candidate_id", sourceIDColumnName}, candidateID, sourceID,
			)
			if err != nil {
				_ = rows.Close()
				return err
			}
			sources = append(sources, candidateSource{
				candidateID: candidateID, sourceID: sourceID, originalKey: key,
			})
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate absorbed candidate sources: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close absorbed candidate sources: %w", err)
		}
	}
	type candidateEvidence struct {
		candidateID, evidenceID int64
		sourceIDs               []int64
	}
	evidenceRows := []candidateEvidence{}
	for _, candidateID := range candidateIDs {
		rows, err := tx.QueryContext(ctx, `SELECT id
			FROM identity_match_evidence WHERE candidate_id = ? ORDER BY id`, candidateID)
		if err != nil {
			return fmt.Errorf("load absorbed candidate evidence: %w", err)
		}
		for rows.Next() {
			var evidenceID int64
			if err := rows.Scan(&evidenceID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan absorbed candidate evidence: %w", err)
			}
			evidenceRows = append(evidenceRows, candidateEvidence{
				candidateID: candidateID, evidenceID: evidenceID,
			})
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate absorbed candidate evidence: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close absorbed candidate evidence: %w", err)
		}
	}
	for index := range evidenceRows {
		evidenceRows[index].sourceIDs, err = personMergeRowIDsTx(ctx, tx, `SELECT source_id
			FROM identity_match_evidence_sources
			WHERE evidence_id = ? ORDER BY source_id`, evidenceRows[index].evidenceID)
		if err != nil {
			return fmt.Errorf("load absorbed evidence sources: %w", err)
		}
	}
	if err := s.rewriteIdentityMatchCandidatesForEndpointMergeTx(
		ctx, tx, IdentityMatchPerson, absorbedID, survivorID, nil, true,
	); err != nil {
		return fmt.Errorf("rewrite person identity candidates: %w", err)
	}
	targets := make(map[int64]*int64, len(candidateIDs))
	for _, candidateID := range candidateIDs {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM identity_match_candidates WHERE id = ?
		)`, candidateID).Scan(&exists); err != nil {
			return fmt.Errorf("inspect rewritten identity candidate: %w", err)
		}
		if exists {
			target := candidateID
			targets[candidateID] = &target
			continue
		}
		survivingID, collapsed, found, err := identityMatchCandidateRedirectTx(
			ctx, tx, candidateID,
		)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: rewritten identity candidate has no redirect",
				ErrPersonMergeInvalid)
		}
		if collapsed {
			targets[candidateID] = nil
			if err := s.setPersonMergeRowDispositionTx(
				ctx, tx, mergeID, identityMatchCandidatesTableName, candidateID,
				"deleted_snapshot", nil,
			); err != nil {
				return err
			}
			continue
		}
		target := survivingID
		targets[candidateID] = &target
		if err := s.setPersonMergeRowDispositionTx(
			ctx, tx, mergeID, identityMatchCandidatesTableName, candidateID,
			personMergeActionDeduplicated, &survivingID,
		); err != nil {
			return err
		}
	}
	for _, source := range sources {
		target := targets[source.candidateID]
		if target != nil && *target == source.candidateID {
			continue
		}
		var currentKey *string
		action := "deleted_snapshot"
		if target != nil {
			key, err := personMergeIntegerRowKey(
				[]string{"candidate_id", sourceIDColumnName}, *target, source.sourceID,
			)
			if err != nil {
				return err
			}
			currentKey = &key
			var targetExisted bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
				SELECT 1 FROM person_merge_rows
				WHERE merge_id = ? AND table_name = 'identity_match_candidate_sources'
				  AND original_row_key = ?
			)`, mergeID, key).Scan(&targetExisted); err != nil {
				return fmt.Errorf("inspect pre-merge candidate source target: %w", err)
			}
			action = "repointed"
			if targetExisted {
				action = personMergeActionDeduplicated
			}
		}
		if err := s.setPersonMergeRowKeyDispositionTx(
			ctx, tx, mergeID, identityMatchCandidateSourcesTableName,
			source.originalKey, action, currentKey,
		); err != nil {
			return err
		}
	}
	for _, evidence := range evidenceRows {
		if targets[evidence.candidateID] != nil {
			continue
		}
		if err := s.setPersonMergeRowDispositionTx(
			ctx, tx, mergeID, identityMatchEvidenceTableName, evidence.evidenceID,
			"deleted_snapshot", nil,
		); err != nil {
			return err
		}
		for _, sourceID := range evidence.sourceIDs {
			key, err := personMergeIntegerRowKey(
				[]string{"evidence_id", sourceIDColumnName}, evidence.evidenceID, sourceID,
			)
			if err != nil {
				return err
			}
			if err := s.setPersonMergeRowKeyDispositionTx(
				ctx, tx, mergeID, identityMatchEvidenceSourcesTableName, key,
				"deleted_snapshot", nil,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) reconcilePersonDailyNotesTx(
	ctx context.Context, tx *loggedTx, mergeID, survivorID, absorbedID int64,
) error {
	entryIDs, err := personMergeRowIDsTx(ctx, tx, `SELECT entry_id
		FROM daily_note_entry_persons WHERE person_id = ? ORDER BY entry_id`, absorbedID)
	if err != nil {
		return fmt.Errorf("load absorbed daily note targets: %w", err)
	}
	for _, entryID := range entryIDs {
		originalKey, err := personMergeIntegerRowKey(
			[]string{"entry_id", "person_id"}, entryID, absorbedID,
		)
		if err != nil {
			return err
		}
		currentKey, err := personMergeIntegerRowKey(
			[]string{"entry_id", "person_id"}, entryID, survivorID,
		)
		if err != nil {
			return err
		}
		var duplicate bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM daily_note_entry_persons
			WHERE entry_id = ? AND person_id = ?
		)`, entryID, survivorID).Scan(&duplicate); err != nil {
			return fmt.Errorf("find duplicate daily note target: %w", err)
		}
		action := "repointed"
		if duplicate {
			action = personMergeActionDeduplicated
			if _, err := tx.ExecContext(ctx, `DELETE FROM daily_note_entry_persons
				WHERE entry_id = ? AND person_id = ?`, entryID, absorbedID); err != nil {
				return fmt.Errorf("delete duplicate daily note target: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE daily_note_entry_persons
			SET person_id = ? WHERE entry_id = ? AND person_id = ?`,
			survivorID, entryID, absorbedID); err != nil {
			return fmt.Errorf("repoint daily note target: %w", err)
		}
		if err := s.setPersonMergeRowKeyDispositionTx(
			ctx, tx, mergeID, "daily_note_entry_persons", originalKey,
			action, &currentKey,
		); err != nil {
			return err
		}
	}
	return nil
}

func rewritePersonMergeParticipantLineageTx(
	ctx context.Context, tx *loggedTx, absorbedParticipantID, survivorParticipantID int64,
) error {
	var conflictingLineage bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1
		FROM person_merge_participants absorbed
		JOIN person_merge_participants survivor
		  ON survivor.merge_id = absorbed.merge_id
		WHERE absorbed.participant_id = ?
		  AND survivor.participant_id = ?
		  AND (absorbed.origin_side <> survivor.origin_side
		    OR COALESCE(absorbed.split_id, 0) <> COALESCE(survivor.split_id, 0))
	)`, absorbedParticipantID, survivorParticipantID).Scan(&conflictingLineage); err != nil {
		return fmt.Errorf("inspect participant consolidation merge lineage: %w", err)
	}
	if conflictingLineage {
		return ErrPersonMergeLineageConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_merge_participants
		SET origin_side = CASE
			WHEN origin_side = 'absorbed' OR 'absorbed' = (
				SELECT absorbed.origin_side
				FROM person_merge_participants absorbed
				WHERE absorbed.merge_id = person_merge_participants.merge_id
				  AND absorbed.participant_id = ?
			) THEN 'absorbed'
			ELSE 'survivor'
		END,
		split_id = COALESCE(split_id, (
			SELECT absorbed.split_id
			FROM person_merge_participants absorbed
			WHERE absorbed.merge_id = person_merge_participants.merge_id
			  AND absorbed.participant_id = ?
		))
		WHERE participant_id = ?
		  AND EXISTS (
			SELECT 1 FROM person_merge_participants absorbed
			WHERE absorbed.merge_id = person_merge_participants.merge_id
			  AND absorbed.participant_id = ?
		  )`, absorbedParticipantID, absorbedParticipantID,
		survivorParticipantID, absorbedParticipantID); err != nil {
		return fmt.Errorf("combine person merge participant lineage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_merge_participants
		WHERE participant_id = ?
		  AND EXISTS (
			SELECT 1 FROM person_merge_participants survivor
			WHERE survivor.merge_id = person_merge_participants.merge_id
			  AND survivor.participant_id = ?
		  )`, absorbedParticipantID, survivorParticipantID); err != nil {
		return fmt.Errorf("deduplicate person merge participant lineage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_merge_participants
		SET participant_id = ? WHERE participant_id = ?`,
		survivorParticipantID, absorbedParticipantID); err != nil {
		return fmt.Errorf("repoint person merge participant lineage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_merge_rows
		SET participant_id = ? WHERE participant_id = ?`,
		survivorParticipantID, absorbedParticipantID); err != nil {
		return fmt.Errorf("repoint person merge row lineage: %w", err)
	}
	return nil
}

func (s *Store) getPersonMergeTx(
	ctx context.Context, tx *loggedTx, mergeID int64,
) (*PersonMerge, error) {
	var (
		merge     PersonMerge
		currentID sql.NullInt64
		createdAt time.Time
	)
	err := tx.QueryRowContext(ctx, `SELECT
		id, survivor_person_id_at_merge, absorbed_person_id, current_person_id,
		survivor_uid, absorbed_uid, survivor_revision_before,
		absorbed_revision_before, survivor_revision_after, actor,
		snapshot_version, snapshot_sha256, created_at
		FROM person_merges WHERE id = ?`, mergeID).Scan(
		&merge.ID, &merge.SurvivorPersonID, &merge.AbsorbedPersonID, &currentID,
		&merge.SurvivorVCardUID, &merge.AbsorbedVCardUID,
		&merge.SurvivorRevisionBefore, &merge.AbsorbedRevisionBefore,
		&merge.SurvivorRevisionAfter, &merge.Actor, &merge.SnapshotVersion,
		&merge.SnapshotSHA256, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPersonMergeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get person merge %d: %w", mergeID, err)
	}
	if currentID.Valid {
		merge.CurrentPersonID = &currentID.Int64
	}
	merge.CreatedAt = createdAt
	return &merge, nil
}

func (s *Store) personMergeResultTx(
	ctx context.Context, tx *loggedTx, merge *PersonMerge,
) (*PersonMergeResult, error) {
	if merge.CurrentPersonID == nil {
		return nil, ErrPersonNotFound
	}
	person, err := s.getPersonTx(ctx, tx, *merge.CurrentPersonID)
	if err != nil {
		return nil, err
	}
	candidates, err := listPersonMergeReviewCandidatesTx(ctx, tx, merge.ID)
	if err != nil {
		return nil, err
	}
	return &PersonMergeResult{
		Person: *person, Merge: *merge, ReviewCandidates: candidates,
	}, nil
}

func listPersonMergeReviewCandidatesTx(
	ctx context.Context, tx *loggedTx, mergeID int64,
) ([]PersonMergeReviewCandidate, error) {
	rows, err := tx.QueryContext(ctx, `SELECT
		id, merge_id, survivor_person_id, definition_id,
		survivor_value_id, absorbed_value_id, state, resolution_value_id,
		reviewed_by, reviewed_at, created_at
		FROM person_merge_review_candidates WHERE merge_id = ? ORDER BY id`, mergeID)
	if err != nil {
		return nil, fmt.Errorf("list person merge review candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []PersonMergeReviewCandidate{}
	for rows.Next() {
		var (
			candidate  PersonMergeReviewCandidate
			resolution sql.NullInt64
			reviewedBy sql.NullString
			reviewedAt sql.NullTime
		)
		if err := rows.Scan(
			&candidate.ID, &candidate.MergeID, &candidate.PersonID,
			&candidate.DefinitionID, &candidate.SurvivorValueID,
			&candidate.AbsorbedValueID, &candidate.State, &resolution,
			&reviewedBy, &reviewedAt, &candidate.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan person merge review candidate: %w", err)
		}
		if resolution.Valid {
			candidate.ResolutionValueID = &resolution.Int64
		}
		if reviewedBy.Valid {
			candidate.ReviewedBy = &reviewedBy.String
		}
		if reviewedAt.Valid {
			candidate.ReviewedAt = &reviewedAt.Time
		}
		result = append(result, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person merge review candidates: %w", err)
	}
	return result, nil
}
