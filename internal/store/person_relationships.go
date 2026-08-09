package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

var (
	ErrPersonRelationshipNotFound         = errors.New("person relationship not found")
	ErrPersonRelationshipRevisionConflict = errors.New("person relationship revision conflict")
	ErrPersonRelationshipDuplicate        = errors.New("an active relationship of this type already exists")
	ErrPersonRelationshipSelf             = errors.New("a person cannot have a relationship with itself")
	ErrPersonRelationshipInvalid          = errors.New("invalid person relationship")
)

const (
	maxRelationshipNotesBytes = 4096
	maxRelationshipActorBytes = 128
)

// RelationshipStatus is derived from the world-time interval and never comes
// from a caller, so it cannot disagree with the edge's end bound.
type RelationshipStatus string

const (
	RelationshipStatusActive RelationshipStatus = "active"
	RelationshipStatusEnded  RelationshipStatus = "ended"
)

func statusForInterval(endDate *PartialDate) RelationshipStatus {
	if endDate == nil {
		return RelationshipStatusActive
	}
	return RelationshipStatusEnded
}

// PersonRelationship is one canonical edge joined to the presentation fields
// of its relationship type.
type PersonRelationship struct {
	ID                 int64              `json:"id"`
	SourcePersonID     int64              `json:"source_person_id"`
	TargetPersonID     int64              `json:"target_person_id"`
	RelationshipTypeID int64              `json:"relationship_type_id"`
	TypeSlug           string             `json:"type_slug"`
	ForwardLabel       string             `json:"forward_label"`
	ReverseLabel       string             `json:"reverse_label"`
	IsSymmetric        bool               `json:"is_symmetric"`
	StartDate          *PartialDate       `json:"start_date,omitempty"`
	EndDate            *PartialDate       `json:"end_date,omitempty"`
	Status             RelationshipStatus `json:"status"`
	Notes              *string            `json:"notes,omitempty"`
	Source             Provenance         `json:"source"`
	SourceRef          *string            `json:"source_ref,omitempty"`
	Confidence         *float64           `json:"confidence,omitempty"`
	VCardIdentity      VCardIdentity      `json:"vcard_identity"`
	CreatedBy          string             `json:"created_by"`
	UpdatedBy          string             `json:"updated_by"`
	Revision           int64              `json:"revision"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

// PersonRelationshipInput declares one relationship. Status is derived from
// EndDate rather than accepted as mutable caller input.
type PersonRelationshipInput struct {
	SourcePersonID int64
	TargetPersonID int64
	TypeSlug       string
	StartDate      *PartialDate
	EndDate        *PartialDate
	Notes          *string
	Source         Provenance
	SourceRef      *string
	Confidence     *float64
	VCardIdentity  VCardIdentity
	Actor          string
}

// PersonRelationshipPatch replaces one or both mutable edge values through a
// single revision-qualified update. The boolean fields distinguish an omitted
// value from an explicit request to clear notes.
type PersonRelationshipPatch struct {
	EndDate       *PartialDate
	UpdateEndDate bool
	Notes         *string
	UpdateNotes   bool
}

const personRelationshipColumns = `
	r.id, r.source_person_id, r.target_person_id, r.relationship_type_id,
	t.slug, t.forward_label, t.reverse_label, t.is_symmetric,
	r.start_year, r.start_month, r.start_day,
	r.end_year, r.end_month, r.end_day, r.status, r.notes,
	r.source, r.source_ref, r.confidence,
	r.vcard_property, r.vcard_group, r.vcard_prop_id, r.vcard_pid, r.vcard_altid,
	r.created_by, r.updated_by, r.revision, r.created_at, r.updated_at
`

const personRelationshipFrom = `
	FROM person_relationships r
	JOIN relationship_types t ON t.id = r.relationship_type_id
`

// AddPersonRelationshipContext stores one edge in canonical orientation.
// A non-canonical inverse type first swaps endpoints and resolves to its
// canonical type; then a symmetric canonical type orders endpoints by ID.
func (s *Store) AddPersonRelationshipContext(
	ctx context.Context, input PersonRelationshipInput,
) (*PersonRelationship, error) {
	actor, notes, err := validatePersonRelationshipInput(input)
	if err != nil {
		return nil, err
	}
	var created *PersonRelationship
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		var txErr error
		created, txErr = s.addPersonRelationshipTx(ctx, tx, input, actor, notes)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func validatePersonRelationshipInput(input PersonRelationshipInput) (string, sql.NullString, error) {
	if _, err := ParseProvenance(string(input.Source)); err != nil {
		return "", sql.NullString{}, err
	}
	actor, err := validateRelationshipActor(input.Actor)
	if err != nil {
		return "", sql.NullString{}, err
	}
	if input.SourcePersonID <= 0 || input.TargetPersonID <= 0 {
		return "", sql.NullString{}, fmt.Errorf("%w: person IDs must be positive", ErrPersonRelationshipInvalid)
	}
	if input.SourcePersonID == input.TargetPersonID {
		return "", sql.NullString{}, fmt.Errorf("%w: person %d", ErrPersonRelationshipSelf, input.SourcePersonID)
	}
	if err := validateRelationshipInterval(input.StartDate, input.EndDate); err != nil {
		return "", sql.NullString{}, err
	}
	if err := validateRelationshipConfidence(input.Source, input.Confidence); err != nil {
		return "", sql.NullString{}, err
	}
	notes, err := normalizeRelationshipNotes(input.Notes)
	if err != nil {
		return "", sql.NullString{}, err
	}
	return actor, notes, nil
}

// addPersonRelationshipTx is the transaction-scoped form used by both the
// public add method and review acceptance. Its caller must validate input
// through validatePersonRelationshipInput before entering the transaction.
func (s *Store) addPersonRelationshipTx(
	ctx context.Context, tx *loggedTx, input PersonRelationshipInput, actor string, notes sql.NullString,
) (*PersonRelationship, error) {
	relationshipType, sourceID, targetID, err := s.canonicalRelationshipEndpointsTx(ctx, tx, input.SourcePersonID, input.TargetPersonID, input.TypeSlug)
	if err != nil {
		return nil, err
	}
	if err := s.requirePersonsExistTx(ctx, tx, sourceID, targetID); err != nil {
		return nil, err
	}

	args := []any{sourceID, targetID, relationshipType.ID}
	args = append(args, relationshipDateArgs(input.StartDate)...)
	args = append(args, relationshipDateArgs(input.EndDate)...)
	args = append(args,
		string(statusForInterval(input.EndDate)), notes,
		string(input.Source), normalizeNullableText(input.SourceRef), confidenceArg(input.Confidence),
		nullableVCardText(input.VCardIdentity.Property),
		nullableVCardPointer(input.VCardIdentity.Group),
		nullableVCardPointer(input.VCardIdentity.PropID),
		nullableVCardText(strings.Join(input.VCardIdentity.PID, ",")),
		nullableVCardPointer(input.VCardIdentity.AltID), actor, actor,
	)

	var insertedID int64
	err = tx.QueryRowContext(ctx, `
			INSERT INTO person_relationships (
				source_person_id, target_person_id, relationship_type_id,
				start_year, start_month, start_day, end_year, end_month, end_day,
				status, notes, source, source_ref, confidence,
				vcard_property, vcard_group, vcard_prop_id, vcard_pid, vcard_altid,
				created_by, updated_by
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id
	`, args...).Scan(&insertedID)
	if err != nil {
		if s.dialect.IsConflictError(err) {
			return nil, fmt.Errorf("%w: %d -> %d as %q", ErrPersonRelationshipDuplicate, sourceID, targetID, relationshipType.Slug)
		}
		return nil, fmt.Errorf("add person relationship: %w", err)
	}
	return s.personRelationshipTx(ctx, tx, insertedID)
}

func (s *Store) canonicalRelationshipEndpointsTx(
	ctx context.Context, tx *loggedTx, sourceID, targetID int64, typeSlug string,
) (*RelationshipType, int64, int64, error) {
	relationshipType, err := s.relationshipTypeBySlugTx(ctx, tx, typeSlug)
	if err != nil {
		return nil, 0, 0, err
	}
	if !relationshipType.IsCanonical {
		if relationshipType.InverseTypeID == nil {
			return nil, 0, 0, fmt.Errorf("%w: type %q is non-canonical without an inverse", ErrPersonRelationshipInvalid, relationshipType.Slug)
		}
		sourceID, targetID = targetID, sourceID
		relationshipType, err = s.relationshipTypeByIDTx(ctx, tx, *relationshipType.InverseTypeID)
		if err != nil {
			return nil, 0, 0, err
		}
	}
	if relationshipType.IsSymmetric && sourceID > targetID {
		sourceID, targetID = targetID, sourceID
	}
	return relationshipType, sourceID, targetID, nil
}

func (s *Store) GetPersonRelationshipContext(ctx context.Context, id int64) (*PersonRelationship, error) {
	var edge *PersonRelationship
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var txErr error
		edge, txErr = s.personRelationshipTx(ctx, tx, id)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	return edge, nil
}

// EndPersonRelationshipContext closes a world-time interval with revision
// compare-and-swap, retaining the historical edge.
func (s *Store) EndPersonRelationshipContext(
	ctx context.Context, id, expectedRevision int64, until PartialDate, actor string,
) (*PersonRelationship, error) {
	return s.PatchPersonRelationshipContext(ctx, id, expectedRevision,
		PersonRelationshipPatch{EndDate: &until, UpdateEndDate: true}, actor)
}

func (s *Store) UpdatePersonRelationshipNotesContext(
	ctx context.Context, id, expectedRevision int64, notes *string, actor string,
) (*PersonRelationship, error) {
	return s.PatchPersonRelationshipContext(ctx, id, expectedRevision,
		PersonRelationshipPatch{Notes: notes, UpdateNotes: true}, actor)
}

// PatchPersonRelationshipContext atomically ends an edge and/or replaces its
// notes. All supplied values and the actor are validated before it begins its
// one transaction; a single revision-qualified UPDATE applies every change
// and increments the revision once.
func (s *Store) PatchPersonRelationshipContext(
	ctx context.Context, id, expectedRevision int64, patch PersonRelationshipPatch, actor string,
) (*PersonRelationship, error) {
	if !patch.UpdateEndDate && !patch.UpdateNotes {
		return nil, fmt.Errorf("%w: end date or notes is required", ErrPersonRelationshipInvalid)
	}
	if patch.UpdateEndDate {
		if patch.EndDate == nil {
			return nil, fmt.Errorf("%w: end date is required", ErrPersonRelationshipInvalid)
		}
		if err := validateRelationshipBound(patch.EndDate); err != nil {
			return nil, err
		}
	}
	var notes sql.NullString
	var err error
	if patch.UpdateNotes {
		notes, err = normalizeRelationshipNotes(patch.Notes)
		if err != nil {
			return nil, err
		}
	}
	trimmedActor, err := validateRelationshipActor(actor)
	if err != nil {
		return nil, err
	}
	var updated *PersonRelationship
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if patch.UpdateEndDate {
			current, txErr := s.personRelationshipTx(ctx, tx, id)
			if txErr != nil {
				return txErr
			}
			if txErr = validateRelationshipInterval(current.StartDate, patch.EndDate); txErr != nil {
				return txErr
			}
		}
		var updatedID int64
		var txErr error
		switch {
		case patch.UpdateEndDate && patch.UpdateNotes:
			args := append(relationshipDateArgs(patch.EndDate), string(RelationshipStatusEnded), notes,
				trimmedActor, id, expectedRevision)
			txErr = tx.QueryRowContext(ctx, fmt.Sprintf(`
				UPDATE person_relationships
				SET end_year = ?, end_month = ?, end_day = ?, status = ?, notes = ?, updated_by = ?,
				    revision = revision + 1, updated_at = %s
				WHERE id = ? AND revision = ?
				RETURNING id
			`, s.dialect.Now()), args...).Scan(&updatedID)
		case patch.UpdateEndDate:
			args := append(relationshipDateArgs(patch.EndDate), string(RelationshipStatusEnded), trimmedActor,
				id, expectedRevision)
			txErr = tx.QueryRowContext(ctx, fmt.Sprintf(`
				UPDATE person_relationships
				SET end_year = ?, end_month = ?, end_day = ?, status = ?, updated_by = ?,
				    revision = revision + 1, updated_at = %s
				WHERE id = ? AND revision = ?
				RETURNING id
			`, s.dialect.Now()), args...).Scan(&updatedID)
		default:
			txErr = tx.QueryRowContext(ctx, fmt.Sprintf(`
				UPDATE person_relationships
				SET notes = ?, updated_by = ?, revision = revision + 1, updated_at = %s
				WHERE id = ? AND revision = ?
				RETURNING id
			`, s.dialect.Now()), notes, trimmedActor, id, expectedRevision).Scan(&updatedID)
		}
		if errors.Is(txErr, sql.ErrNoRows) {
			return s.personRelationshipCASMissTx(ctx, tx, id)
		}
		if txErr != nil {
			return fmt.Errorf("patch person relationship %d: %w", id, txErr)
		}
		updated, txErr = s.personRelationshipTx(ctx, tx, updatedID)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) DeletePersonRelationshipContext(ctx context.Context, id, expectedRevision int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var deletedID int64
		err := tx.QueryRowContext(ctx,
			`DELETE FROM person_relationships WHERE id = ? AND revision = ? RETURNING id`, id, expectedRevision,
		).Scan(&deletedID)
		if errors.Is(err, sql.ErrNoRows) {
			return s.personRelationshipCASMissTx(ctx, tx, id)
		}
		if err != nil {
			return fmt.Errorf("delete person relationship %d: %w", id, err)
		}
		return nil
	})
}

func (s *Store) personRelationshipCASMissTx(ctx context.Context, tx *loggedTx, id int64) error {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT revision FROM person_relationships WHERE id = ?`, id).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPersonRelationshipNotFound
	}
	if err != nil {
		return fmt.Errorf("check person relationship %d after revision miss: %w", id, err)
	}
	return ErrPersonRelationshipRevisionConflict
}

func (s *Store) personRelationshipTx(ctx context.Context, tx *loggedTx, id int64) (*PersonRelationship, error) {
	edge, err := scanPersonRelationship(tx.QueryRowContext(ctx,
		`SELECT `+personRelationshipColumns+personRelationshipFrom+` WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPersonRelationshipNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get person relationship %d: %w", id, err)
	}
	return edge, nil
}

func (s *Store) relationshipTypeBySlugTx(ctx context.Context, tx *loggedTx, slug string) (*RelationshipType, error) {
	trimmed := strings.TrimSpace(slug)
	relationshipType, err := scanRelationshipType(tx.QueryRowContext(ctx,
		`SELECT `+relationshipTypeColumns+` FROM relationship_types WHERE slug = ?`, trimmed))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrRelationshipTypeNotFound, trimmed)
	}
	if err != nil {
		return nil, fmt.Errorf("load relationship type %q: %w", trimmed, err)
	}
	return relationshipType, nil
}

func (s *Store) relationshipTypeByIDTx(ctx context.Context, tx *loggedTx, id int64) (*RelationshipType, error) {
	relationshipType, err := scanRelationshipType(tx.QueryRowContext(ctx,
		`SELECT `+relationshipTypeColumns+` FROM relationship_types WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: id %d", ErrRelationshipTypeNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("load relationship type %d: %w", id, err)
	}
	return relationshipType, nil
}

func (s *Store) requirePersonsExistTx(ctx context.Context, tx *loggedTx, ids ...int64) error {
	for _, id := range ids {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM persons WHERE id = ?`, id).Scan(&exists); err != nil {
			return fmt.Errorf("verify person %d: %w", id, err)
		}
		if exists == 0 {
			return fmt.Errorf("%w: id %d", ErrPersonNotFound, id)
		}
	}
	return nil
}

func validateRelationshipConfidence(source Provenance, confidence *float64) error {
	if confidence == nil {
		return nil
	}
	if source.IsDeclared() {
		return fmt.Errorf("%w: declared provenance %q must not carry a confidence score", ErrConfidenceScope, source)
	}
	if math.IsNaN(*confidence) || *confidence < 0 || *confidence > 1 {
		return fmt.Errorf("%w: confidence %v is outside [0,1]", ErrConfidenceScope, *confidence)
	}
	return nil
}

func validateRelationshipActor(actor string) (string, error) {
	trimmed := strings.TrimSpace(actor)
	if trimmed == "" {
		return "", fmt.Errorf("%w: actor is required", ErrPersonRelationshipInvalid)
	}
	if len(trimmed) > maxRelationshipActorBytes {
		return "", fmt.Errorf("%w: actor exceeds %d bytes", ErrPersonRelationshipInvalid, maxRelationshipActorBytes)
	}
	return trimmed, nil
}

func normalizeRelationshipNotes(notes *string) (sql.NullString, error) {
	if notes == nil {
		return sql.NullString{}, nil
	}
	trimmed := strings.TrimSpace(*notes)
	if trimmed == "" {
		return sql.NullString{}, nil
	}
	if len(trimmed) > maxRelationshipNotesBytes {
		return sql.NullString{}, fmt.Errorf("%w: notes exceed %d bytes", ErrPersonRelationshipInvalid, maxRelationshipNotesBytes)
	}
	return sql.NullString{String: trimmed, Valid: true}, nil
}

func confidenceArg(value *float64) sql.NullFloat64 {
	if value == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *value, Valid: true}
}

func relationshipDateArgs(bound *PartialDate) []any {
	if bound == nil {
		return []any{nil, nil, nil}
	}
	return PartialDateArgs(*bound)
}

func nullableVCardText(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func nullableVCardPointer(value *string) sql.NullString {
	if value == nil {
		return sql.NullString{}
	}
	return nullableVCardText(*value)
}

func scanVCardIdentity(property, group, propID, pid, altID sql.NullString) VCardIdentity {
	identity := VCardIdentity{Property: property.String}
	if group.Valid {
		identity.Group = &group.String
	}
	if propID.Valid {
		identity.PropID = &propID.String
	}
	if pid.Valid && pid.String != "" {
		identity.PID = strings.Split(pid.String, ",")
	}
	if altID.Valid {
		identity.AltID = &altID.String
	}
	return identity
}

type personRelationshipScan struct {
	edge       PersonRelationship
	startYear  sql.NullInt64
	startMonth sql.NullInt64
	startDay   sql.NullInt64
	endYear    sql.NullInt64
	endMonth   sql.NullInt64
	endDay     sql.NullInt64
	status     string
	notes      sql.NullString
	source     string
	sourceRef  sql.NullString
	confidence sql.NullFloat64
	property   sql.NullString
	group      sql.NullString
	propID     sql.NullString
	pid        sql.NullString
	altID      sql.NullString
}

// destinations is the sole mapping for personRelationshipColumns. Both edge
// and endpoint-view scanners use it, keeping the canonical row hydration in
// lockstep as columns evolve.
func (scan *personRelationshipScan) destinations() []any {
	return []any{
		&scan.edge.ID, &scan.edge.SourcePersonID, &scan.edge.TargetPersonID, &scan.edge.RelationshipTypeID,
		&scan.edge.TypeSlug, &scan.edge.ForwardLabel, &scan.edge.ReverseLabel, &scan.edge.IsSymmetric,
		&scan.startYear, &scan.startMonth, &scan.startDay, &scan.endYear, &scan.endMonth, &scan.endDay, &scan.status, &scan.notes,
		&scan.source, &scan.sourceRef, &scan.confidence, &scan.property, &scan.group, &scan.propID, &scan.pid, &scan.altID,
		&scan.edge.CreatedBy, &scan.edge.UpdatedBy, &scan.edge.Revision, &scan.edge.CreatedAt, &scan.edge.UpdatedAt,
	}
}

func (scan *personRelationshipScan) relationship() PersonRelationship {
	edge := scan.edge
	if scan.startYear.Valid {
		value := ScanPartialDate(scan.startYear, scan.startMonth, scan.startDay)
		edge.StartDate = &value
	}
	if scan.endYear.Valid {
		value := ScanPartialDate(scan.endYear, scan.endMonth, scan.endDay)
		edge.EndDate = &value
	}
	edge.Status = RelationshipStatus(scan.status)
	edge.Source = Provenance(scan.source)
	if scan.notes.Valid {
		edge.Notes = &scan.notes.String
	}
	if scan.sourceRef.Valid {
		edge.SourceRef = &scan.sourceRef.String
	}
	if scan.confidence.Valid {
		edge.Confidence = &scan.confidence.Float64
	}
	edge.VCardIdentity = scanVCardIdentity(scan.property, scan.group, scan.propID, scan.pid, scan.altID)
	return edge
}

func scanPersonRelationship(row scanner) (*PersonRelationship, error) {
	var scan personRelationshipScan
	if err := row.Scan(scan.destinations()...); err != nil {
		return nil, err
	}
	edge := scan.relationship()
	return &edge, nil
}

// PersonRelationshipView is one canonical edge rendered from one endpoint's
// point of view. Direction and CounterpartLabel are computed from the row's
// orientation and its type's labels; there is no second stored row.
type PersonRelationshipView struct {
	Relationship           PersonRelationship    `json:"relationship"`
	Direction              RelationshipDirection `json:"direction"`
	CounterpartPersonID    int64                 `json:"counterpart_person_id"`
	CounterpartLabel       string                `json:"counterpart_label"`
	CounterpartDisplayName *string               `json:"counterpart_display_name,omitempty"`
	CounterpartVCardUID    string                `json:"counterpart_vcard_uid"`
}

// PersonRelationshipListOptions scopes an endpoint view. The default shows
// only currently-true relationships; IncludeEnded adds closed intervals so a
// caller can render history without a second entry point.
type PersonRelationshipListOptions struct {
	IncludeEnded bool
}

// ListPersonRelationshipsContext returns every relationship in which personID
// participates, rendered from that person's side.
//
// The counterpart join is an equality join on persons(id) through the same
// CASE expression used for the counterpart ID, so each edge yields exactly
// one row without DISTINCT. A self-edge would break that guarantee, which is
// why the table forbids one structurally.
//
// Ordering is CASE-based rather than a bare boolean sort so SQLite's 0/1
// integers and PostgreSQL's booleans produce the same sequence.
func (s *Store) ListPersonRelationshipsContext(
	ctx context.Context, personID int64, opts PersonRelationshipListOptions,
) ([]PersonRelationshipView, error) {
	currentFilter := ""
	if !opts.IncludeEnded {
		currentFilter = " AND r.end_year IS NULL"
	}
	query := `
		SELECT ` + personRelationshipColumns + `,
		       CASE WHEN r.source_person_id = ? THEN r.target_person_id
		            ELSE r.source_person_id END AS counterpart_person_id,
		       CASE WHEN r.source_person_id = ? THEN ? ELSE ? END AS direction,
		       CASE WHEN r.source_person_id = ? THEN t.reverse_label
		            ELSE t.forward_label END AS counterpart_label,
		       cp.display_name AS counterpart_display_name,
		       cp.vcard_uid AS counterpart_vcard_uid
		` + personRelationshipFrom + `
		JOIN persons cp ON cp.id = CASE WHEN r.source_person_id = ?
		                                THEN r.target_person_id
		                                ELSE r.source_person_id END
		WHERE (r.source_person_id = ? OR r.target_person_id = ?)` + currentFilter + `
		ORDER BY CASE WHEN r.end_year IS NULL THEN 0 ELSE 1 END,
		         LOWER(COALESCE(cp.display_name, cp.vcard_uid)),
		         t.slug, r.id
	`
	rows, err := s.db.QueryContext(ctx, query,
		personID,
		personID, string(RelationshipDirectionOutgoing), string(RelationshipDirectionIncoming),
		personID,
		personID,
		personID, personID,
	)
	if err != nil {
		return nil, fmt.Errorf("list relationships for person %d: %w", personID, err)
	}
	defer func() { _ = rows.Close() }()

	views := make([]PersonRelationshipView, 0)
	for rows.Next() {
		view, scanErr := scanPersonRelationshipView(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan relationship for person %d: %w", personID, scanErr)
		}
		views = append(views, *view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate relationships for person %d: %w", personID, err)
	}
	return views, nil
}

func scanPersonRelationshipView(row scanner) (*PersonRelationshipView, error) {
	var (
		scan        personRelationshipScan
		view        PersonRelationshipView
		direction   string
		displayName sql.NullString
	)
	destinations := scan.destinations()
	destinations = append(destinations,
		&view.CounterpartPersonID, &direction, &view.CounterpartLabel,
		&displayName, &view.CounterpartVCardUID,
	)
	if err := row.Scan(destinations...); err != nil {
		return nil, err
	}
	view.Relationship = scan.relationship()
	view.Direction = RelationshipDirection(direction)
	if displayName.Valid {
		view.CounterpartDisplayName = &displayName.String
	}
	return &view, nil
}
