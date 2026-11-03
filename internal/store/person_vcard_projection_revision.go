package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// The projection fingerprint alone cannot decide whether a rendered envelope
// is still current. It is computed from a snapshot, rechecked inside the
// commit transaction, and on PostgreSQL that transaction's REPEATABLE READ
// snapshot is established before the recheck runs — so a semantic write that
// commits between the two is invisible to the recheck and the commit stores a
// projection of state that no longer exists. Rechecking harder cannot close
// that window; nothing the commit transaction reads can show it a commit its
// own snapshot excludes.
//
// persons.vcard_projection_revision closes it by giving both sides one row to
// meet on. Every writer that can change what loadPersonVCardSnapshotTx reads
// bumps the affected persons' revision inside its own transaction, which takes
// that row's write lock; the envelope commit locks the same row as its FIRST
// statement (lockPersonVCardProjectionTx). Ordering then decides the outcome
// and both outcomes are correct: a writer that reaches the row first makes the
// commit's lock fail with a serialization error (the row moved under its
// snapshot), which is reported as the same projection conflict a changed
// fingerprint would be; a writer that arrives second blocks until the commit
// finishes and lands after it. Writes that commit BEFORE the commit
// transaction starts need none of this — the fingerprint recheck already sees
// them.
//
// The bump must be an UPDATE, not a SELECT ... FOR UPDATE. Two lock requests
// serialize, but a lock alone leaves the row version untouched, so the commit
// would take the lock after the writer released it and still recheck against
// its own stale snapshot without any error to warn it.
//
// On SQLite the row lock is the database writer lock, and the commit has to
// take it before it reads anything (Dialect.RowWriterLockSQL). Component
// writers take the writer lock with their first statement, so a commit that
// read the snapshot first would keep losing SQLITE_BUSY_SNAPSHOT to them and
// starve; taking the lock first makes it wait its turn instead. The column
// still moves on SQLite, which is what makes the mechanism testable there.
//
// This comment is the authoritative description; schema.sql and schema_pg.sql
// point here.

// bumpPersonVCardProjectionsTx records that this transaction changed the
// semantic input of each person's native vCard projection. It deliberately
// leaves persons.revision and persons.updated_at alone: those are the person
// record's own compare-and-swap token and watermark, and a write to an
// employment or a relationship must not look to callers like the person
// record changed. Component writers that DO change the person record bump
// both through bumpPersonRevisionsTx.
func (s *Store) bumpPersonVCardProjectionsTx(
	ctx context.Context, tx *loggedTx, personIDs ...int64,
) error {
	placeholders, args := sortedIDPlaceholders(personIDs)
	if placeholders == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE persons
		SET vcard_projection_revision = vcard_projection_revision + 1
		WHERE id IN (`+placeholders+`)
	`, args...); err != nil {
		return fmt.Errorf("bump person vCard projection revisions: %w", err)
	}
	return nil
}

// bumpEmployedPersonVCardProjectionsTx follows employment into the person
// projections an organization change reaches. A person's snapshot carries the
// full profile of every organization they are employed by, so an organization
// or organization-component write changes their projection without touching
// any person-scoped row.
//
// Callers that remove employments or the organization itself must run this
// BEFORE the removal, while the rows that name the affected persons still
// exist.
func (s *Store) bumpEmployedPersonVCardProjectionsTx(
	ctx context.Context, tx *loggedTx, organizationIDs ...int64,
) error {
	placeholders, args := sortedIDPlaceholders(organizationIDs)
	if placeholders == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE persons
		SET vcard_projection_revision = vcard_projection_revision + 1
		WHERE EXISTS (
			SELECT 1 FROM employments e
			WHERE e.person_id = persons.id AND e.organization_id IN (`+placeholders+`)
		)
	`, args...); err != nil {
		return fmt.Errorf(
			"bump employed person vCard projection revisions: %w", err,
		)
	}
	return nil
}

// bumpDisplayNameCounterpartVCardProjectionsTx records that a person's
// display name changed in every counterpart snapshot that joins and sorts on
// it. The renamed person's own projection is advanced by the person UPDATE.
func (s *Store) bumpDisplayNameCounterpartVCardProjectionsTx(
	ctx context.Context, tx *loggedTx, personID int64,
) error {
	if personID <= 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE persons
		SET vcard_projection_revision = vcard_projection_revision + 1
		WHERE id <> ? AND EXISTS (
			SELECT 1 FROM person_relationships r
			WHERE (r.source_person_id = ? AND r.target_person_id = persons.id)
			   OR (r.target_person_id = ? AND r.source_person_id = persons.id)
		)
	`, personID, personID, personID); err != nil {
		return fmt.Errorf(
			"bump display-name counterpart vCard projection revisions: %w", err,
		)
	}
	return nil
}

// bumpPersonDeletionCounterpartVCardProjectionsTx follows a person deletion
// out into the cards it rewrites from the outside. Deleting a person cascades
// away every relationship edge they stand at either end of, and clears the
// matched person of every review that resolved to them. Both rows are
// projected onto the OTHER person's card, and the deletion writes nothing else
// of that person's that would carry the bump.
//
// Reviews narrow to pending because the snapshot lists only pending ones. That
// is safe from inside a repeatable-read snapshot: a review's status only ever
// moves from pending to settled, and the write that settles it bumps its own
// person, so a stale read here can bump one row too many but never too few.
//
// Callers must run this BEFORE the delete, while the cascading rows still name
// the persons they reach.
func (s *Store) bumpPersonDeletionCounterpartVCardProjectionsTx(
	ctx context.Context, tx *loggedTx, personID int64,
) error {
	if personID <= 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE persons
		SET vcard_projection_revision = vcard_projection_revision + 1
		WHERE EXISTS (
			SELECT 1 FROM person_relationships r
			WHERE (r.source_person_id = ? AND r.target_person_id = persons.id)
			   OR (r.target_person_id = ? AND r.source_person_id = persons.id)
		) OR EXISTS (
			SELECT 1 FROM person_relationship_reviews v
			WHERE v.matched_person_id = ? AND v.person_id = persons.id
			  AND v.status = ?
		)
	`, personID, personID, personID, string(RelationshipReviewPending)); err != nil {
		return fmt.Errorf(
			"bump person %d deletion counterpart vCard projection revisions: %w",
			personID, err,
		)
	}
	return nil
}

// bumpRelationshipReviewVCardProjectionsTx bumps the person whose card the
// review belongs to. The snapshot lists that person's pending reviews, so
// staging one, claiming one, or attaching an accepted edge to one all change
// their projection. The review's matched person is not affected: reviews are
// selected by person_id alone.
func (s *Store) bumpRelationshipReviewVCardProjectionsTx(
	ctx context.Context, tx *loggedTx, reviewID int64,
) error {
	if reviewID <= 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE persons
		SET vcard_projection_revision = vcard_projection_revision + 1
		WHERE EXISTS (
			SELECT 1 FROM person_relationship_reviews v
			WHERE v.id = ? AND v.person_id = persons.id
		)
	`, reviewID); err != nil {
		return fmt.Errorf(
			"bump relationship review vCard projection revisions: %w", err,
		)
	}
	return nil
}

// bumpRelationshipTypeVCardProjectionsTx bumps every person whose snapshot
// resolves through the relationship type: the endpoints of every edge of that
// type, and of every edge whose type names it as inverse, because the snapshot
// loads each edge's type together with that type's inverse. Nobody else's card
// mentions the type, so nobody else is bumped.
//
// Delete callers must run this BEFORE the DELETE, while other types' inverse
// links still name it.
func (s *Store) bumpRelationshipTypeVCardProjectionsTx(
	ctx context.Context, tx *loggedTx, typeID int64,
) error {
	if typeID <= 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE persons
		SET vcard_projection_revision = vcard_projection_revision + 1
		WHERE EXISTS (
			SELECT 1 FROM person_relationships r
			WHERE persons.id IN (r.source_person_id, r.target_person_id)
			  AND (r.relationship_type_id = ? OR r.relationship_type_id IN (
			       SELECT t.id FROM relationship_types t WHERE t.inverse_type_id = ?
			  ))
		)
	`, typeID, typeID); err != nil {
		return fmt.Errorf(
			"bump relationship type %d vCard projection revisions: %w", typeID, err,
		)
	}
	return nil
}

// bumpAttributeDefinitionVCardProjectionsTx bumps every person when a
// definition write can reach their snapshot, and nobody otherwise. The
// snapshot lists every active vCard-mapped person definition whether or not
// the person has a value for it, so a write to one of those really does
// change every projection; a definition without a vCard property, or one for
// another object type, is never read by the snapshot and moves nothing.
func (s *Store) bumpAttributeDefinitionVCardProjectionsTx(
	ctx context.Context, tx *loggedTx, definition *AttributeDefinition,
) error {
	if definition.ObjectType != AttributeObjectPerson || definition.VCardProperty == nil {
		return nil
	}
	return s.bumpAllVCardProjectionsTx(ctx, tx)
}

// bumpAllVCardProjectionsTx bumps every person, for writes to the shared
// catalogs a projection resolves through rather than owns. Callers narrow
// first (bumpAttributeDefinitionVCardProjectionsTx,
// bumpRelationshipTypeVCardProjectionsTx); the seed reconcilers reach this
// directly because a drifted seed can change any column, and they gate the
// bump on a row count so a store open that repairs nothing bumps nothing.
//
// communication_services is the third catalog the snapshot joins and the one
// exception, by construction rather than by oversight: both contact-point
// reads project a single column from it, slug, and slug is fixed at insert —
// UpdateCommunicationServiceContext refuses a change with ErrServiceSlugConflict
// and nothing else rewrites it. A newly inserted service has no contact point
// pointing at it yet, and repointing an existing one is a write to
// person_contact_points, which bumps through bumpPersonRevisionsTx. So no
// communication-service write can move a projection. Widening those SELECTs
// to a second column would change that.
func (s *Store) bumpAllVCardProjectionsTx(
	ctx context.Context, tx *loggedTx,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE persons
		SET vcard_projection_revision = vcard_projection_revision + 1
	`); err != nil {
		return fmt.Errorf("bump all vCard projection revisions: %w", err)
	}
	return nil
}

// lockPersonVCardProjectionTx takes the person's projection row lock. It must
// be the first statement of the envelope commit transaction, so the lock is
// held for the whole of it and no projection writer can commit between the
// fingerprint recheck and the envelope UPDATE. On SQLite the dialect's writer
// lock statement comes first, so the transaction is a writer before it reads;
// on PostgreSQL the SELECT ... FOR UPDATE is the lock.
//
// A serialization failure here is not an error to surface raw: it means a
// projection writer committed after this transaction's snapshot, which is the
// stale-render case ErrVCardProjectionConflict already names. The fingerprint
// that state would have produced is unknowable from inside an aborted
// transaction, so the conflict carries the expected value only.
func (s *Store) lockPersonVCardProjectionTx(
	ctx context.Context, tx *loggedTx,
	personID int64, expectedFingerprint string,
) error {
	if lock := s.dialect.RowWriterLockSQL("persons", "vcard_projection_revision"); lock != "" {
		if _, err := tx.ExecContext(ctx, lock, personID); err != nil {
			return fmt.Errorf("lock person %d vCard projection: %w", personID, err)
		}
	}
	var locked int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM persons WHERE id = ?`+s.dialect.SelectForUpdate(),
		personID,
	).Scan(&locked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrPersonNotFound
	case s.dialect.IsSerializationFailureError(err):
		return &VCardProjectionConflictError{
			PersonID: personID, Expected: expectedFingerprint,
		}
	case err != nil:
		return fmt.Errorf("lock person %d vCard projection: %w", personID, err)
	}
	return nil
}

// sortedIDPlaceholders sorts and de-duplicates ids, drops non-positive ones, and
// returns the matching `?, ?, ...` list with its bind arguments. An empty
// placeholder string means there is nothing to bind and the caller's statement
// should not run.
func sortedIDPlaceholders(ids []int64) (string, []any) {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	ids = slices.DeleteFunc(ids, func(id int64) bool { return id <= 0 })
	if len(ids) == 0 {
		return "", nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return placeholders(len(ids)), args
}
