package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrRelationshipReviewNotFound   = errors.New("relationship review not found")
	ErrRelationshipReviewNotPending = errors.New("relationship review is not pending")
	ErrRelationshipReviewInvalid    = errors.New("invalid relationship review input")
)

const (
	maxRelatedValueBytes = 1024
	maxRelatedTypeBytes  = 128
	urnUUIDPrefix        = "urn:uuid:"
)

// RelatedValueKind records whether RELATED carried a URI or free text.
type RelatedValueKind string

const (
	RelatedValueKindURI  RelatedValueKind = "uri"
	RelatedValueKindText RelatedValueKind = "text"
)

func (k RelatedValueKind) valid() bool {
	return k == RelatedValueKindURI || k == RelatedValueKindText
}

// RelationshipReviewStatus is the lifecycle of an unresolved RELATED value.
type RelationshipReviewStatus string

const (
	RelationshipReviewPending  RelationshipReviewStatus = "pending"
	RelationshipReviewAccepted RelationshipReviewStatus = "accepted"
	RelationshipReviewRejected RelationshipReviewStatus = "rejected"
)

// RelationshipReview preserves an imported assertion until a human resolves it.
type RelationshipReview struct {
	ID                     int64                    `json:"id"`
	PersonID               int64                    `json:"person_id"`
	RawRelatedValue        string                   `json:"raw_related_value"`
	RawRelatedType         string                   `json:"raw_related_type"`
	ValueKind              RelatedValueKind         `json:"value_kind"`
	MatchedPersonID        *int64                   `json:"matched_person_id,omitempty"`
	Status                 RelationshipReviewStatus `json:"status"`
	AcceptedRelationshipID *int64                   `json:"accepted_relationship_id,omitempty"`
	Source                 Provenance               `json:"source"`
	SourceRef              *string                  `json:"source_ref,omitempty"`
	VCardIdentity          VCardIdentity            `json:"vcard_identity"`
	CreatedBy              string                   `json:"created_by"`
	ReviewedBy             *string                  `json:"reviewed_by,omitempty"`
	ReviewedAt             *time.Time               `json:"reviewed_at,omitempty"`
	CreatedAt              time.Time                `json:"created_at"`
	UpdatedAt              time.Time                `json:"updated_at"`
}

// RelatedImport is one already-parsed RELATED property occurrence. RawType is
// exactly one TYPE parameter value; callers must split parameter lists first.
type RelatedImport struct {
	PersonID      int64
	RawValue      string
	RawType       string
	ValueKind     RelatedValueKind
	Source        Provenance
	SourceRef     *string
	VCardIdentity VCardIdentity
	Actor         string
}

// RelatedResolution contains either an automatic edge or a durable review.
type RelatedResolution struct {
	Relationship *PersonRelationship `json:"relationship,omitempty"`
	Review       *RelationshipReview `json:"review,omitempty"`
}

// Status returns the staged review status, or empty for an automatic edge.
func (r RelatedResolution) Status() RelationshipReviewStatus {
	if r.Review == nil {
		return ""
	}
	return r.Review.Status
}

// RelationshipReviewListOptions filters the review queue.
type RelationshipReviewListOptions struct {
	Status   RelationshipReviewStatus
	PersonID int64
}

// ResolvePersonByVCardUIDContext matches canonical vCard UID byte-for-byte.
func (s *Store) ResolvePersonByVCardUIDContext(ctx context.Context, uid string) (*Person, error) {
	if uid == "" {
		return nil, fmt.Errorf("%w: empty vCard UID", ErrPersonNotFound)
	}
	var person *Person
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM persons WHERE vcard_uid = ?`, uid).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: vcard_uid %q", ErrPersonNotFound, uid)
		}
		if err != nil {
			return fmt.Errorf("resolve person by vCard UID: %w", err)
		}
		person, err = s.getPersonTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return person, nil
}

// ResolveRelatedValueContext automatically links only an exact UID plus a
// recognized relationship type. All other inputs remain durable reviews.
func (s *Store) ResolveRelatedValueContext(ctx context.Context, in RelatedImport) (*RelatedResolution, error) {
	source, err := ParseProvenance(string(in.Source))
	if err != nil {
		return nil, err
	}
	in.Source = source
	actor, err := validateRelationshipActor(in.Actor)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRelationshipReviewInvalid, err)
	}
	if in.PersonID <= 0 {
		return nil, fmt.Errorf("%w: person ID must be positive", ErrRelationshipReviewInvalid)
	}
	if !in.ValueKind.valid() {
		return nil, fmt.Errorf("%w: value kind %q", ErrRelationshipReviewInvalid, in.ValueKind)
	}
	if strings.TrimSpace(in.RawValue) == "" || len(in.RawValue) > maxRelatedValueBytes {
		return nil, fmt.Errorf("%w: RELATED value is required and at most %d bytes", ErrRelationshipReviewInvalid, maxRelatedValueBytes)
	}
	if len(in.RawType) > maxRelatedTypeBytes || strings.Contains(in.RawType, ",") {
		return nil, fmt.Errorf("%w: RELATED TYPE must be one value of at most %d bytes", ErrRelationshipReviewInvalid, maxRelatedTypeBytes)
	}

	matchedPersonID, personMatched, err := s.matchRelatedValueToPerson(ctx, in)
	if err != nil {
		return nil, err
	}
	// The review schema forbids self matches and an import must never create a
	// self edge. Preserve the raw assertion, but leave its match unset.
	if personMatched && matchedPersonID == in.PersonID {
		personMatched = false
	}
	relationshipType, typeMatched, err := s.relationshipTypeForRelatedType(ctx, in.RawType)
	if err != nil {
		return nil, err
	}
	if personMatched && typeMatched {
		edge, err := s.AddPersonRelationshipContext(ctx, PersonRelationshipInput{
			SourcePersonID: in.PersonID, TargetPersonID: matchedPersonID,
			TypeSlug: relationshipType.Slug, Source: in.Source, SourceRef: in.SourceRef,
			VCardIdentity: in.VCardIdentity, Actor: actor,
		})
		if err == nil {
			return &RelatedResolution{Relationship: edge}, nil
		}
		if !errors.Is(err, ErrPersonRelationshipDuplicate) {
			return nil, err
		}
		existing, findErr := s.activePersonRelationshipContext(ctx, in.PersonID, matchedPersonID, relationshipType.Slug)
		if findErr != nil {
			return nil, findErr
		}
		return &RelatedResolution{Relationship: existing}, nil
	}

	review, err := s.stageRelationshipReview(ctx, in, matchedPersonID, personMatched, actor)
	if err != nil {
		return nil, err
	}
	return &RelatedResolution{Review: review}, nil
}

func (s *Store) matchRelatedValueToPerson(ctx context.Context, in RelatedImport) (int64, bool, error) {
	candidates := []string{in.RawValue}
	if lower := strings.ToLower(in.RawValue); strings.HasPrefix(lower, urnUUIDPrefix) {
		rawUUID := strings.TrimPrefix(lower, urnUUIDPrefix)
		// uuid.Parse accepts compact and brace-wrapped forms too. RELATED's
		// narrow exception is only a canonical hyphenated UUID after urn:uuid:.
		if parsed, err := uuid.Parse(rawUUID); len(rawUUID) == 36 && err == nil && parsed.String() == rawUUID {
			candidates = append(candidates, parsed.String())
		}
	}
	for _, candidate := range candidates {
		person, err := s.ResolvePersonByVCardUIDContext(ctx, candidate)
		if errors.Is(err, ErrPersonNotFound) {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		return person.ID, true, nil
	}
	return 0, false, nil
}

func (s *Store) relationshipTypeForRelatedType(ctx context.Context, rawType string) (RelationshipType, bool, error) {
	normalized := strings.ToLower(strings.TrimSpace(rawType))
	if normalized == "" {
		return RelationshipType{}, false, nil
	}
	relationshipType, err := scanRelationshipType(s.db.QueryRowContext(ctx,
		`SELECT `+relationshipTypeColumns+` FROM relationship_types WHERE vcard_related_type = ?`, normalized))
	if errors.Is(err, sql.ErrNoRows) {
		return RelationshipType{}, false, nil
	}
	if err != nil {
		return RelationshipType{}, false, fmt.Errorf("look up relationship type for RELATED TYPE %q: %w", rawType, err)
	}
	return *relationshipType, true, nil
}

// activePersonRelationshipContext looks up exactly the canonical triple that
// AddPersonRelationshipContext would write. It never searches both endpoint
// orientations, because reciprocal asymmetric edges may coexist.
func (s *Store) activePersonRelationshipContext(ctx context.Context, sourceID, targetID int64, typeSlug string) (*PersonRelationship, error) {
	var edge *PersonRelationship
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		relationshipType, canonicalSource, canonicalTarget, err := s.canonicalRelationshipEndpointsTx(ctx, tx, sourceID, targetID, typeSlug)
		if err != nil {
			return err
		}
		edge, err = scanPersonRelationship(tx.QueryRowContext(ctx,
			`SELECT `+personRelationshipColumns+personRelationshipFrom+`
			 WHERE r.source_person_id = ? AND r.target_person_id = ?
			   AND r.relationship_type_id = ? AND r.end_year IS NULL`,
			canonicalSource, canonicalTarget, relationshipType.ID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPersonRelationshipNotFound
		}
		if err != nil {
			return fmt.Errorf("find active relationship: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return edge, nil
}

func (s *Store) stageRelationshipReview(ctx context.Context, in RelatedImport, matchedPersonID int64, personMatched bool, actor string) (*RelationshipReview, error) {
	var review *RelationshipReview
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.requirePersonsExistTx(ctx, tx, in.PersonID); err != nil {
			return err
		}
		var insertedID int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO person_relationship_reviews (
				person_id, raw_related_value, raw_related_type, value_kind, matched_person_id,
				status, source, source_ref, vcard_property, vcard_group, vcard_prop_id,
				vcard_pid, vcard_altid, created_by
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING
			RETURNING id`,
			in.PersonID, in.RawValue, in.RawType, string(in.ValueKind), matchedPersonArg(matchedPersonID, personMatched),
			string(RelationshipReviewPending), string(in.Source), normalizeNullableText(in.SourceRef),
			nullableVCardText(in.VCardIdentity.Property), nullableVCardPointer(in.VCardIdentity.Group),
			nullableVCardPointer(in.VCardIdentity.PropID), nullableVCardText(strings.Join(in.VCardIdentity.PID, ",")),
			nullableVCardPointer(in.VCardIdentity.AltID), actor,
		).Scan(&insertedID)
		if err == nil {
			review, err = s.relationshipReviewTx(ctx, tx, insertedID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("stage relationship review: %w", err)
		}
		review, err = s.relationshipReviewByOccurrenceTx(ctx, tx, in)
		return err
	})
	if err != nil {
		return nil, err
	}
	return review, nil
}

func (s *Store) ListRelationshipReviewsContext(ctx context.Context, opts RelationshipReviewListOptions) ([]RelationshipReview, error) {
	query := `SELECT ` + relationshipReviewColumns + ` FROM person_relationship_reviews WHERE 1 = 1`
	args := make([]any, 0, 2)
	if opts.Status != "" {
		query += ` AND status = ?`
		args = append(args, string(opts.Status))
	}
	if opts.PersonID > 0 {
		query += ` AND person_id = ?`
		args = append(args, opts.PersonID)
	}
	query += ` ORDER BY person_id, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list relationship reviews: %w", err)
	}
	defer func() { _ = rows.Close() }()
	reviews := make([]RelationshipReview, 0)
	for rows.Next() {
		review, scanErr := scanRelationshipReview(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan relationship review: %w", scanErr)
		}
		reviews = append(reviews, *review)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate relationship reviews: %w", err)
	}
	return reviews, nil
}

// AcceptRelationshipReviewContext claims the pending row, writes its edge,
// and records that edge ID in one transaction. Any edge failure rolls the
// conditional claim back, so review and edge can never diverge.
func (s *Store) AcceptRelationshipReviewContext(ctx context.Context, id int64, typeSlug string, targetPersonID int64, actor string) (*PersonRelationship, error) {
	trimmedActor, err := validateRelationshipActor(actor)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRelationshipReviewInvalid, err)
	}
	if targetPersonID <= 0 {
		return nil, fmt.Errorf("%w: target person ID must be positive", ErrRelationshipReviewInvalid)
	}
	var edge *PersonRelationship
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		review, txErr := s.claimRelationshipReviewTx(ctx, tx, id, RelationshipReviewAccepted, trimmedActor)
		if txErr != nil {
			return txErr
		}
		input := PersonRelationshipInput{SourcePersonID: review.PersonID, TargetPersonID: targetPersonID, TypeSlug: typeSlug, Source: review.Source, SourceRef: review.SourceRef, VCardIdentity: review.VCardIdentity, Actor: trimmedActor}
		validatedActor, notes, txErr := validatePersonRelationshipInput(input)
		if txErr != nil {
			return txErr
		}
		edge, txErr = s.addPersonRelationshipTx(ctx, tx, input, validatedActor, notes)
		if txErr != nil {
			return txErr
		}
		_, txErr = tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE person_relationship_reviews
			SET accepted_relationship_id = ?, updated_at = %s
			WHERE id = ?`, s.dialect.Now()), edge.ID, id)
		if txErr != nil {
			return fmt.Errorf("record accepted relationship for review %d: %w", id, txErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return edge, nil
}

// RejectRelationshipReviewContext atomically changes only a pending review.
func (s *Store) RejectRelationshipReviewContext(ctx context.Context, id int64, actor string) (*RelationshipReview, error) {
	trimmedActor, err := validateRelationshipActor(actor)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRelationshipReviewInvalid, err)
	}
	var rejected *RelationshipReview
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		rejected, err = s.claimRelationshipReviewTx(ctx, tx, id, RelationshipReviewRejected, trimmedActor)
		return err
	})
	if err != nil {
		return nil, err
	}
	return rejected, nil
}

func (s *Store) claimRelationshipReviewTx(ctx context.Context, tx *loggedTx, id int64, status RelationshipReviewStatus, actor string) (*RelationshipReview, error) {
	var updatedID int64
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`
		UPDATE person_relationship_reviews
		SET status = ?, reviewed_by = ?, reviewed_at = %s, updated_at = %s
		WHERE id = ? AND status = ?
		RETURNING id`, s.dialect.Now(), s.dialect.Now()),
		string(status), actor, id, string(RelationshipReviewPending)).Scan(&updatedID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, s.relationshipReviewPendingMissTx(ctx, tx, id)
	}
	if err != nil {
		return nil, fmt.Errorf("claim relationship review %d: %w", id, err)
	}
	return s.relationshipReviewTx(ctx, tx, updatedID)
}

func (s *Store) relationshipReviewPendingMissTx(ctx context.Context, tx *loggedTx, id int64) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_relationship_reviews WHERE id = ?`, id).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check relationship review %d: %w", id, err)
	}
	if exists == 0 {
		return fmt.Errorf("%w: id %d", ErrRelationshipReviewNotFound, id)
	}
	return fmt.Errorf("%w: id %d", ErrRelationshipReviewNotPending, id)
}

const relationshipReviewColumns = `
	id, person_id, raw_related_value, raw_related_type, value_kind,
	matched_person_id, status, accepted_relationship_id, source, source_ref,
	vcard_property, vcard_group, vcard_prop_id, vcard_pid, vcard_altid,
	created_by, reviewed_by, reviewed_at, created_at, updated_at
`

func (s *Store) relationshipReviewTx(ctx context.Context, tx *loggedTx, id int64) (*RelationshipReview, error) {
	review, err := scanRelationshipReview(tx.QueryRowContext(ctx, `SELECT `+relationshipReviewColumns+` FROM person_relationship_reviews WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: id %d", ErrRelationshipReviewNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("get relationship review %d: %w", id, err)
	}
	return review, nil
}

func (s *Store) relationshipReviewByOccurrenceTx(ctx context.Context, tx *loggedTx, in RelatedImport) (*RelationshipReview, error) {
	review, err := scanRelationshipReview(tx.QueryRowContext(ctx, `SELECT `+relationshipReviewColumns+`
		FROM person_relationship_reviews
		WHERE person_id = ? AND raw_related_type = ? AND raw_related_value = ? AND source = ?
		  AND COALESCE(source_ref, '') = ?
		  AND COALESCE(vcard_property, '') = ?
		  AND COALESCE(vcard_group, '') = ?
		  AND COALESCE(vcard_prop_id, '') = ?
		  AND COALESCE(vcard_pid, '') = ?
		  AND COALESCE(vcard_altid, '') = ?`,
		in.PersonID, in.RawType, in.RawValue, string(in.Source),
		normalizedRelationshipReviewSourceRef(in.SourceRef),
		in.VCardIdentity.Property,
		relationshipReviewIdentityPointer(in.VCardIdentity.Group),
		relationshipReviewIdentityPointer(in.VCardIdentity.PropID),
		strings.Join(in.VCardIdentity.PID, ","),
		relationshipReviewIdentityPointer(in.VCardIdentity.AltID),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRelationshipReviewNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("look up staged relationship review: %w", err)
	}
	return review, nil
}

func normalizedRelationshipReviewSourceRef(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func relationshipReviewIdentityPointer(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func matchedPersonArg(id int64, matched bool) sql.NullInt64 {
	return sql.NullInt64{Int64: id, Valid: matched}
}

func scanRelationshipReview(row scanner) (*RelationshipReview, error) {
	var (
		review     RelationshipReview
		valueKind  string
		matchedID  sql.NullInt64
		status     string
		acceptedID sql.NullInt64
		source     string
		sourceRef  sql.NullString
		property   sql.NullString
		group      sql.NullString
		propID     sql.NullString
		pid        sql.NullString
		altID      sql.NullString
		reviewedBy sql.NullString
		reviewedAt sql.NullTime
	)
	if err := row.Scan(&review.ID, &review.PersonID, &review.RawRelatedValue, &review.RawRelatedType, &valueKind,
		&matchedID, &status, &acceptedID, &source, &sourceRef, &property, &group, &propID, &pid, &altID,
		&review.CreatedBy, &reviewedBy, &reviewedAt, &review.CreatedAt, &review.UpdatedAt); err != nil {
		return nil, err
	}
	review.ValueKind = RelatedValueKind(valueKind)
	review.Status = RelationshipReviewStatus(status)
	review.Source = Provenance(source)
	if matchedID.Valid {
		review.MatchedPersonID = &matchedID.Int64
	}
	if acceptedID.Valid {
		review.AcceptedRelationshipID = &acceptedID.Int64
	}
	if sourceRef.Valid {
		review.SourceRef = &sourceRef.String
	}
	review.VCardIdentity = scanVCardIdentity(property, group, propID, pid, altID)
	if reviewedBy.Valid {
		review.ReviewedBy = &reviewedBy.String
	}
	if reviewedAt.Valid {
		review.ReviewedAt = &reviewedAt.Time
	}
	return &review, nil
}
