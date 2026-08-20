package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vcard"
)

var (
	ErrVCardResourceNotFound       = errors.New("vCard resource envelope not found")
	ErrVCardResourceInvalid        = errors.New("invalid vCard resource envelope")
	ErrVCardResourceIdentityExists = errors.New("vCard resource identity already exists")
	ErrVCardResourceWriteConflict  = errors.New("vCard resource envelope write conflict")
	ErrVCardProjectionConflict     = errors.New("vCard semantic projection changed")
	ErrPersonUIDAliasNotFound      = errors.New("person UID alias not found")
	ErrPersonUIDAliasInvalid       = errors.New("invalid person UID alias")
)

// VCardResourceWriteConflictError reports that another writer changed a
// resource after the caller read it.
type VCardResourceWriteConflictError struct {
	SourceRef         string
	SourceResourceUID string
	ExpectedRevision  int64
}

func (e *VCardResourceWriteConflictError) Error() string {
	return fmt.Sprintf(
		"%s: source %q resource %q no longer has revision %d",
		ErrVCardResourceWriteConflict, e.SourceRef, e.SourceResourceUID,
		e.ExpectedRevision,
	)
}

func (e *VCardResourceWriteConflictError) Unwrap() error {
	return ErrVCardResourceWriteConflict
}

// VCardResourceEnvelopeRecord is one durable native resource and its database
// concurrency metadata.
type VCardResourceEnvelopeRecord struct {
	vcard.ResourceEnvelope

	ID                    int64     `json:"id"`
	PersonID              int64     `json:"person_id"`
	ProjectionFingerprint string    `json:"projection_fingerprint,omitempty"`
	Revision              int64     `json:"revision"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type VCardResourceEnvelopeInput struct {
	PersonID              int64
	ExpectedRevision      *int64
	ProjectionFingerprint string
	Envelope              vcard.ResourceEnvelope
}

// PersonUIDAlias is a retired canonical UID. Source-resource lookup never
// consults this namespace.
type PersonUIDAlias struct {
	RetiredUID        string    `json:"retired_uid"`
	SurvivingPersonID *int64    `json:"surviving_person_id,omitempty"`
	Reason            string    `json:"reason"`
	CreatedAt         time.Time `json:"created_at"`
}

// PutVCardResourceEnvelopeContext inserts a resource or replaces its complete
// envelope with optimistic concurrency. Updates never replace the original raw
// bytes captured by the first insert.
func (s *Store) PutVCardResourceEnvelopeContext(
	ctx context.Context, input VCardResourceEnvelopeInput,
) (*VCardResourceEnvelopeRecord, error) {
	return s.putVCardResourceEnvelopeContext(ctx, input, "")
}

// putVCardResourceEnvelopeContext is the shared write body. An
// expectedProjectionFingerprint selects the projection-serialized mode: the
// repeatable-read transaction, the row lock, and the recheck. Only the semantic
// commit path supplies one, and it requires one; the empty value here means the
// caller is writing bytes it did not render from a snapshot, which nothing can
// be stale against.
func (s *Store) putVCardResourceEnvelopeContext(
	ctx context.Context, input VCardResourceEnvelopeInput,
	expectedProjectionFingerprint string,
) (*VCardResourceEnvelopeRecord, error) {
	if input.PersonID <= 0 {
		return nil, fmt.Errorf("%w: person ID must be positive", ErrVCardResourceInvalid)
	}
	if input.ExpectedRevision != nil && *input.ExpectedRevision <= 0 {
		return nil, fmt.Errorf("%w: expected revision must be positive", ErrVCardResourceInvalid)
	}
	envelope, err := prepareVCardEnvelope(input.Envelope)
	if err != nil {
		return nil, err
	}

	runTx := s.withTxContext
	if expectedProjectionFingerprint != "" {
		runTx = func(ctx context.Context, fn func(tx *loggedTx) error) error {
			return s.withTxOptionsContext(ctx, &sql.TxOptions{
				Isolation: sql.LevelRepeatableRead,
			}, fn)
		}
	}
	// Read-then-write: on SQLite an unrelated write that commits between
	// this transaction's reads and its first write fails it with SQLITE_BUSY
	// rather than waiting, and the retry starts over from the reads. The
	// projection-serialized mode is the exception: it takes the writer lock
	// first (lockPersonVCardProjectionTx) so a busy neighbour cannot starve
	// it, and the retry there covers PostgreSQL deadlocks and identity races.
	return retryContendedWrite(ctx, s, "write vCard resource envelope",
		func() (*VCardResourceEnvelopeRecord, error) {
			return s.writeVCardResourceEnvelopeOnce(
				ctx, runTx, input, envelope, expectedProjectionFingerprint,
			)
		})
}

func (s *Store) writeVCardResourceEnvelopeOnce(
	ctx context.Context,
	runTx func(context.Context, func(tx *loggedTx) error) error,
	input VCardResourceEnvelopeInput,
	envelope vcard.ResourceEnvelope,
	expectedProjectionFingerprint string,
) (*VCardResourceEnvelopeRecord, error) {
	var result *VCardResourceEnvelopeRecord
	err := runTx(ctx, func(tx *loggedTx) error {
		// The projection row lock has to come before every other statement:
		// on PostgreSQL it is what stops a semantic write from committing
		// inside this transaction's snapshot window, and holding it from the
		// first statement leaves no gap the recheck below cannot see. See
		// person_vcard_projection_revision.go.
		if expectedProjectionFingerprint != "" {
			if err := s.lockPersonVCardProjectionTx(
				ctx, tx, input.PersonID, expectedProjectionFingerprint,
			); err != nil {
				return err
			}
		}
		canonicalUID, err := vcardCanonicalUIDTx(ctx, tx, input.PersonID)
		if err != nil {
			return err
		}
		if envelope.CanonicalPersonUID != "" &&
			envelope.CanonicalPersonUID != canonicalUID {
			return fmt.Errorf(
				"%w: canonical UID does not match person %d",
				ErrVCardResourceInvalid, input.PersonID,
			)
		}
		envelope.CanonicalPersonUID = canonicalUID
		if expectedProjectionFingerprint != "" {
			if err := s.recheckPersonVCardProjectionTx(
				ctx, tx, input.PersonID, expectedProjectionFingerprint,
			); err != nil {
				return err
			}
		}

		current, err := s.findVCardResourceEnvelopeTx(
			ctx, tx, envelope.SourceRef, envelope.SourceResourceUID,
		)
		switch {
		case errors.Is(err, ErrVCardResourceNotFound):
			result, err = s.insertVCardResourceEnvelopeTx(ctx, tx, input, envelope)
			return err
		case err != nil:
			return err
		}
		result, err = s.updateVCardResourceEnvelopeTx(ctx, tx, input, envelope, current)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// insertVCardResourceEnvelopeTx captures a resource the store has not seen.
// The insert is the one write that records the original raw bytes.
func (s *Store) insertVCardResourceEnvelopeTx(
	ctx context.Context, tx *loggedTx,
	input VCardResourceEnvelopeInput, envelope vcard.ResourceEnvelope,
) (*VCardResourceEnvelopeRecord, error) {
	if input.ExpectedRevision != nil {
		return nil, newVCardResourceWriteConflict(
			envelope.SourceRef, envelope.SourceResourceUID,
			*input.ExpectedRevision,
		)
	}
	metadata, err := vcard.MarshalResourceMetadata(envelope)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVCardResourceInvalid, err)
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO vcard_resource_envelopes (
		person_id, canonical_person_uid, source_ref, source_resource_uid,
		href, original_raw_bytes, stored_body, resource_metadata,
		projection_fingerprint, content_hash, etag, revision,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`, ?, ?, ?, 1, `+
		s.dialect.Now()+`, `+s.dialect.Now()+`)
	RETURNING id`,
		input.PersonID, envelope.CanonicalPersonUID, envelope.SourceRef,
		envelope.SourceResourceUID, nullableVCardString(envelope.Href),
		envelope.OriginalRawBytes, envelope.StoredBody, string(metadata),
		nullableVCardString(input.ProjectionFingerprint),
		envelope.ContentHash, envelope.ETag,
	).Scan(&id)
	if err != nil {
		if s.dialect.IsConflictError(err) {
			return nil, ErrVCardResourceIdentityExists
		}
		return nil, fmt.Errorf("insert vCard resource envelope: %w", err)
	}
	return s.getVCardResourceEnvelopeTx(ctx, tx, id)
}

// updateVCardResourceEnvelopeTx replaces the stored envelope of a resource
// under revision compare-and-swap, keeping the raw bytes the insert captured.
// An update that would change nothing still claims the row at the expected
// revision, so success always means the caller's revision is the live one.
func (s *Store) updateVCardResourceEnvelopeTx(
	ctx context.Context, tx *loggedTx,
	input VCardResourceEnvelopeInput, envelope vcard.ResourceEnvelope,
	current *VCardResourceEnvelopeRecord,
) (*VCardResourceEnvelopeRecord, error) {
	if input.ExpectedRevision == nil {
		return nil, fmt.Errorf(
			"%w: expected revision is required for an update",
			ErrVCardResourceInvalid,
		)
	}
	if current.Revision != *input.ExpectedRevision {
		return nil, newVCardResourceWriteConflict(
			envelope.SourceRef, envelope.SourceResourceUID,
			*input.ExpectedRevision,
		)
	}
	if current.PersonID != input.PersonID {
		return nil, fmt.Errorf(
			"%w: an existing resource cannot change persons",
			ErrVCardResourceInvalid,
		)
	}
	envelope.OriginalRawBytes = append(
		[]byte(nil), current.OriginalRawBytes...,
	)
	metadata, err := vcard.MarshalResourceMetadata(envelope)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVCardResourceInvalid, err)
	}
	if sameVCardEnvelopeRecord(
		current, input.PersonID, envelope, metadata,
		input.ProjectionFingerprint,
	) {
		// Nothing to write, but success still has to mean the caller's
		// revision is the live one. Claim the row under that revision
		// without changing it, so a replacement committed after the read
		// above fails this call exactly as it would fail a real update.
		if err := s.claimVCardResourceRevisionTx(
			ctx, tx, current.ID, envelope, *input.ExpectedRevision,
		); err != nil {
			return nil, err
		}
		return current, nil
	}
	nextRevision := *input.ExpectedRevision + 1
	if nextRevision <= 1 {
		return nil, fmt.Errorf("%w: expected revision is too large", ErrVCardResourceInvalid)
	}
	updated, err := tx.ExecContext(ctx, `UPDATE vcard_resource_envelopes SET
		href = ?,
		stored_body = ?, resource_metadata = `+s.dialect.JSONBindExpr()+`,
		projection_fingerprint = ?, content_hash = ?, etag = ?, revision = ?,
		updated_at = `+s.dialect.Now()+`
		WHERE id = ? AND revision = ?`,
		nullableVCardString(envelope.Href),
		envelope.StoredBody, string(metadata),
		nullableVCardString(input.ProjectionFingerprint),
		envelope.ContentHash, envelope.ETag, nextRevision,
		current.ID, *input.ExpectedRevision,
	)
	if err := s.vcardResourceCASOutcome(
		updated, err, "update vCard resource envelope",
		envelope, *input.ExpectedRevision,
	); err != nil {
		return nil, err
	}
	return s.getVCardResourceEnvelopeTx(ctx, tx, current.ID)
}

// CommitVCardResourceEnvelopeContext persists a fully prepared envelope. It is
// the semantic render path and intentionally accepts no body-only update.
//
// The projection fingerprint is required, not optional. It is the only thing
// tying a render to the semantic state it was made from, and it is what arms
// both halves of the guarantee this path exists to give: the projection row
// lock and the recheck inside the commit transaction. A commit without one
// would take neither, so a profile write landing between render and commit
// would be projected over silently. Writes that carry no render need none of
// this and stay separate operations — PutVCardResourceEnvelopeContext for the
// initial capture and whole-envelope replacement, and
// RewriteVCardResourceSourceUIDContext for identity-only metadata — so no
// unguarded write reaches the semantic entry point.
func (s *Store) CommitVCardResourceEnvelopeContext(
	ctx context.Context,
	sourceRef, sourceUID string,
	expectedRevision int64,
	projectionFingerprint string,
	prepared vcard.ResourceEnvelope,
) (*VCardResourceEnvelopeRecord, error) {
	if strings.TrimSpace(projectionFingerprint) == "" {
		return nil, fmt.Errorf(
			"%w: semantic commits require a projection fingerprint",
			ErrVCardResourceInvalid,
		)
	}
	current, err := s.GetVCardResourceEnvelopeContext(ctx, sourceRef, sourceUID)
	if err != nil {
		return nil, err
	}
	if prepared.SourceRef != sourceRef ||
		prepared.SourceResourceUID != sourceUID {
		return nil, fmt.Errorf(
			"%w: prepared envelope source identity changed",
			ErrVCardResourceInvalid,
		)
	}
	if prepared.RenderMetadata.StoredVersion != vcard.Version40 ||
		prepared.RenderMetadata.RenderRequired {
		return nil, fmt.Errorf(
			"%w: semantic commits require a prepared canonical vCard 4 envelope",
			ErrVCardResourceInvalid,
		)
	}
	return s.putVCardResourceEnvelopeContext(ctx, VCardResourceEnvelopeInput{
		PersonID: current.PersonID, ExpectedRevision: &expectedRevision,
		ProjectionFingerprint: projectionFingerprint, Envelope: prepared,
	}, projectionFingerprint)
}

// GetVCardResourceEnvelopeContext loads one resource by its source identity.
func (s *Store) GetVCardResourceEnvelopeContext(
	ctx context.Context, sourceRef, sourceUID string,
) (*VCardResourceEnvelopeRecord, error) {
	record, err := s.scanVCardResourceEnvelope(s.db.QueryRowContext(
		ctx, vcardResourceEnvelopeSelect+
			` WHERE source_ref = ? AND source_resource_uid = ?`,
		sourceRef, sourceUID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrVCardResourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get vCard resource envelope: %w", err)
	}
	return record, nil
}

func (s *Store) GetVCardResourceEnvelopeByCanonicalUIDContext(
	ctx context.Context, canonicalUID string,
) ([]VCardResourceEnvelopeRecord, error) {
	rows, err := s.db.QueryContext(
		ctx, vcardResourceEnvelopeSelect+
			` WHERE canonical_person_uid = ? ORDER BY id`,
		canonicalUID,
	)
	if err != nil {
		return nil, fmt.Errorf("list vCard resources by canonical UID: %w", err)
	}
	defer func() { _ = rows.Close() }()

	records := make([]VCardResourceEnvelopeRecord, 0)
	for rows.Next() {
		record, scanErr := s.scanVCardResourceEnvelope(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan vCard resource by canonical UID: %w", scanErr)
		}
		records = append(records, *record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vCard resources by canonical UID: %w", err)
	}
	return records, nil
}

func (s *Store) RenderVCardResourceViewContext(
	ctx context.Context,
	sourceRef, sourceUID string,
	version vcard.Version,
) ([]byte, error) {
	record, err := s.GetVCardResourceEnvelopeContext(ctx, sourceRef, sourceUID)
	if err != nil {
		return nil, err
	}
	return record.RenderView(version)
}

func (s *Store) RetirePersonUIDAliasContext(
	ctx context.Context,
	retiredUID string,
	survivingPersonID *int64,
	reason string,
) (*PersonUIDAlias, error) {
	retiredUID = strings.TrimSpace(retiredUID)
	reason = strings.TrimSpace(reason)
	if retiredUID == "" || reason == "" {
		return nil, fmt.Errorf(
			"%w: retired UID and reason are required", ErrPersonUIDAliasInvalid,
		)
	}
	return retryContendedWrite(ctx, s, "retire person UID alias",
		func() (*PersonUIDAlias, error) {
			return s.retirePersonUIDAliasOnce(ctx, retiredUID, survivingPersonID, reason)
		})
}

func (s *Store) retirePersonUIDAliasOnce(
	ctx context.Context,
	retiredUID string,
	survivingPersonID *int64,
	reason string,
) (*PersonUIDAlias, error) {
	var alias *PersonUIDAlias
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM persons WHERE vcard_uid = ?)`,
			retiredUID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check retired person UID: %w", err)
		}
		if exists {
			return fmt.Errorf("%w: UID is still canonical", ErrPersonUIDAliasInvalid)
		}
		if survivingPersonID != nil {
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS (SELECT 1 FROM persons WHERE id = ?)`,
				*survivingPersonID,
			).Scan(&exists); err != nil {
				return fmt.Errorf("check surviving person: %w", err)
			}
			if !exists {
				return ErrPersonNotFound
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO person_uid_aliases (
			retired_uid, surviving_person_id, reason, created_at
		) VALUES (?, ?, ?, `+s.dialect.Now()+`)`,
			retiredUID, nullableVCardInt64(survivingPersonID), reason,
		)
		if err != nil {
			if s.dialect.IsConflictError(err) {
				return fmt.Errorf("%w: %s", ErrPersonUIDAliasInvalid, retiredUID)
			}
			return fmt.Errorf("insert person UID alias: %w", err)
		}
		alias, err = s.getPersonUIDAliasTx(ctx, tx, retiredUID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return alias, nil
}

func (s *Store) ResolveRetiredPersonUIDContext(
	ctx context.Context, retiredUID string,
) (*PersonUIDAlias, error) {
	alias, err := s.getPersonUIDAliasTx(ctx, nil, retiredUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPersonUIDAliasNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve retired person UID: %w", err)
	}
	return alias, nil
}

func prepareVCardEnvelope(
	input vcard.ResourceEnvelope,
) (vcard.ResourceEnvelope, error) {
	envelope := input
	if strings.TrimSpace(envelope.SourceRef) == "" {
		return vcard.ResourceEnvelope{}, fmt.Errorf(
			"%w: source reference is required", ErrVCardResourceInvalid,
		)
	}
	if strings.TrimSpace(envelope.SourceResourceUID) == "" {
		return vcard.ResourceEnvelope{}, fmt.Errorf(
			"%w: source resource UID is required", ErrVCardResourceInvalid,
		)
	}
	if len(envelope.OriginalRawBytes) == 0 || len(envelope.StoredBody) == 0 {
		return vcard.ResourceEnvelope{}, fmt.Errorf(
			"%w: original and stored bodies are required",
			ErrVCardResourceInvalid,
		)
	}
	if envelope.RenderMetadata.RenderRequired {
		return vcard.ResourceEnvelope{}, fmt.Errorf(
			"%w: envelope must be prepared before persistence",
			ErrVCardResourceInvalid,
		)
	}
	parsed, err := vcard.ParseResourceEnvelope(envelope.StoredBody)
	if err != nil {
		return vcard.ResourceEnvelope{}, fmt.Errorf(
			"%w: %w", ErrVCardResourceInvalid, err,
		)
	}
	if envelope.RenderMetadata.StoredVersion != parsed.RenderMetadata.StoredVersion {
		return vcard.ResourceEnvelope{}, fmt.Errorf(
			"%w: metadata version %q does not match body version %q",
			ErrVCardResourceInvalid, envelope.RenderMetadata.StoredVersion,
			parsed.RenderMetadata.StoredVersion,
		)
	}
	if len(envelope.PropertyTree) != len(parsed.PropertyTree) {
		return vcard.ResourceEnvelope{}, fmt.Errorf(
			"%w: property tree length does not match stored body",
			ErrVCardResourceInvalid,
		)
	}
	for index := range envelope.PropertyTree {
		if !reflect.DeepEqual(
			envelope.PropertyTree[index].Property,
			parsed.PropertyTree[index].Property,
		) {
			return vcard.ResourceEnvelope{}, fmt.Errorf(
				"%w: property tree occurrence %d does not match stored body",
				ErrVCardResourceInvalid, index,
			)
		}
	}
	wantHash := vcard.ContentHash(envelope.StoredBody)
	wantETag := vcard.ETagForBody(envelope.StoredBody)
	if envelope.ContentHash != "" && envelope.ContentHash != wantHash {
		return vcard.ResourceEnvelope{}, fmt.Errorf(
			"%w: content hash does not match stored body",
			ErrVCardResourceInvalid,
		)
	}
	if envelope.ETag != "" && envelope.ETag != wantETag {
		return vcard.ResourceEnvelope{}, fmt.Errorf(
			"%w: ETag does not match stored body", ErrVCardResourceInvalid,
		)
	}
	envelope.ContentHash = wantHash
	envelope.ETag = wantETag
	if _, err := vcard.MarshalResourceMetadata(envelope); err != nil {
		return vcard.ResourceEnvelope{}, fmt.Errorf(
			"%w: %w", ErrVCardResourceInvalid, err,
		)
	}
	return envelope, nil
}

// claimVCardResourceRevisionTx asserts, with a write, that a resource row is
// still at expectedRevision. The statement changes nothing, but as a
// revision-qualified UPDATE it fails the same way a real one does when another
// writer got there first: SQLite refuses the writer lock to a transaction whose
// read snapshot is stale (SQLITE_BUSY, retried from the reads), PostgreSQL
// re-evaluates the WHERE against the replaced row and matches nothing, or under
// REPEATABLE READ reports a serialization failure. Reads alone can commit
// successfully against a row that has already moved on; this cannot.
func (s *Store) claimVCardResourceRevisionTx(
	ctx context.Context, tx *loggedTx, id int64,
	envelope vcard.ResourceEnvelope, expectedRevision int64,
) error {
	claimed, err := tx.ExecContext(ctx, `UPDATE vcard_resource_envelopes
		SET revision = revision
		WHERE id = ? AND revision = ?`,
		id, expectedRevision,
	)
	return s.vcardResourceCASOutcome(
		claimed, err, "claim vCard resource envelope revision",
		envelope, expectedRevision,
	)
}

// vcardResourceCASOutcome translates the result of a revision-qualified UPDATE
// on vcard_resource_envelopes. Exactly one affected row is success. Zero rows,
// or PostgreSQL's serialization failure under REPEATABLE READ, both mean
// another writer replaced the row after the caller read it, and become the
// write conflict; a unique-index violation is an identity collision. Busy
// errors pass through untranslated so retryContendedWrite restarts the
// transaction from its reads.
func (s *Store) vcardResourceCASOutcome(
	res sql.Result, err error, operation string,
	envelope vcard.ResourceEnvelope, expectedRevision int64,
) error {
	if err != nil {
		switch {
		case s.dialect.IsConflictError(err):
			return ErrVCardResourceIdentityExists
		case s.dialect.IsSerializationFailureError(err):
			return newVCardResourceWriteConflict(
				envelope.SourceRef, envelope.SourceResourceUID, expectedRevision,
			)
		}
		return fmt.Errorf("%s: %w", operation, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: count affected rows: %w", operation, err)
	}
	if affected != 1 {
		return newVCardResourceWriteConflict(
			envelope.SourceRef, envelope.SourceResourceUID, expectedRevision,
		)
	}
	return nil
}

func sameVCardEnvelopeRecord(
	current *VCardResourceEnvelopeRecord,
	personID int64,
	envelope vcard.ResourceEnvelope,
	metadata []byte,
	projectionFingerprint string,
) bool {
	currentMetadata, err := vcard.MarshalResourceMetadata(
		current.ResourceEnvelope,
	)
	return err == nil &&
		current.PersonID == personID &&
		current.CanonicalPersonUID == envelope.CanonicalPersonUID &&
		current.Href == envelope.Href &&
		bytes.Equal(current.StoredBody, envelope.StoredBody) &&
		bytes.Equal(currentMetadata, metadata) &&
		current.ProjectionFingerprint == projectionFingerprint &&
		current.ContentHash == envelope.ContentHash &&
		current.ETag == envelope.ETag
}

func vcardCanonicalUIDTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (string, error) {
	var uid string
	err := tx.QueryRowContext(
		ctx, `SELECT vcard_uid FROM persons WHERE id = ?`, personID,
	).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPersonNotFound
	}
	if err != nil {
		return "", fmt.Errorf("load person %d canonical vCard UID: %w", personID, err)
	}
	return uid, nil
}

func newVCardResourceWriteConflict(
	sourceRef, sourceUID string, expectedRevision int64,
) error {
	return &VCardResourceWriteConflictError{
		SourceRef: sourceRef, SourceResourceUID: sourceUID,
		ExpectedRevision: expectedRevision,
	}
}

const vcardResourceEnvelopeSelect = `SELECT
	id, person_id, canonical_person_uid, source_ref, source_resource_uid, href,
	original_raw_bytes, stored_body, resource_metadata, projection_fingerprint,
	content_hash, etag, revision, created_at, updated_at
	FROM vcard_resource_envelopes`

func (s *Store) findVCardResourceEnvelopeTx(
	ctx context.Context, tx *loggedTx, sourceRef, sourceUID string,
) (*VCardResourceEnvelopeRecord, error) {
	record, err := s.scanVCardResourceEnvelope(tx.QueryRowContext(
		ctx, vcardResourceEnvelopeSelect+
			` WHERE source_ref = ? AND source_resource_uid = ?`,
		sourceRef, sourceUID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrVCardResourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find vCard resource envelope: %w", err)
	}
	return record, nil
}

func (s *Store) getVCardResourceEnvelopeTx(
	ctx context.Context, tx *loggedTx, id int64,
) (*VCardResourceEnvelopeRecord, error) {
	record, err := s.scanVCardResourceEnvelope(tx.QueryRowContext(
		ctx, vcardResourceEnvelopeSelect+` WHERE id = ?`, id,
	))
	if err != nil {
		return nil, fmt.Errorf("get vCard resource envelope %d: %w", id, err)
	}
	return record, nil
}

func (s *Store) scanVCardResourceEnvelope(
	row scanner,
) (*VCardResourceEnvelopeRecord, error) {
	var (
		record                             VCardResourceEnvelopeRecord
		href, fingerprint                  sql.NullString
		canonicalUID, sourceRef, sourceUID string
		original, stored, raw              []byte
		storedHash, storedETag             string
	)
	if err := row.Scan(
		&record.ID, &record.PersonID, &canonicalUID,
		&sourceRef, &sourceUID, &href,
		&original, &stored, &raw, &fingerprint,
		&storedHash, &storedETag, &record.Revision,
		&record.CreatedAt, &record.UpdatedAt,
	); err != nil {
		return nil, err
	}
	metadata, err := vcard.UnmarshalResourceMetadata(raw)
	if err != nil {
		return nil, fmt.Errorf("decode vCard resource metadata: %w", err)
	}
	record.ResourceEnvelope = metadata
	record.CanonicalPersonUID = canonicalUID
	record.SourceRef = sourceRef
	record.SourceResourceUID = sourceUID
	record.Href = href.String
	record.OriginalRawBytes = append([]byte(nil), original...)
	record.StoredBody = append([]byte(nil), stored...)
	record.ContentHash = storedHash
	if storedHash != vcard.ContentHash(stored) {
		return nil, fmt.Errorf("%w: stored content hash is corrupt", ErrVCardResourceInvalid)
	}
	record.ETag = storedETag
	if storedETag != vcard.ETagForBody(stored) {
		return nil, fmt.Errorf("%w: stored ETag is corrupt", ErrVCardResourceInvalid)
	}
	record.ProjectionFingerprint = fingerprint.String
	validated, err := prepareVCardEnvelope(record.ResourceEnvelope)
	if err != nil {
		return nil, err
	}
	record.ResourceEnvelope = validated
	return &record, nil
}

func (s *Store) getPersonUIDAliasTx(
	ctx context.Context, tx *loggedTx, retiredUID string,
) (*PersonUIDAlias, error) {
	var queryer contextStatementQuerier = s.db
	if tx != nil {
		queryer = tx
	}
	var alias PersonUIDAlias
	var surviving sql.NullInt64
	err := queryer.QueryRowContext(ctx, `SELECT
		retired_uid, surviving_person_id, reason, created_at
		FROM person_uid_aliases WHERE retired_uid = ?`,
		retiredUID,
	).Scan(
		&alias.RetiredUID, &surviving, &alias.Reason, &alias.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if surviving.Valid {
		alias.SurvivingPersonID = &surviving.Int64
	}
	return &alias, nil
}

func nullableVCardString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableVCardInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
