package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// PersonVCardMediaData is the inline payload for one current media value.
// Metadata remains on PersonProfile.Media; separating bytes keeps ordinary
// profile reads compact while making the native projection snapshot complete.
type PersonVCardMediaData struct {
	MediaID   int64  `json:"media_id"`
	MediaType string `json:"media_type,omitempty"`
	Data      []byte `json:"data"`
}

// PersonVCardAttribute pairs one portable vCard-mapped definition with all of
// its current values. Definitions with no value are retained so a concurrent
// insertion changes the projection fingerprint.
type PersonVCardAttribute struct {
	Definition AttributeDefinition    `json:"definition"`
	Values     []PersonAttributeValue `json:"values"`
}

// PersonVCardEmployment carries the organization profile selected by one
// employment without requiring a second, independently timed store read.
type PersonVCardEmployment struct {
	Employment   Employment          `json:"employment"`
	Organization OrganizationProfile `json:"organization"`
}

// PersonVCardSnapshot contains every semantic row that can affect a native
// vCard projection. All fields are loaded in one repeatable-read transaction.
//
// Fingerprint and ProjectionRevision are both excluded from the JSON encoding
// the fingerprint is computed over. Fingerprint because it is the output;
// ProjectionRevision because it is serialization metadata rather than semantic
// content — folding it in would make an unrelated catalog bump look like this
// person's card changed, and would defeat the no-op detection that keeps a
// re-render of unchanged content from advancing the envelope revision. The
// same goes for the watermarks inside the aggregate; see
// personVCardFingerprintView.
type PersonVCardSnapshot struct {
	Profile                    PersonProfile            `json:"profile"`
	MediaData                  []PersonVCardMediaData   `json:"media_data"`
	Attributes                 []PersonVCardAttribute   `json:"attributes"`
	Employments                []PersonVCardEmployment  `json:"employments"`
	Relationships              []PersonRelationshipView `json:"relationships"`
	RelationshipTypes          []RelationshipType       `json:"relationship_types"`
	PendingRelationshipReviews []RelationshipReview     `json:"pending_relationship_reviews"`
	// AcceptedRelationshipReviews lets a projection hand a review-owned RELATED
	// occurrence to the edge that satisfied it, per resource: the envelope's
	// own mapping is what knows which occurrence on which card the review
	// stood for, and the edge's single vCard identity cannot say that for
	// every card the edge appears on.
	AcceptedRelationshipReviews []PersonVCardAcceptedReview `json:"accepted_relationship_reviews"`
	Fingerprint                 string                      `json:"-"`
	ProjectionRevision          int64                       `json:"-"`
}

// PersonVCardAcceptedReview binds an accepted relationship review to the edge
// that satisfied it. VCardIdentity, SourceRef, and SourceResourceUID are the
// review's: the occurrence it stood for and the complete identity of the
// resource that occurrence lives in. They stay on the review rather than being
// copied onto the edge, because the edge sits on every card of both endpoints
// and a single identity on it could not say which card's occurrence it means.
type PersonVCardAcceptedReview struct {
	ReviewID          int64         `json:"review_id"`
	RelationshipID    int64         `json:"relationship_id"`
	VCardIdentity     VCardIdentity `json:"vcard_identity"`
	SourceRef         *string       `json:"source_ref,omitempty"`
	SourceResourceUID *string       `json:"source_resource_uid,omitempty"`
}

// VCardProjectionConflictError reports that semantic input changed after a
// caller rendered an envelope from a snapshot.
type VCardProjectionConflictError struct {
	PersonID int64
	Expected string
	Actual   string
}

func (e *VCardProjectionConflictError) Error() string {
	// A conflict caught by the projection row lock knows that semantic input
	// changed but not what it changed to: the transaction that would have
	// read the new state is the one the backend aborted.
	if e.Actual == "" {
		return fmt.Sprintf(
			"%s: person %d changed concurrently with a commit rendered from fingerprint %q",
			ErrVCardProjectionConflict, e.PersonID, e.Expected,
		)
	}
	return fmt.Sprintf(
		"%s: person %d fingerprint changed from %q to %q",
		ErrVCardProjectionConflict, e.PersonID, e.Expected, e.Actual,
	)
}

func (e *VCardProjectionConflictError) Unwrap() error {
	return ErrVCardProjectionConflict
}

// LoadPersonVCardSnapshotContext returns one stable aggregate plus its
// deterministic fingerprint.
func (s *Store) LoadPersonVCardSnapshotContext(
	ctx context.Context, personID int64,
) (*PersonVCardSnapshot, error) {
	var snapshot *PersonVCardSnapshot
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		snapshot, err = s.loadPersonVCardSnapshotTx(ctx, tx, personID)
		return err
	})
	return snapshot, err
}

func (s *Store) recheckPersonVCardProjectionTx(
	ctx context.Context, tx *loggedTx,
	personID int64, expectedFingerprint string,
) error {
	if expectedFingerprint == "" {
		return fmt.Errorf("%w: projection fingerprint is required", ErrVCardResourceInvalid)
	}
	current, err := s.loadPersonVCardSnapshotTx(ctx, tx, personID)
	if err != nil {
		return err
	}
	if current.Fingerprint != expectedFingerprint {
		return &VCardProjectionConflictError{
			PersonID: personID, Expected: expectedFingerprint,
			Actual: current.Fingerprint,
		}
	}
	return nil
}

func (s *Store) loadPersonVCardSnapshotTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (*PersonVCardSnapshot, error) {
	profile, err := s.getPersonProfileTx(ctx, tx, personID, true)
	if err != nil {
		return nil, err
	}
	snapshot := &PersonVCardSnapshot{Profile: *profile}
	if err := tx.QueryRowContext(ctx,
		`SELECT vcard_projection_revision FROM persons WHERE id = ?`, personID,
	).Scan(&snapshot.ProjectionRevision); err != nil {
		return nil, fmt.Errorf(
			"read person %d vCard projection revision: %w", personID, err,
		)
	}
	if snapshot.MediaData, err = loadPersonVCardMediaDataTx(
		ctx, tx, profile.Media,
	); err != nil {
		return nil, err
	}
	if snapshot.Attributes, err = s.loadPersonVCardAttributesTx(
		ctx, tx, personID,
	); err != nil {
		return nil, err
	}
	employments, err := s.listAllEmploymentsContext(
		ctx, tx, EmploymentFilter{PersonID: personID},
	)
	if err != nil {
		return nil, err
	}
	snapshot.Employments = make([]PersonVCardEmployment, 0, len(employments))
	for _, employment := range employments {
		organization, err := getOrganizationTx(
			ctx, tx, employment.OrganizationID,
		)
		if err != nil {
			return nil, err
		}
		organizationProfile, err := s.loadOrganizationProfileContext(
			ctx, tx, organization, false,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"load organization %d for vCard snapshot: %w",
				employment.OrganizationID, err,
			)
		}
		snapshot.Employments = append(snapshot.Employments, PersonVCardEmployment{
			Employment: employment, Organization: *organizationProfile,
		})
	}
	if snapshot.Relationships, err = s.listPersonRelationshipsContext(
		ctx, tx, personID, PersonRelationshipListOptions{},
	); err != nil {
		return nil, err
	}
	if snapshot.RelationshipTypes, err = s.loadVCardRelationshipTypesTx(
		ctx, tx, snapshot.Relationships,
	); err != nil {
		return nil, err
	}
	if snapshot.PendingRelationshipReviews, err = s.listRelationshipReviewsContext(
		ctx, tx, RelationshipReviewListOptions{
			Status: RelationshipReviewPending, PersonID: personID,
		},
	); err != nil {
		return nil, err
	}
	if snapshot.AcceptedRelationshipReviews, err = s.loadVCardAcceptedReviewsTx(
		ctx, tx, personID,
	); err != nil {
		return nil, err
	}
	snapshot.Fingerprint, err = personVCardSnapshotFingerprint(snapshot)
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *Store) loadVCardAcceptedReviewsTx(
	ctx context.Context, tx *loggedTx, personID int64,
) ([]PersonVCardAcceptedReview, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, accepted_relationship_id, source_ref, source_resource_uid,
		       vcard_property, vcard_group, vcard_prop_id, vcard_pid, vcard_altid
		FROM person_relationship_reviews
		WHERE person_id = ? AND status = ? AND accepted_relationship_id IS NOT NULL
		ORDER BY id`, personID, string(RelationshipReviewAccepted))
	if err != nil {
		return nil, fmt.Errorf("load accepted relationship reviews: %w", err)
	}
	defer func() { _ = rows.Close() }()
	accepted := make([]PersonVCardAcceptedReview, 0)
	for rows.Next() {
		var binding PersonVCardAcceptedReview
		var sourceRef, sourceResourceUID, property, group, propID, pid, altID sql.NullString
		if err := rows.Scan(
			&binding.ReviewID, &binding.RelationshipID, &sourceRef, &sourceResourceUID,
			&property, &group, &propID, &pid, &altID,
		); err != nil {
			return nil, fmt.Errorf("scan accepted relationship review: %w", err)
		}
		binding.SourceRef = nullStringPtr(sourceRef)
		binding.SourceResourceUID = nullStringPtr(sourceResourceUID)
		binding.VCardIdentity = scanVCardIdentity(property, group, propID, pid, altID)
		accepted = append(accepted, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accepted relationship reviews: %w", err)
	}
	return accepted, nil
}

func loadPersonVCardMediaDataTx(
	ctx context.Context, tx *loggedTx, media []PersonMedia,
) ([]PersonVCardMediaData, error) {
	data := make([]PersonVCardMediaData, 0)
	for _, item := range media {
		if !item.HasData {
			continue
		}
		var value []byte
		var mediaType *string
		if err := tx.QueryRowContext(ctx,
			`SELECT data, media_type FROM person_media
			 WHERE person_id = ? AND id = ?`,
			item.PersonID, item.Envelope.ID,
		).Scan(&value, &mediaType); err != nil {
			return nil, fmt.Errorf("read person media data for vCard snapshot: %w", err)
		}
		entry := PersonVCardMediaData{
			MediaID: item.Envelope.ID, Data: append([]byte(nil), value...),
		}
		if mediaType != nil {
			entry.MediaType = *mediaType
		}
		data = append(data, entry)
	}
	return data, nil
}

func (s *Store) loadPersonVCardAttributesTx(
	ctx context.Context, tx *loggedTx, personID int64,
) ([]PersonVCardAttribute, error) {
	definitions, err := s.listAttributeDefinitionsContext(
		ctx, tx, AttributeDefinitionFilter{ObjectType: AttributeObjectPerson},
	)
	if err != nil {
		return nil, err
	}
	values, err := s.listPersonAttributeValuesContext(
		ctx, tx, personID, PersonAttributeQuery{},
	)
	if err != nil {
		return nil, err
	}
	valuesByDefinition := make(map[int64][]PersonAttributeValue)
	for _, value := range values {
		valuesByDefinition[value.DefinitionID] = append(
			valuesByDefinition[value.DefinitionID], value,
		)
	}
	attributes := make([]PersonVCardAttribute, 0)
	for _, definition := range definitions {
		if definition.VCardProperty == nil {
			continue
		}
		attributes = append(attributes, PersonVCardAttribute{
			Definition: definition,
			Values: append(
				[]PersonAttributeValue(nil), valuesByDefinition[definition.ID]...,
			),
		})
	}
	return attributes, nil
}

func (s *Store) loadVCardRelationshipTypesTx(
	ctx context.Context, tx *loggedTx, relationships []PersonRelationshipView,
) ([]RelationshipType, error) {
	types := make(map[int64]RelationshipType)
	for _, relationship := range relationships {
		id := relationship.Relationship.RelationshipTypeID
		if _, exists := types[id]; exists {
			continue
		}
		relationshipType, err := s.relationshipTypeByIDTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		types[id] = *relationshipType
		if relationshipType.InverseTypeID != nil {
			inverse, err := s.relationshipTypeByIDTx(
				ctx, tx, *relationshipType.InverseTypeID,
			)
			if err != nil {
				return nil, err
			}
			types[inverse.ID] = *inverse
		}
	}
	ids := make([]int64, 0, len(types))
	for id := range types {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	result := make([]RelationshipType, 0, len(ids))
	for _, id := range ids {
		result = append(result, types[id])
	}
	return result, nil
}

// personVCardSnapshotFingerprint hashes the projection-relevant view of the
// snapshot, so the fingerprint changes exactly when projected content does.
func personVCardSnapshotFingerprint(snapshot *PersonVCardSnapshot) (string, error) {
	encoded, err := json.Marshal(personVCardFingerprintView(snapshot))
	if err != nil {
		return "", fmt.Errorf("encode person vCard snapshot: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// personVCardFingerprintView copies the snapshot with every field that is not
// projection input zeroed. Revisions and timestamps are compare-and-swap
// tokens and watermarks: the person record's move when a participant is
// linked or a message is imported, which changes no card, and a render made
// just before such a write must still commit rather than conflict and produce
// an identical card. Definition presentation fields (label, description,
// display order) are likewise never projected, and their edits do not bump
// the projection revision. Everything else stays, including the value
// envelopes' own timestamps: those change only through component writes that
// do bump. The projection revision remains the lock; this is only what the
// no-op check compares.
func personVCardFingerprintView(snapshot *PersonVCardSnapshot) PersonVCardSnapshot {
	view := *snapshot
	view.Fingerprint = ""
	view.Profile.Person = vcardFingerprintPerson(view.Profile.Person)
	view.Employments = slices.Clone(view.Employments)
	for i := range view.Employments {
		employment := &view.Employments[i]
		employment.Employment.Revision = 0
		employment.Employment.CreatedAt = time.Time{}
		employment.Employment.UpdatedAt = time.Time{}
		employment.Organization.Organization.Revision = 0
		employment.Organization.Organization.CreatedAt = time.Time{}
		employment.Organization.Organization.UpdatedAt = time.Time{}
	}
	view.Attributes = slices.Clone(view.Attributes)
	for i := range view.Attributes {
		definition := &view.Attributes[i].Definition
		definition.Label = ""
		definition.Description = nil
		definition.DisplayOrder = 0
		definition.Revision = 0
		definition.CreatedAt = time.Time{}
		definition.UpdatedAt = time.Time{}
	}
	view.RelationshipTypes = slices.Clone(view.RelationshipTypes)
	for i := range view.RelationshipTypes {
		relationshipType := &view.RelationshipTypes[i]
		relationshipType.Revision = 0
		relationshipType.CreatedAt = time.Time{}
		relationshipType.UpdatedAt = time.Time{}
	}
	return view
}

// vcardFingerprintPerson keeps the person record fields a card projects:
// its identity and display name. ParticipantIDs are the binding set, which a
// participant link changes without touching the card.
func vcardFingerprintPerson(person Person) Person {
	return Person{ID: person.ID, VCardUID: person.VCardUID, DisplayName: person.DisplayName}
}
