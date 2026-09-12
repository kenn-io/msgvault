package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/vcard"
)

// CardDAVInferenceExportState records the revision of inferred information
// that can change a person's published CardDAV representation. A missing row
// is the zero state.
type CardDAVInferenceExportState struct {
	PersonID                     int64
	InferenceRevision            int64
	ApprovedRevision             int64
	ApprovedConnectionGeneration *int64
	ApprovedAddressBookID        *int64
}

type CardDAVReviewArtifactKind string

const (
	CardDAVReviewCurrent  CardDAVReviewArtifactKind = "current"
	CardDAVReviewPending  CardDAVReviewArtifactKind = "pending"
	CardDAVReviewConflict CardDAVReviewArtifactKind = "conflict"
)

type CardDAVReviewArtifactFence struct {
	Kind                 CardDAVReviewArtifactKind `json:"kind"`
	PersonID             int64                     `json:"person_id"`
	BodySHA256           string                    `json:"body_sha256"`
	PersonFingerprint    string                    `json:"person_fingerprint"`
	InferenceRevision    int64                     `json:"inference_revision"`
	ConnectionGeneration int64                     `json:"connection_generation"`
	AddressBookID        int64                     `json:"address_book_id"`
	BookSyncRevision     int64                     `json:"book_sync_revision"`
	Href                 string                    `json:"href"`
	MappingRevision      *int64                    `json:"mapping_revision"`
	RemoteETag           *string                   `json:"remote_etag"`
	MutationRevision     *int64                    `json:"mutation_revision"`
	ConflictID           *int64                    `json:"conflict_id"`
	ConflictRevision     *int64                    `json:"conflict_revision"`
}

type CardDAVPublicationReviewSource struct {
	Person               Person
	Snapshot             *PersonVCardSnapshot
	Inference            CardDAVInferenceExportState
	ConnectionGeneration int64
	Book                 CardDAVAddressBook
	Publication          *CardDAVPublication
	Resource             *CardDAVResource
	Envelope             *VCardResourceEnvelopeRecord
	Conflict             *CardDAVConflict
}

// personInferenceExportProjection deliberately omits media bytes and every
// non-rendering input. It keeps the source classification so a declared value
// becoming inferred invalidates review even when its textual value is equal.
type personInferenceExportProjection struct {
	Attributes  []personInferenceExportAttribute  `json:"attributes"`
	Employments []personInferenceExportEmployment `json:"employments"`
}

type personInferenceExportAttribute struct {
	RowID    int64              `json:"-"`
	Slot     string             `json:"-"`
	Property string             `json:"property"`
	Type     AttributeValueType `json:"type"`
	Value    string             `json:"value"`
	Inferred bool               `json:"inferred"`
}

type personInferenceExportEmployment struct {
	RowID        int64    `json:"-"`
	Organization []string `json:"organization"`
	Title        string   `json:"title"`
	Role         string   `json:"role"`
	Inferred     bool     `json:"inferred"`
}

func (s *Store) loadPersonInferenceExportProjectionTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (personInferenceExportProjection, error) {
	attributes, err := s.loadPersonVCardAttributesTx(ctx, tx, personID)
	if err != nil {
		return personInferenceExportProjection{}, err
	}
	projection := personInferenceExportProjection{
		Attributes:  make([]personInferenceExportAttribute, 0),
		Employments: make([]personInferenceExportEmployment, 0),
	}
	for _, attribute := range attributes {
		if attribute.Definition.VCardProperty == nil {
			continue
		}
		for _, value := range attribute.Values {
			if value.Value.Type == AttributeValueJSON || value.Value.Type == AttributeValueRecordReference || vcard.IsReservedProperty(*attribute.Definition.VCardProperty) {
				continue
			}
			canonical, err := value.Value.CanonicalString()
			if err != nil {
				return personInferenceExportProjection{}, err
			}
			if value.Value.Type == AttributeValueTimestamp {
				canonical = value.Value.Timestamp.UTC().Format("20060102T150405Z")
			}
			projection.Attributes = append(projection.Attributes, personInferenceExportAttribute{
				RowID:    value.ID,
				Slot:     fmt.Sprintf("%d/%d", attribute.Definition.ID, value.Ordinal),
				Property: strings.ToUpper(strings.TrimSpace(*attribute.Definition.VCardProperty)),
				Type:     value.Value.Type,
				Value:    canonical,
				Inferred: provenanceIsInferred(value.Source),
			})
		}
	}
	employments, err := s.listAllEmploymentsContext(ctx, tx, EmploymentFilter{PersonID: personID})
	if err != nil {
		return personInferenceExportProjection{}, err
	}
	for _, employment := range employments {
		if !employment.IsCurrent || !employment.IsPrimary {
			continue
		}
		organization, err := getOrganizationTx(ctx, tx, employment.OrganizationID)
		if err != nil {
			return personInferenceExportProjection{}, err
		}
		contributor := personInferenceExportEmployment{
			RowID:        employment.ID,
			Organization: vcard.OrganizationComponents(organization.Name, inferenceExportEmploymentText(employment.Department)),
			Title:        inferenceExportEmploymentText(employment.Title),
			Role:         inferenceExportEmploymentText(employment.Role),
			Inferred:     provenanceIsInferred(employment.Source),
		}
		if len(contributor.Organization) != 0 || contributor.Title != "" || contributor.Role != "" {
			projection.Employments = append(projection.Employments, contributor)
		}
	}
	return projection, nil
}

func inferenceExportEmploymentText(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func provenanceIsInferred(source Provenance) bool {
	return source == ProvenanceExtraction || source == ProvenanceEnrichment
}

func inferenceExportProjectionChanged(
	before, after personInferenceExportProjection,
) (bool, error) {
	// Comparing a multiset ignores row identity, ordinal gaps and catalog
	// ordering while retaining duplicate property occurrences. Declared
	// contributors cannot independently create inference debt.
	encode := func(projection personInferenceExportProjection) ([]string, error) {
		projection = inferredExportContributors(projection)
		values := make([]string, 0, len(projection.Attributes)+len(projection.Employments))
		for _, attribute := range projection.Attributes {
			encoded, err := json.Marshal(attribute)
			if err != nil {
				return nil, err
			}
			values = append(values, "attribute:"+string(encoded))
		}
		for _, employment := range projection.Employments {
			encoded, err := json.Marshal(employment)
			if err != nil {
				return nil, err
			}
			values = append(values, "employment:"+string(encoded))
		}
		slices.Sort(values)
		return values, nil
	}
	left, err := encode(before)
	if err != nil {
		return false, fmt.Errorf("encode prior inference export projection: %w", err)
	}
	right, err := encode(after)
	if err != nil {
		return false, fmt.Errorf("encode updated inference export projection: %w", err)
	}
	return !slices.Equal(left, right), nil
}

func inferenceExportProjectionHasInferred(projection personInferenceExportProjection) bool {
	for _, attribute := range projection.Attributes {
		if attribute.Inferred {
			return true
		}
	}
	for _, employment := range projection.Employments {
		if employment.Inferred {
			return true
		}
	}
	return false
}

func (s *Store) backfillCardDAVInferenceExportState(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM persons ORDER BY id`)
		if err != nil {
			return fmt.Errorf("list people for CardDAV inference export backfill: %w", err)
		}
		defer func() { _ = rows.Close() }()
		personIDs := make([]int64, 0)
		for rows.Next() {
			var personID int64
			if err := rows.Scan(&personID); err != nil {
				return fmt.Errorf("scan CardDAV inference export backfill person: %w", err)
			}
			personIDs = append(personIDs, personID)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate CardDAV inference export backfill people: %w", err)
		}
		for _, personID := range personIDs {
			projection, err := s.loadPersonInferenceExportProjectionTx(ctx, tx, personID)
			if err != nil {
				return fmt.Errorf("load CardDAV inference export backfill projection: %w", err)
			}
			needsReview := inferenceExportProjectionHasInferred(projection)
			if !needsReview {
				needsReview, err = s.personHasPublishedInferenceHistoryTx(ctx, tx, personID)
				if err != nil {
					return err
				}
			}
			if needsReview {
				if _, err := tx.ExecContext(ctx, `INSERT INTO person_carddav_inference_state
                    (person_id, inference_revision) VALUES (?, 1)
                    ON CONFLICT(person_id) DO NOTHING`, personID); err != nil {
					return fmt.Errorf("initialize CardDAV inference export state: %w", err)
				}
			}
		}
		return nil
	})
}

// personHasPublishedInferenceHistoryTx conservatively recognizes prior inferred
// contributors only for an existing desired or pending publication. Applied
// projection references survive physical deletion; claims alone prove nothing.
func (s *Store) personHasPublishedInferenceHistoryTx(ctx context.Context, tx *loggedTx, personID int64) (bool, error) {
	var published bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM carddav_publications
    WHERE person_id = ? AND (desired = TRUE OR pending_operation IS NOT NULL))`, personID).Scan(&published); err != nil {
		return false, fmt.Errorf("check historical inference publication: %w", err)
	}
	if !published {
		return false, nil
	}
	var employmentHistory bool
	if err := tx.QueryRowContext(ctx, `SELECT
    EXISTS(SELECT 1 FROM employments WHERE person_id = ? AND source IN ('extraction', 'enrichment'))
    OR EXISTS(SELECT 1 FROM person_fact_decisions d
       WHERE d.person_id = ? AND d.action = 'applied' AND d.projection_kind = 'employment'
         AND d.projection_row_id IS NOT NULL AND EXISTS(SELECT 1 FROM person_fact_claims c
           WHERE c.id = d.claim_id AND c.origin IN ('extraction', 'enrichment', 'brief')))`, personID, personID).Scan(&employmentHistory); err != nil {
		return false, fmt.Errorf("check inferred employment history: %w", err)
	}
	if employmentHistory {
		return true, nil
	}
	// Retired attribute values keep their provenance. Include applied references
	// as well, since native mappings can outlive the referenced projection row.
	rows, err := tx.QueryContext(ctx, `SELECT v.id, d.vcard_property,
    v.value_json IS NULL AND v.value_record_id IS NULL
    FROM person_attribute_values v JOIN attribute_definitions d ON d.id = v.definition_id
    WHERE v.person_id = ? AND v.source IN ('extraction', 'enrichment')
    UNION ALL SELECT d.projection_row_id, NULL, FALSE FROM person_fact_decisions d
    WHERE d.person_id = ? AND d.action = 'applied' AND d.projection_kind = 'person_attribute'
      AND d.projection_row_id IS NOT NULL AND EXISTS(SELECT 1 FROM person_fact_claims c
        WHERE c.id = d.claim_id AND c.origin IN ('extraction', 'enrichment', 'brief'))`, personID, personID)
	if err != nil {
		return false, fmt.Errorf("load inferred attribute history: %w", err)
	}
	inferredRows := make(map[int64]bool)
	portable := false
	for rows.Next() {
		var id int64
		var property sql.NullString
		var supported bool
		if err := rows.Scan(&id, &property, &supported); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("scan inferred attribute history: %w", err)
		}
		inferredRows[id] = true
		if supported && property.Valid && strings.TrimSpace(property.String) != "" && !vcard.IsReservedProperty(property.String) {
			portable = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, fmt.Errorf("iterate inferred attribute history: %w", err)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if portable {
		return true, nil
	}
	if len(inferredRows) == 0 {
		return false, nil
	}
	// Select only mapping metadata: full envelopes may contain large media bodies
	// and property trees, which are irrelevant to migration attribution.
	mappingsExpr := "json_extract(resource_metadata, '$.native_mappings')"
	if s.IsPostgreSQL() {
		mappingsExpr = "resource_metadata ->> 'native_mappings'"
	}
	rows, err = tx.QueryContext(ctx, `SELECT `+mappingsExpr+` FROM vcard_resource_envelopes
    WHERE person_id = ? AND source_ref = 'carddav:' || CAST((SELECT address_book_id
       FROM carddav_publications WHERE person_id = ?) AS TEXT)`, personID, personID)
	if err != nil {
		return false, fmt.Errorf("load historical native mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return false, fmt.Errorf("scan historical native mappings: %w", err)
		}
		if !raw.Valid {
			continue
		}
		var mappings []vcard.NativeMapping
		if err := json.Unmarshal([]byte(raw.String), &mappings); err != nil {
			return false, fmt.Errorf("decode historical native mappings: %w", err)
		}
		for _, mapping := range mappings {
			if mapping.Table == "person_attribute_values" && inferredRows[mapping.RowID] {
				found = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate historical native mappings: %w", err)
	}
	return found, nil
}

// advancePersonInferenceExportRevisionTx serializes first invalidation with
// review approval on the person row, then creates or advances the sparse state
// row in the caller-owned transaction.
func (s *Store) advancePersonInferenceExportRevisionTx(
	ctx context.Context, tx *loggedTx, personID int64,
) error {
	var lockedID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM persons WHERE id = ?`+
		s.dialect.SelectForUpdate(), personID).Scan(&lockedID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPersonNotFound
		}
		return fmt.Errorf("lock person inference export state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO person_carddav_inference_state
		(person_id, inference_revision) VALUES (?, 1)
		ON CONFLICT(person_id) DO UPDATE SET
		inference_revision = person_carddav_inference_state.inference_revision + 1`, personID); err != nil {
		return fmt.Errorf("advance person inference export revision: %w", err)
	}
	return nil
}

func (s *Store) getCardDAVInferenceExportStateTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (CardDAVInferenceExportState, error) {
	state := CardDAVInferenceExportState{PersonID: personID}
	var generation, bookID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT inference_revision, approved_revision,
		approved_connection_generation, approved_address_book_id
		FROM person_carddav_inference_state WHERE person_id = ?`, personID).Scan(
		&state.InferenceRevision, &state.ApprovedRevision, &generation, &bookID)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return CardDAVInferenceExportState{}, fmt.Errorf("load person inference export state: %w", err)
	}
	state.ApprovedConnectionGeneration = cardDAVInferenceNullInt64Ptr(generation)
	state.ApprovedAddressBookID = cardDAVInferenceNullInt64Ptr(bookID)
	return state, nil
}

func cardDAVInferenceNullInt64Ptr(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func (s *Store) clearCardDAVInferenceApprovalsForBooksTx(
	ctx context.Context, tx *loggedTx, bookIDs ...int64,
) error {
	placeholders, args := sortedIDPlaceholders(bookIDs)
	if placeholders == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_carddav_inference_state
		SET approved_revision = 0, approved_connection_generation = NULL,
		approved_address_book_id = NULL
		WHERE approved_address_book_id IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("clear CardDAV inference approvals for removed books: %w", err)
	}
	return nil
}

// LoadCardDAVPublicationReviewSourceContext reads all rendering inputs and
// publication fences from one consistent database snapshot.
func (s *Store) LoadCardDAVPublicationReviewSourceContext(
	ctx context.Context, personID int64,
) (*CardDAVPublicationReviewSource, error) {
	var source *CardDAVPublicationReviewSource
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		source, err = s.loadCardDAVPublicationReviewSourceTx(ctx, tx, personID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return source, nil
}

func (s *Store) loadCardDAVPublicationReviewSourceTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (*CardDAVPublicationReviewSource, error) {
	snapshot, err := s.loadPersonVCardSnapshotTx(ctx, tx, personID)
	if err != nil {
		return nil, err
	}
	inference, err := s.getCardDAVInferenceExportStateTx(ctx, tx, personID)
	if err != nil {
		return nil, err
	}
	source := &CardDAVPublicationReviewSource{Person: snapshot.Profile.Person, Snapshot: snapshot, Inference: inference}
	publication, err := getCardDAVPublicationFrom(ctx, tx, personID, "")
	if err != nil && !errors.Is(err, ErrCardDAVPublicationNotFound) {
		return nil, err
	}
	source.Publication = publication
	account, err := getCardDAVAccountFrom(ctx, tx.Tx, s.Rebind)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, ErrCardDAVNoWriteTarget
	}
	source.ConnectionGeneration = account.ConnectionGeneration
	books, err := listCardDAVBooksFrom(ctx, tx.Tx, s.Rebind)
	if err != nil {
		return nil, err
	}
	var capturedBookID int64
	if publication != nil && (publication.Desired || publication.PendingOperation != "") {
		capturedBookID = publication.AddressBookID
		if capturedBookID == 0 {
			return nil, ErrCardDAVAddressBookNotFound
		}
	}
	for _, book := range books {
		if (capturedBookID != 0 && book.ID == capturedBookID) ||
			(capturedBookID == 0 && book.IsWriteTarget && book.IsSubscribed) {
			source.Book = book
			break
		}
	}
	if source.Book.ID == 0 {
		if capturedBookID != 0 {
			return nil, ErrCardDAVAddressBookNotFound
		}
		return nil, ErrCardDAVNoWriteTarget
	}
	resource, err := findCardDAVResourceForPersonTx(ctx, tx, source.Book.ID, personID, "")
	if err != nil && !errors.Is(err, ErrCardDAVResourceNotFound) {
		return nil, err
	}
	source.Resource = resource
	href := ""
	if resource != nil {
		href = resource.Href
		source.Envelope, err = s.findVCardResourceEnvelopeTx(ctx, tx, fmt.Sprintf("carddav:%d", source.Book.ID), href)
		if err != nil {
			return nil, err
		}
	} else if publication != nil {
		href = publication.Href
	}
	if href != "" {
		conflict, err := scanCardDAVConflict(tx.QueryRowContext(ctx,
			`SELECT `+cardDAVConflictColumns+` FROM carddav_conflicts
    WHERE address_book_id = ? AND href = ? AND status = 'unresolved'`, source.Book.ID, href))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("load CardDAV review conflict: %w", err)
		}
		if err == nil {
			source.Conflict = conflict
		}
	}
	if (publication == nil || publication.PendingOperation == "") &&
		(!source.Book.IsSubscribed || (!source.Book.IsWriteTarget && source.Conflict == nil)) {
		return nil, ErrCardDAVNoWriteTarget
	}
	return source, nil
}

var (
	ErrCardDAVInferenceReviewRequired = errors.New("CardDAV inference export review is required")
	ErrCardDAVReviewStale             = errors.New("CardDAV publication review is stale")
)

// ReviewRequired reports whether this account and book have approval for the
// current inference revision. A zero revision needs no approval scope.
func (state CardDAVInferenceExportState) ReviewRequired(generation, bookID int64) bool {
	return state.InferenceRevision > 0 && (state.ApprovedRevision != state.InferenceRevision ||
		state.ApprovedConnectionGeneration == nil || *state.ApprovedConnectionGeneration != generation ||
		state.ApprovedAddressBookID == nil || *state.ApprovedAddressBookID != bookID)
}

// CardDAVReviewedPublicationPlan binds approval and preparation in one transaction.
type CardDAVReviewedPublicationPlan struct {
	Publication   CardDAVPublicationPlan
	Fence         CardDAVReviewArtifactFence
	ApprovalToken string
}

func CardDAVBodySHA256(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// CardDAVReviewToken uses a versioned struct encoding, including explicit null
// optional fences. It is separate from the persisted native vCard fingerprint.
func CardDAVReviewToken(fence CardDAVReviewArtifactFence) string {
	body, _ := json.Marshal(struct { //nolint:errchkjson // fixed struct of scalars and pointers cannot fail to encode
		Version int                        `json:"version"`
		Fence   CardDAVReviewArtifactFence `json:"fence"`
	}{Version: 1, Fence: fence})
	return CardDAVBodySHA256(body)
}

func CardDAVCurrentReviewFence(source *CardDAVPublicationReviewSource, body []byte, href string) CardDAVReviewArtifactFence {
	fence := CardDAVReviewArtifactFence{
		Kind: CardDAVReviewCurrent, PersonID: source.Person.ID, BodySHA256: CardDAVBodySHA256(body),
		PersonFingerprint: source.Snapshot.Fingerprint, InferenceRevision: source.Inference.InferenceRevision,
		ConnectionGeneration: source.ConnectionGeneration, AddressBookID: source.Book.ID,
		BookSyncRevision: source.Book.SyncRevision, Href: href,
	}
	if source.Resource != nil {
		revision, etag := source.Resource.MappingRevision, source.Resource.RemoteETag
		fence.MappingRevision, fence.RemoteETag = &revision, &etag
		fence.Href = source.Resource.Href
	}
	if source.Publication != nil {
		revision := source.Publication.MutationRevision
		fence.MutationRevision = &revision
	}
	return fence
}

func (s *Store) PrepareReviewedCardDAVPublicationContext(ctx context.Context, plan CardDAVReviewedPublicationPlan) (*CardDAVPublication, error) {
	if !plan.Publication.Desired || plan.Publication.PersonID <= 0 || plan.Publication.AddressBookID <= 0 ||
		plan.Publication.Href == "" || len(plan.Publication.OutgoingBody) == 0 || plan.Publication.OutgoingSemanticHash == "" ||
		plan.Publication.LocalHash == "" || plan.ApprovalToken == "" {
		return nil, ErrCardDAVInvalidPlan
	}
	return s.prepareCardDAVPublicationContext(ctx, plan.Publication, &plan)
}

func (s *Store) approveCardDAVInferenceTx(ctx context.Context, tx *loggedTx, state CardDAVInferenceExportState, generation, bookID int64) error {
	// Keep the all-zero state sparse. The caller already holds the person lock,
	// which also serializes approval with the first inference-state insertion.
	if state.InferenceRevision == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE person_carddav_inference_state SET approved_revision = inference_revision,
  approved_connection_generation = ?, approved_address_book_id = ? WHERE person_id = ?`, generation, bookID, state.PersonID)
	return err
}

func (s *Store) clearPersonCardDAVInferenceApprovalTx(ctx context.Context, tx *loggedTx, personID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE person_carddav_inference_state SET approved_revision = 0,
  approved_connection_generation = NULL, approved_address_book_id = NULL WHERE person_id = ?`, personID)
	return err
}

func (s *Store) loadCardDAVPublicationReviewRequiredTx(ctx context.Context, tx *loggedTx, source *CardDAVPublicationStateSource) error {
	state, err := s.getCardDAVInferenceExportStateTx(ctx, tx, source.PersonID)
	if err != nil {
		return err
	}
	account, err := getCardDAVAccountFrom(ctx, tx.Tx, s.Rebind)
	if err != nil {
		return err
	}
	generation := int64(0)
	if account != nil {
		generation = account.ConnectionGeneration
	}
	bookID := source.AddressBookID
	if !source.HasPublication {
		bookID = source.ProspectiveBookID
	}
	source.InferenceReviewRequired = state.ReviewRequired(generation, bookID)
	if source.PendingOperation == CardDAVMutationCreate {
		pending, err := getCardDAVPublicationFrom(ctx, tx, source.PersonID, "")
		if err != nil {
			return err
		}
		source.InferenceReviewRequired = source.InferenceReviewRequired || !pending.HasExactBodyApproval()
	}
	if source.ConflictID != 0 && state.InferenceRevision > 0 {
		conflict, err := getCardDAVConflictFrom(ctx, tx, source.ConflictID, "")
		if err != nil {
			return err
		}
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT sync_revision FROM carddav_address_books WHERE id=?`, bookID).Scan(&revision); err != nil {
			return err
		}
		source.InferenceReviewRequired = source.InferenceReviewRequired || !conflict.HasExactLocalApproval(state, generation, revision)
	}
	return nil
}

func nullableCardDAVMetadata(metadata []byte) any {
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

// lockCardDAVPublicationOperationTx gives publication completion and recovery
// the same account/book/person/publication order as preparation.
func (s *Store) lockCardDAVPublicationOperationTx(ctx context.Context, tx *loggedTx, personID, bookID int64) (*CardDAVPublication, error) {
	if err := s.lockCardDAVPublicationTargetTx(ctx, tx, bookID); err != nil {
		return nil, err
	}
	if s.cardDAVReviewPersonLockHook != nil {
		s.cardDAVReviewPersonLockHook()
	}
	if err := s.lockPersonVCardProjectionTx(ctx, tx, personID, ""); err != nil {
		return nil, err
	}
	current, err := getCardDAVPublicationFrom(ctx, tx, personID, s.dialect.SelectForUpdate())
	if err != nil {
		return nil, err
	}
	if current.AddressBookID != bookID {
		return nil, ErrCardDAVStalePlan
	}
	return current, nil
}

func validateCardDAVPublicationMetadata(body, metadata []byte, sourceRef, href, uid string) (vcard.ResourceEnvelope, error) {
	envelope, err := vcard.UnmarshalResourceMetadata(metadata)
	if err != nil {
		return vcard.ResourceEnvelope{}, ErrCardDAVInvalidPlan
	}
	envelope.SourceRef, envelope.SourceResourceUID, envelope.Href, envelope.CanonicalPersonUID = sourceRef, href, href, uid
	envelope.OriginalRawBytes, envelope.StoredBody = body, body
	prepared, err := prepareVCardEnvelope(envelope)
	if err != nil {
		return vcard.ResourceEnvelope{}, fmt.Errorf("%w: invalid publication ownership metadata", ErrCardDAVInvalidPlan)
	}
	return prepared, nil
}

func (s *Store) putCardDAVPublicationEnvelopeTx(ctx context.Context, tx *loggedTx, bookID, personID int64, href string, outgoingBody, metadata, canonicalBody []byte) error {
	uid, err := vcardCanonicalUIDTx(ctx, tx, personID)
	if err != nil {
		return err
	}
	sourceRef := fmt.Sprintf("carddav:%d", bookID)
	prepared, err := validateCardDAVPublicationMetadata(outgoingBody, metadata, sourceRef, href, uid)
	if err != nil {
		return err
	}
	canonical, err := vcard.ParseResourceEnvelope(canonicalBody)
	if err != nil {
		return err
	}
	canonical.SourceRef, canonical.SourceResourceUID, canonical.Href, canonical.CanonicalPersonUID = sourceRef, href, href, uid
	rebound, err := vcard.RebindResourceOwnership(prepared, canonical, true)
	if errors.Is(err, vcard.ErrResourceOwnershipMismatch) {
		return ErrCardDAVPublicationMismatch
	}
	if err != nil {
		return err
	}
	return s.putCardDAVPreparedEnvelopeTx(ctx, tx, bookID, personID, href, rebound)
}

func (s *Store) lockCardDAVPublicationTargetTx(ctx context.Context, tx *loggedTx, bookID int64) error {
	if lock := s.dialect.RowWriterLockSQL("carddav_accounts", "connection_generation"); lock != "" {
		if _, err := tx.ExecContext(ctx, lock, 1); err != nil {
			return err
		}
	}
	var locked int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM carddav_accounts WHERE id = 1`+s.dialect.SelectForUpdate()).Scan(&locked); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM carddav_address_books WHERE id = ?`+s.dialect.SelectForUpdate(), bookID).Scan(&locked); err != nil {
		return err
	}
	return nil
}
