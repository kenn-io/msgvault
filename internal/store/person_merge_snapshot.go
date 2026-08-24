package store

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const personMergeSnapshotVersion = 1

const (
	identityMatchCandidatesTableName       = "identity_match_candidates"
	identityMatchCandidateSourcesTableName = "identity_match_candidate_sources"
	identityMatchEvidenceTableName         = "identity_match_evidence"
	identityMatchEvidenceSourcesTableName  = "identity_match_evidence_sources"
	personMergeActionDeduplicated          = "deduplicated"
	personAttributeValuesTableName         = "person_attribute_values"
	personContactPointsTableName           = "person_contact_points"
	personMergeReviewCandidatesTableName   = "person_merge_review_candidates"
	personRelationshipsTableName           = "person_relationships"
	personRelationshipReviewsTableName     = "person_relationship_reviews"
	sourceIDColumnName                     = "source_id"
)

var ErrPersonMergeSnapshotCorrupt = errors.New("person merge snapshot is corrupt")

type personMergeOriginSide string

const (
	personMergeOriginSurvivor personMergeOriginSide = "survivor"
	personMergeOriginAbsorbed personMergeOriginSide = "absorbed"
)

type personMergeProvenanceKind string

const (
	personMergeProvenanceParticipantExact personMergeProvenanceKind = "participant_exact"
	personMergeProvenanceAbsorbedProfile  personMergeProvenanceKind = "absorbed_profile"
	personMergeProvenanceDerived          personMergeProvenanceKind = "derived"
	personMergeProvenanceInboundReference personMergeProvenanceKind = "inbound_reference"
)

type personMergeSnapshotValueKind string

const (
	personMergeSnapshotNull    personMergeSnapshotValueKind = "null"
	personMergeSnapshotInteger personMergeSnapshotValueKind = "integer"
	personMergeSnapshotReal    personMergeSnapshotValueKind = "real"
	personMergeSnapshotBoolean personMergeSnapshotValueKind = "boolean"
	personMergeSnapshotText    personMergeSnapshotValueKind = "text"
	personMergeSnapshotBytes   personMergeSnapshotValueKind = "bytes"
)

// personMergeSnapshotValue is a portable, typed SQL value. A compile-time
// table registry chooses each column's kind, keeping SQLite and PostgreSQL
// driver representations from changing canonical JSON.
type personMergeSnapshotValue struct {
	Kind    personMergeSnapshotValueKind `json:"kind"`
	Integer *int64                       `json:"integer,omitempty"`
	Real    *float64                     `json:"real,omitempty"`
	Boolean *bool                        `json:"boolean,omitempty"`
	Text    *string                      `json:"text,omitempty"`
	Bytes   []byte                       `json:"bytes,omitempty"`
}

type personMergeSnapshotColumn struct {
	Name  string                   `json:"name"`
	Value personMergeSnapshotValue `json:"value"`
}

type personMergeSnapshotPerson struct {
	ID                      int64   `json:"id"`
	VCardUID                string  `json:"vcard_uid"`
	DisplayName             *string `json:"display_name,omitempty"`
	Revision                int64   `json:"revision"`
	VCardProjectionRevision int64   `json:"vcard_projection_revision"`
	CreatedAt               string  `json:"created_at"`
	UpdatedAt               string  `json:"updated_at"`
	ParticipantIDs          []int64 `json:"participant_ids"`
}

type personMergeSnapshotRow struct {
	TableName      string                      `json:"table_name"`
	RowID          int64                       `json:"row_id"`
	RowKey         string                      `json:"row_key,omitempty"`
	OriginSide     personMergeOriginSide       `json:"origin_side"`
	ProvenanceKind personMergeProvenanceKind   `json:"provenance_kind"`
	ParticipantID  *int64                      `json:"participant_id,omitempty"`
	Columns        []personMergeSnapshotColumn `json:"columns"`
}

type personMergeSnapshot struct {
	Version int                         `json:"version"`
	Persons []personMergeSnapshotPerson `json:"persons"`
	Rows    []personMergeSnapshotRow    `json:"rows"`
}

type personMergeReferenceKind string

const (
	personMergeReferenceDirect      personMergeReferenceKind = "direct"
	personMergeReferencePolymorphic personMergeReferenceKind = "polymorphic"
)

type personMergeReference struct {
	Kind       personMergeReferenceKind
	IDColumn   string
	KindColumn string
	KindValue  string
}

type personMergeTableSpec struct {
	TableName        string
	KeyColumn        string
	KeyColumns       []string
	PersonReferences []personMergeReference
	Snapshot         bool
}

func (s personMergeTableSpec) keyColumns() []string {
	if len(s.KeyColumns) > 0 {
		return s.KeyColumns
	}
	if s.KeyColumn == "" {
		return nil
	}
	return []string{s.KeyColumn}
}

func directPersonReference(column string) personMergeReference {
	return personMergeReference{Kind: personMergeReferenceDirect, IDColumn: column}
}

func polymorphicPersonReference(idColumn, kindColumn string) personMergeReference {
	return personMergeReference{
		Kind: personMergeReferencePolymorphic, IDColumn: idColumn,
		KindColumn: kindColumn, KindValue: string(AttributeObjectPerson),
	}
}

const personMergePersonIDColumn = "person_id"

// personMergeTableRegistry is the closed allowlist for every table that can
// point at a person. Operation tables are classified but not recursively
// snapshotted; their targeted lineage updates are journaled separately.
var personMergeTableRegistry = map[string]personMergeTableSpec{
	"person_participants": {
		TableName: "person_participants", KeyColumn: "participant_id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_tracking": {
		TableName: "person_tracking", KeyColumn: "person_id", Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"vcard_resource_envelopes": {
		TableName: "vcard_resource_envelopes", KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"carddav_resources": {
		TableName: "carddav_resources", KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"carddav_publications": {
		TableName: "carddav_publications", KeyColumn: "person_id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_uid_aliases": {
		TableName: "person_uid_aliases", KeyColumn: "retired_uid", Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("surviving_person_id")},
	},
	personRelationshipsTableName: {
		TableName: personRelationshipsTableName, KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{
			directPersonReference("source_person_id"),
			directPersonReference("target_person_id"),
		},
	},
	personRelationshipReviewsTableName: {
		TableName: personRelationshipReviewsTableName, KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{
			directPersonReference("person_id"),
			directPersonReference("matched_person_id"),
		},
	},
	personAttributeValuesTableName: {
		TableName: personAttributeValuesTableName, KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{
			directPersonReference("person_id"),
			polymorphicPersonReference("value_record_id", "value_record_type"),
		},
	},
	"organization_attribute_values": {
		TableName: "organization_attribute_values", KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{
			polymorphicPersonReference("value_record_id", "value_record_type"),
		},
	},
	"person_merges": {
		TableName: "person_merges", KeyColumn: "id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference("current_person_id")},
	},
	personMergeReviewCandidatesTableName: {
		TableName: personMergeReviewCandidatesTableName, KeyColumn: "id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference("survivor_person_id")},
	},
	// Sweep rows are derived scheduling, cursor, and attempt history. They stay
	// scoped to the person that produced them and cascade with an absorbed root
	// instead of becoming survivor profile state.
	"person_sweep_changes": {
		TableName: "person_sweep_changes", KeyColumn: "sequence", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference(personMergePersonIDColumn)},
	},
	"person_sweep_work": {
		TableName: "person_sweep_work", KeyColumn: personMergePersonIDColumn, Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference(personMergePersonIDColumn)},
	},
	"person_sweep_cursors": {
		TableName: "person_sweep_cursors", KeyColumn: personMergePersonIDColumn, Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference(personMergePersonIDColumn)},
	},
	"person_sweep_attempts": {
		TableName: "person_sweep_attempts", KeyColumn: "id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference(personMergePersonIDColumn)},
	},
	// Enrichment rows are derived work, provider identity, audit, and accounting
	// state. They stay scoped to the person that produced them and cascade with
	// an absorbed root instead of becoming survivor profile state.
	"person_enrichment_work": {
		TableName: "person_enrichment_work", KeyColumn: personMergePersonIDColumn, Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference(personMergePersonIDColumn)},
	},
	"person_enrichment_attempts": {
		TableName: "person_enrichment_attempts", KeyColumn: "id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference(personMergePersonIDColumn)},
	},
	"person_enrichment_provider_identities": {
		TableName: "person_enrichment_provider_identities", KeyColumn: personMergePersonIDColumn, Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference(personMergePersonIDColumn)},
	},
	"person_enrichment_citations": {
		TableName: "person_enrichment_citations", KeyColumn: "id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference(personMergePersonIDColumn)},
	},
	"person_enrichment_person_day_counters": {
		TableName: "person_enrichment_person_day_counters", KeyColumn: personMergePersonIDColumn, Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference(personMergePersonIDColumn)},
	},
	// Fact-ledger history remains scoped to the person whose identity produced
	// it, so an absorbed person's ledger cascades with that root. Explicit pin
	// events are profile state, however, and must survive an exact split.
	"person_fact_generations": {
		TableName: "person_fact_generations", KeyColumn: "id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_fact_evidence": {
		TableName: "person_fact_evidence", KeyColumn: "id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_fact_claims": {
		TableName: "person_fact_claims", KeyColumn: "id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_fact_evidence_status_events": {
		TableName: "person_fact_evidence_status_events", KeyColumn: "id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_fact_resolutions": {
		TableName: "person_fact_resolutions", KeyColumn: "id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_fact_decisions": {
		TableName: "person_fact_decisions", KeyColumn: "id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_fact_pin_events": {
		TableName: "person_fact_pin_events", KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_names": {
		TableName: "person_names", KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	personContactPointsTableName: {
		TableName: personContactPointsTableName, KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_addresses": {
		TableName: "person_addresses", KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_dates": {
		TableName: "person_dates", KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_categories": {
		TableName: "person_categories", KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_media": {
		TableName: "person_media", KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	identityMatchCandidatesTableName: {
		TableName: identityMatchCandidatesTableName, KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{
			polymorphicPersonReference("left_id", "left_kind"),
			polymorphicPersonReference("right_id", "right_kind"),
		},
	},
	"identity_match_candidate_redirects": {
		TableName: "identity_match_candidate_redirects", KeyColumn: "retired_candidate_id",
	},
	identityMatchCandidateSourcesTableName: {
		TableName: identityMatchCandidateSourcesTableName, KeyColumn: "candidate_id",
		KeyColumns: []string{"candidate_id", sourceIDColumnName},
	},
	identityMatchEvidenceTableName: {
		TableName: identityMatchEvidenceTableName, KeyColumn: "id",
	},
	identityMatchEvidenceSourcesTableName: {
		TableName: identityMatchEvidenceSourcesTableName, KeyColumn: "evidence_id",
		KeyColumns: []string{"evidence_id", sourceIDColumnName},
	},
	"employments": {
		TableName: "employments", KeyColumn: "id", Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"activity_event_persons": {
		TableName: "activity_event_persons", KeyColumn: "message_id",
		KeyColumns: []string{"message_id", "person_id"}, Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"person_contact_state": {
		TableName: "person_contact_state", KeyColumn: "person_id", Snapshot: false,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
	"daily_note_entry_persons": {
		TableName: "daily_note_entry_persons", KeyColumn: "entry_id",
		KeyColumns: []string{"entry_id", "person_id"}, Snapshot: true,
		PersonReferences: []personMergeReference{directPersonReference("person_id")},
	},
}

func (s *Store) capturePersonMergeSnapshotTx(
	ctx context.Context, tx *loggedTx, survivorID, absorbedID int64,
) (personMergeSnapshot, error) {
	snapshot := personMergeSnapshot{
		Version: personMergeSnapshotVersion,
		Persons: make([]personMergeSnapshotPerson, 0, 2),
		Rows:    []personMergeSnapshotRow{},
	}
	for _, personID := range []int64{survivorID, absorbedID} {
		person, err := capturePersonMergeRootTx(ctx, tx, personID)
		if err != nil {
			return personMergeSnapshot{}, err
		}
		snapshot.Persons = append(snapshot.Persons, person)
	}

	tables := make([]string, 0, len(personMergeTableRegistry))
	for table, spec := range personMergeTableRegistry {
		if spec.Snapshot {
			tables = append(tables, table)
		}
	}
	sort.Strings(tables)
	for _, table := range tables {
		rows, err := s.capturePersonMergeTableTx(
			ctx, tx, personMergeTableRegistry[table], survivorID, absorbedID,
		)
		if err != nil {
			return personMergeSnapshot{}, err
		}
		snapshot.Rows = append(snapshot.Rows, rows...)
	}
	operationRows, err := s.capturePersonMergeOperationReferencesTx(ctx, tx, absorbedID)
	if err != nil {
		return personMergeSnapshot{}, err
	}
	snapshot.Rows = append(snapshot.Rows, operationRows...)
	dependentRows, err := s.capturePersonMergeIdentityDependentsTx(
		ctx, tx, snapshot.Rows, absorbedID,
	)
	if err != nil {
		return personMergeSnapshot{}, err
	}
	snapshot.Rows = append(snapshot.Rows, dependentRows...)
	relationshipDependents, err := s.capturePersonMergeRelationshipDependentsTx(
		ctx, tx, snapshot.Rows, absorbedID,
	)
	if err != nil {
		return personMergeSnapshot{}, err
	}
	snapshot.Rows = appendPersonMergeSnapshotRowsUnique(snapshot.Rows, relationshipDependents...)
	sort.SliceStable(snapshot.Rows, func(i, j int) bool {
		if snapshot.Rows[i].TableName != snapshot.Rows[j].TableName {
			return snapshot.Rows[i].TableName < snapshot.Rows[j].TableName
		}
		if snapshot.Rows[i].RowID != snapshot.Rows[j].RowID {
			return snapshot.Rows[i].RowID < snapshot.Rows[j].RowID
		}
		return snapshot.Rows[i].RowKey < snapshot.Rows[j].RowKey
	})
	return snapshot, nil
}

func (s *Store) capturePersonMergeRelationshipDependentsTx(
	ctx context.Context,
	tx *loggedTx,
	primaryRows []personMergeSnapshotRow,
	absorbedID int64,
) ([]personMergeSnapshotRow, error) {
	relationshipOrigins := make(map[int64]personMergeOriginSide)
	for _, row := range primaryRows {
		if row.TableName == personRelationshipsTableName {
			relationshipOrigins[row.RowID] = row.OriginSide
		}
	}
	ids := sortedPersonMergeSnapshotIDs(relationshipOrigins)
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.capturePersonMergeQueryTx(ctx, tx,
		personMergeTableRegistry[personRelationshipReviewsTableName],
		`SELECT * FROM person_relationship_reviews
		 WHERE accepted_relationship_id IN (`+personMergeSnapshotPlaceholders(len(ids))+`)
		 ORDER BY id`, personMergeSnapshotIDArgs(ids), absorbedID)
	if err != nil {
		return nil, err
	}
	setDependentSnapshotOrigins(rows, relationshipOrigins, "accepted_relationship_id")
	return rows, nil
}

func appendPersonMergeSnapshotRowsUnique(
	rows []personMergeSnapshotRow, extra ...personMergeSnapshotRow,
) []personMergeSnapshotRow {
	seen := make(map[string]struct{}, len(rows)+len(extra))
	for _, row := range rows {
		seen[row.TableName+"\x00"+row.RowKey] = struct{}{}
	}
	for _, row := range extra {
		key := row.TableName + "\x00" + row.RowKey
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		rows = append(rows, row)
	}
	return rows
}

func (s *Store) capturePersonMergeOperationReferencesTx(
	ctx context.Context, tx *loggedTx, absorbedID int64,
) ([]personMergeSnapshotRow, error) {
	queries := []struct {
		table string
		query string
	}{
		{
			table: "person_merges",
			query: `SELECT id, current_person_id FROM person_merges
				WHERE current_person_id = ? ORDER BY id`,
		},
		{
			table: personMergeReviewCandidatesTableName,
			query: `SELECT id, survivor_person_id, state, reviewed_at
				FROM person_merge_review_candidates
				WHERE survivor_person_id = ? ORDER BY id`,
		},
	}
	result := []personMergeSnapshotRow{}
	for _, item := range queries {
		rows, err := s.capturePersonMergeQueryTx(
			ctx, tx, personMergeTableRegistry[item.table], item.query, []any{absorbedID}, absorbedID,
		)
		if err != nil {
			return nil, err
		}
		result = append(result, rows...)
	}
	return result, nil
}

func capturePersonMergeRootTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (personMergeSnapshotPerson, error) {
	var (
		person      personMergeSnapshotPerson
		displayName sql.NullString
		createdAt   any
		updatedAt   any
	)
	err := tx.QueryRowContext(ctx, `SELECT
		id, vcard_uid, display_name, revision, vcard_projection_revision,
		created_at, updated_at
		FROM persons WHERE id = ?`, personID).Scan(
		&person.ID, &person.VCardUID, &displayName, &person.Revision,
		&person.VCardProjectionRevision, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return personMergeSnapshotPerson{}, ErrPersonNotFound
	}
	if err != nil {
		return personMergeSnapshotPerson{}, fmt.Errorf("capture person %d root: %w", personID, err)
	}
	if displayName.Valid {
		person.DisplayName = &displayName.String
	}
	person.CreatedAt = personMergeSnapshotTextValue(createdAt)
	person.UpdatedAt = personMergeSnapshotTextValue(updatedAt)
	person.ParticipantIDs = []int64{}

	rows, err := tx.QueryContext(ctx, `SELECT participant_id
		FROM person_participants WHERE person_id = ? ORDER BY participant_id`, personID)
	if err != nil {
		return personMergeSnapshotPerson{}, fmt.Errorf("capture person %d bindings: %w", personID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var participantID int64
		if err := rows.Scan(&participantID); err != nil {
			return personMergeSnapshotPerson{}, fmt.Errorf("scan person %d binding: %w", personID, err)
		}
		person.ParticipantIDs = append(person.ParticipantIDs, participantID)
	}
	if err := rows.Err(); err != nil {
		return personMergeSnapshotPerson{}, fmt.Errorf("iterate person %d bindings: %w", personID, err)
	}
	return person, nil
}

func (s *Store) capturePersonMergeTableTx(
	ctx context.Context,
	tx *loggedTx,
	spec personMergeTableSpec,
	survivorID, absorbedID int64,
) ([]personMergeSnapshotRow, error) {
	predicates := make([]string, 0, len(spec.PersonReferences))
	args := make([]any, 0, len(spec.PersonReferences)*3)
	keyColumns := spec.keyColumns()
	if len(keyColumns) == 0 {
		return nil, fmt.Errorf("capture %s: no stable key columns", spec.TableName)
	}
	orderColumns := append([]string(nil), keyColumns...)
	for _, reference := range spec.PersonReferences {
		switch reference.Kind {
		case personMergeReferenceDirect:
			predicates = append(predicates,
				fmt.Sprintf("(%s = ? OR %s = ?)", reference.IDColumn, reference.IDColumn))
			args = append(args, survivorID, absorbedID)
		case personMergeReferencePolymorphic:
			predicates = append(predicates, fmt.Sprintf(
				"(%s = ? AND (%s = ? OR %s = ?))",
				reference.KindColumn, reference.IDColumn, reference.IDColumn))
			args = append(args, reference.KindValue, survivorID, absorbedID)
		default:
			return nil, fmt.Errorf("capture %s: unknown reference kind %q", spec.TableName, reference.Kind)
		}
		orderColumns = appendUniqueString(orderColumns, reference.IDColumn)
		if reference.KindColumn != "" {
			orderColumns = appendUniqueString(orderColumns, reference.KindColumn)
		}
	}
	if len(predicates) == 0 {
		return nil, fmt.Errorf("capture %s: no person reference", spec.TableName)
	}
	query := fmt.Sprintf("SELECT * FROM %s WHERE %s ORDER BY %s",
		spec.TableName, strings.Join(predicates, " OR "), strings.Join(orderColumns, ", "))
	return s.capturePersonMergeQueryTx(ctx, tx, spec, query, args, absorbedID)
}

func (s *Store) capturePersonMergeQueryTx(
	ctx context.Context,
	tx *loggedTx,
	spec personMergeTableSpec,
	query string,
	args []any,
	absorbedID int64,
) ([]personMergeSnapshotRow, error) {
	keyColumns := spec.keyColumns()
	if len(keyColumns) == 0 {
		return nil, fmt.Errorf("capture %s: no stable key columns", spec.TableName)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("capture %s rows: %w", spec.TableName, err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read %s snapshot columns: %w", spec.TableName, err)
	}
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, fmt.Errorf("read %s snapshot column types: %w", spec.TableName, err)
	}
	indexes := make(map[string]int, len(columns))
	for i, column := range columns {
		indexes[column] = i
	}

	result := []personMergeSnapshotRow{}
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("scan %s snapshot row: %w", spec.TableName, err)
		}
		row := personMergeSnapshotRow{
			TableName:      spec.TableName,
			OriginSide:     personMergeRowOrigin(values, indexes, spec.PersonReferences, absorbedID),
			ProvenanceKind: personMergeRowProvenance(spec.TableName, values, indexes, absorbedID),
			Columns:        make([]personMergeSnapshotColumn, 0, len(columns)),
		}
		for i, column := range columns {
			value, normalizeErr := normalizePersonMergeSnapshotValue(
				values[i], columnTypes[i].DatabaseTypeName(),
			)
			if normalizeErr != nil {
				return nil, fmt.Errorf("normalize %s.%s: %w", spec.TableName, column, normalizeErr)
			}
			row.Columns = append(row.Columns, personMergeSnapshotColumn{Name: column, Value: value})
		}
		if len(keyColumns) == 1 {
			if id, ok := personMergeSnapshotInt64(values[indexes[keyColumns[0]]]); ok {
				row.RowID = id
			}
		}
		row.RowKey, err = canonicalPersonMergeSnapshotRowKey(row.Columns, keyColumns)
		if err != nil {
			return nil, fmt.Errorf("key %s snapshot row: %w", spec.TableName, err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s snapshot rows: %w", spec.TableName, err)
	}
	return result, nil
}

func personMergeRowProvenance(
	table string, values []any, indexes map[string]int, absorbedID int64,
) personMergeProvenanceKind {
	if table == personAttributeValuesTableName {
		if index, ok := indexes["person_id"]; ok {
			if id, valid := personMergeSnapshotInt64(values[index]); valid && id == absorbedID {
				return personMergeProvenanceAbsorbedProfile
			}
		}
		return personMergeProvenanceInboundReference
	}
	return personMergeTableProvenance(table)
}

func canonicalPersonMergeSnapshotRowKey(
	columns []personMergeSnapshotColumn, keyColumns []string,
) (string, error) {
	byName := make(map[string]personMergeSnapshotValue, len(columns))
	for _, column := range columns {
		byName[column.Name] = column.Value
	}
	key := make([]personMergeSnapshotColumn, 0, len(keyColumns))
	for _, name := range keyColumns {
		value, ok := byName[name]
		if !ok {
			return "", fmt.Errorf("missing key column %q", name)
		}
		key = append(key, personMergeSnapshotColumn{Name: name, Value: value})
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		return "", fmt.Errorf("encode stable row key: %w", err)
	}
	return string(encoded), nil
}

func (s *Store) capturePersonMergeIdentityDependentsTx(
	ctx context.Context,
	tx *loggedTx,
	primaryRows []personMergeSnapshotRow,
	absorbedID int64,
) ([]personMergeSnapshotRow, error) {
	candidateOrigins := make(map[int64]personMergeOriginSide)
	for _, row := range primaryRows {
		if row.TableName == identityMatchCandidatesTableName {
			candidateOrigins[row.RowID] = row.OriginSide
		}
	}
	candidateIDs := sortedPersonMergeSnapshotIDs(candidateOrigins)
	if len(candidateIDs) == 0 {
		return nil, nil
	}
	candidateArgs := personMergeSnapshotIDArgs(candidateIDs)
	candidatePlaceholders := personMergeSnapshotPlaceholders(len(candidateIDs))

	result := []personMergeSnapshotRow{}
	redirectArgs := append(append([]any(nil), candidateArgs...), candidateArgs...)
	redirects, err := s.capturePersonMergeQueryTx(ctx, tx,
		personMergeTableRegistry["identity_match_candidate_redirects"],
		`SELECT * FROM identity_match_candidate_redirects
		 WHERE retired_candidate_id IN (`+candidatePlaceholders+`)
		    OR surviving_candidate_id IN (`+candidatePlaceholders+`)
		 ORDER BY retired_candidate_id`, redirectArgs, absorbedID)
	if err != nil {
		return nil, err
	}
	setDependentSnapshotOrigins(redirects, candidateOrigins,
		"surviving_candidate_id", "retired_candidate_id")
	result = append(result, redirects...)

	sources, err := s.capturePersonMergeQueryTx(ctx, tx,
		personMergeTableRegistry[identityMatchCandidateSourcesTableName],
		`SELECT * FROM identity_match_candidate_sources
		 WHERE candidate_id IN (`+candidatePlaceholders+`)
		 ORDER BY candidate_id, source_id`, candidateArgs, absorbedID)
	if err != nil {
		return nil, err
	}
	setDependentSnapshotOrigins(sources, candidateOrigins, "candidate_id")
	result = append(result, sources...)

	evidenceRows, err := s.capturePersonMergeQueryTx(ctx, tx,
		personMergeTableRegistry[identityMatchEvidenceTableName],
		`SELECT * FROM identity_match_evidence
		 WHERE candidate_id IN (`+candidatePlaceholders+`)
		 ORDER BY id`, candidateArgs, absorbedID)
	if err != nil {
		return nil, err
	}
	setDependentSnapshotOrigins(evidenceRows, candidateOrigins, "candidate_id")
	result = append(result, evidenceRows...)

	evidenceOrigins := make(map[int64]personMergeOriginSide, len(evidenceRows))
	for _, row := range evidenceRows {
		evidenceOrigins[row.RowID] = row.OriginSide
	}
	evidenceIDs := sortedPersonMergeSnapshotIDs(evidenceOrigins)
	if len(evidenceIDs) == 0 {
		return result, nil
	}
	evidenceArgs := personMergeSnapshotIDArgs(evidenceIDs)
	evidenceSources, err := s.capturePersonMergeQueryTx(ctx, tx,
		personMergeTableRegistry[identityMatchEvidenceSourcesTableName],
		`SELECT * FROM identity_match_evidence_sources
		 WHERE evidence_id IN (`+personMergeSnapshotPlaceholders(len(evidenceIDs))+`)
		 ORDER BY evidence_id, source_id`, evidenceArgs, absorbedID)
	if err != nil {
		return nil, err
	}
	setDependentSnapshotOrigins(evidenceSources, evidenceOrigins, "evidence_id")
	result = append(result, evidenceSources...)
	return result, nil
}

func setDependentSnapshotOrigins(
	rows []personMergeSnapshotRow,
	origins map[int64]personMergeOriginSide,
	columns ...string,
) {
	for i := range rows {
		for _, column := range columns {
			id, ok := personMergeSnapshotRowInteger(rows[i], column)
			if !ok {
				continue
			}
			if origin, exists := origins[id]; exists {
				rows[i].OriginSide = origin
				break
			}
		}
	}
}

func personMergeSnapshotRowInteger(row personMergeSnapshotRow, name string) (int64, bool) {
	for _, column := range row.Columns {
		if column.Name == name && column.Value.Integer != nil {
			return *column.Value.Integer, true
		}
	}
	return 0, false
}

func sortedPersonMergeSnapshotIDs(origins map[int64]personMergeOriginSide) []int64 {
	ids := make([]int64, 0, len(origins))
	for id := range origins {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func personMergeSnapshotIDArgs(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

func personMergeSnapshotPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func personMergeRowOrigin(
	values []any,
	indexes map[string]int,
	references []personMergeReference,
	absorbedID int64,
) personMergeOriginSide {
	for _, reference := range references {
		if reference.Kind == personMergeReferencePolymorphic &&
			personMergeSnapshotTextValue(values[indexes[reference.KindColumn]]) != reference.KindValue {
			continue
		}
		if id, ok := personMergeSnapshotInt64(values[indexes[reference.IDColumn]]); ok && id == absorbedID {
			return personMergeOriginAbsorbed
		}
	}
	return personMergeOriginSurvivor
}

func personMergeTableProvenance(table string) personMergeProvenanceKind {
	switch table {
	case "activity_event_persons", "person_contact_state":
		return personMergeProvenanceDerived
	case "organization_attribute_values", "person_uid_aliases", personRelationshipsTableName,
		personRelationshipReviewsTableName, identityMatchCandidatesTableName,
		"identity_match_candidate_redirects", identityMatchCandidateSourcesTableName,
		identityMatchEvidenceTableName, identityMatchEvidenceSourcesTableName, "person_merges",
		personMergeReviewCandidatesTableName, "daily_note_entry_persons":
		return personMergeProvenanceInboundReference
	default:
		return personMergeProvenanceAbsorbedProfile
	}
}

func normalizePersonMergeSnapshotValue(raw any, databaseType string) (personMergeSnapshotValue, error) {
	if raw == nil {
		return personMergeSnapshotValue{Kind: personMergeSnapshotNull}, nil
	}
	databaseType = strings.ToUpper(databaseType)
	switch {
	case strings.Contains(databaseType, "BOOL"):
		value, err := personMergeSnapshotBool(raw)
		return personMergeSnapshotValue{Kind: personMergeSnapshotBoolean, Boolean: &value}, err
	case strings.Contains(databaseType, "INT"):
		value, ok := personMergeSnapshotInt64(raw)
		if !ok {
			return personMergeSnapshotValue{}, fmt.Errorf("%T is not an integer", raw)
		}
		return personMergeSnapshotValue{Kind: personMergeSnapshotInteger, Integer: &value}, nil
	case strings.Contains(databaseType, "REAL") || strings.Contains(databaseType, "FLOA") ||
		strings.Contains(databaseType, "DOUBL") || strings.Contains(databaseType, "NUMERIC"):
		value, err := personMergeSnapshotFloat64(raw)
		return personMergeSnapshotValue{Kind: personMergeSnapshotReal, Real: &value}, err
	case strings.Contains(databaseType, "BLOB") || strings.Contains(databaseType, "BYTEA"):
		value, err := personMergeSnapshotBytesValue(raw)
		return personMergeSnapshotValue{Kind: personMergeSnapshotBytes, Bytes: value}, err
	default:
		value := personMergeSnapshotTextValue(raw)
		if strings.Contains(databaseType, "JSON") {
			var decoded any
			decoder := json.NewDecoder(strings.NewReader(value))
			decoder.UseNumber()
			if err := decoder.Decode(&decoded); err != nil {
				return personMergeSnapshotValue{}, err
			}
			var trailing any
			if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
				if err == nil {
					err = errors.New("multiple JSON values")
				}
				return personMergeSnapshotValue{}, err
			}
			canonical, err := json.Marshal(decoded)
			if err != nil {
				return personMergeSnapshotValue{}, err
			}
			value = string(canonical)
		}
		return personMergeSnapshotValue{Kind: personMergeSnapshotText, Text: &value}, nil
	}
}

func personMergeSnapshotInt64(raw any) (int64, bool) {
	switch value := raw.(type) {
	case int64:
		return value, true
	case int32:
		return int64(value), true
	case int:
		return int64(value), true
	case bool:
		if value {
			return 1, true
		}
		return 0, true
	case []byte:
		parsed, err := strconv.ParseInt(string(value), 10, 64)
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func personMergeSnapshotBool(raw any) (bool, error) {
	if value, ok := raw.(bool); ok {
		return value, nil
	}
	if value, ok := personMergeSnapshotInt64(raw); ok {
		return value != 0, nil
	}
	parsed, err := strconv.ParseBool(personMergeSnapshotTextValue(raw))
	if err != nil {
		return false, fmt.Errorf("parse snapshot boolean: %w", err)
	}
	return parsed, nil
}

func personMergeSnapshotFloat64(raw any) (float64, error) {
	switch value := raw.(type) {
	case float64:
		return value, nil
	case float32:
		return float64(value), nil
	case int64:
		return float64(value), nil
	case []byte:
		parsed, err := strconv.ParseFloat(string(value), 64)
		if err != nil {
			return 0, fmt.Errorf("parse snapshot float bytes: %w", err)
		}
		return parsed, nil
	case string:
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, fmt.Errorf("parse snapshot float: %w", err)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%T is not a real number", raw)
	}
}

func personMergeSnapshotBytesValue(raw any) ([]byte, error) {
	switch value := raw.(type) {
	case []byte:
		return append([]byte(nil), value...), nil
	case string:
		return []byte(value), nil
	default:
		return nil, fmt.Errorf("%T is not bytes", raw)
	}
}

func personMergeSnapshotTextValue(raw any) string {
	switch value := raw.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	case time.Time:
		return value.UTC().Format(time.RFC3339Nano)
	case nil:
		return ""
	default:
		return fmt.Sprint(value)
	}
}

func appendUniqueString(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func encodePersonMergeSnapshot(snapshot personMergeSnapshot) ([]byte, string, error) {
	if snapshot.Version != personMergeSnapshotVersion {
		return nil, "", fmt.Errorf("%w: unsupported snapshot version %d",
			ErrPersonMergeInvalid, snapshot.Version)
	}
	canonical, err := json.Marshal(snapshot)
	if err != nil {
		return nil, "", fmt.Errorf("marshal person merge snapshot: %w", err)
	}
	digest := sha256.Sum256(canonical)

	var compressed bytes.Buffer
	writer, err := zlib.NewWriterLevel(&compressed, zlib.BestCompression)
	if err != nil {
		return nil, "", fmt.Errorf("create person merge snapshot compressor: %w", err)
	}
	if _, err := writer.Write(canonical); err != nil {
		_ = writer.Close()
		return nil, "", fmt.Errorf("compress person merge snapshot: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("finish person merge snapshot compression: %w", err)
	}
	return compressed.Bytes(), hex.EncodeToString(digest[:]), nil
}

func decodePersonMergeSnapshot(compressed []byte, wantSHA256 string) (personMergeSnapshot, error) {
	reader, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return personMergeSnapshot{}, fmt.Errorf("%w: open zlib stream: %w",
			ErrPersonMergeSnapshotCorrupt, err)
	}
	canonical, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return personMergeSnapshot{}, fmt.Errorf("%w: read zlib stream: %w",
			ErrPersonMergeSnapshotCorrupt, readErr)
	}
	if closeErr != nil {
		return personMergeSnapshot{}, fmt.Errorf("%w: close zlib stream: %w",
			ErrPersonMergeSnapshotCorrupt, closeErr)
	}
	digest := sha256.Sum256(canonical)
	if hex.EncodeToString(digest[:]) != wantSHA256 {
		return personMergeSnapshot{}, fmt.Errorf("%w: SHA-256 mismatch",
			ErrPersonMergeSnapshotCorrupt)
	}

	var snapshot personMergeSnapshot
	if err := json.Unmarshal(canonical, &snapshot); err != nil {
		return personMergeSnapshot{}, fmt.Errorf("%w: decode JSON: %w",
			ErrPersonMergeSnapshotCorrupt, err)
	}
	if snapshot.Version != personMergeSnapshotVersion {
		return personMergeSnapshot{}, fmt.Errorf("%w: unsupported version %d",
			ErrPersonMergeSnapshotCorrupt, snapshot.Version)
	}
	return snapshot, nil
}
