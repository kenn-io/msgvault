package store

import (
	"context"
	"fmt"
	"strings"
)

// vcardPersonComponentTables and vcardOrganizationComponentTables are the
// provenance-bearing profile component tables, keyed by person_id and
// organization_id respectively. A source resource identity rewrite has to
// visit every one of them, and the projection and CAS bumps that precede it
// resolve the affected owners from the same list.
var (
	vcardPersonComponentTables = []string{
		"person_names", "person_contact_points", "person_addresses",
		"person_dates", "person_categories", "person_media",
	}
	vcardOrganizationComponentTables = []string{
		"organization_names", "organization_identifiers", "organization_addresses",
		"organization_contact_points", "organization_categories", "organization_media",
	}
)

// sourceResourceColumn names one ID column of one provenance-bearing table.
type sourceResourceColumn struct{ table, column string }

func sourceResourceColumns(tables []string, column string) []sourceResourceColumn {
	columns := make([]sourceResourceColumn, 0, len(tables))
	for _, table := range tables {
		columns = append(columns, sourceResourceColumn{table: table, column: column})
	}
	return columns
}

// RewriteVCardResourceSourceUIDContext rewrites a resource identity and all
// provenance rows that refer to it under revision compare-and-swap.
func (s *Store) RewriteVCardResourceSourceUIDContext(
	ctx context.Context,
	sourceRef, oldUID, newUID string,
	expectedRevision int64,
) (*VCardResourceEnvelopeRecord, error) {
	if strings.TrimSpace(newUID) == "" || expectedRevision <= 0 {
		return nil, fmt.Errorf(
			"%w: new source UID and positive revision are required",
			ErrVCardResourceInvalid,
		)
	}
	return retryContendedWrite(ctx, s, "rewrite vCard source resource UID",
		func() (*VCardResourceEnvelopeRecord, error) {
			return s.rewriteVCardResourceSourceUIDOnce(
				ctx, sourceRef, oldUID, newUID, expectedRevision,
			)
		})
}

func (s *Store) rewriteVCardResourceSourceUIDOnce(
	ctx context.Context,
	sourceRef, oldUID, newUID string,
	expectedRevision int64,
) (*VCardResourceEnvelopeRecord, error) {
	var record *VCardResourceEnvelopeRecord
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		current, err := s.findVCardResourceEnvelopeTx(ctx, tx, sourceRef, oldUID)
		if err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return newVCardResourceWriteConflict(
				sourceRef, oldUID, expectedRevision,
			)
		}
		if err := s.bumpVCardSourceResourceOwnersTx(
			ctx, tx, current.PersonID, sourceRef, oldUID,
		); err != nil {
			return err
		}
		if err := s.rewriteVCardSourceResourceProvenanceTx(
			ctx, tx, sourceRef, oldUID, newUID,
		); err != nil {
			return err
		}
		updated, err := tx.ExecContext(ctx, `UPDATE vcard_resource_envelopes
			SET source_resource_uid = ?, revision = revision + 1,
			    updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND revision = ?`,
			newUID, current.ID, expectedRevision,
		)
		if err := s.vcardResourceCASOutcome(
			updated, err, "rewrite vCard source resource UID",
			current.ResourceEnvelope, expectedRevision,
		); err != nil {
			return err
		}
		record, err = s.getVCardResourceEnvelopeTx(ctx, tx, current.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

// bumpVCardSourceResourceOwnersTx advances every token a source resource
// identity rewrite invalidates. The rewrite changes provenance rows without
// changing their values, but it still changes the occurrence identity every
// projection built from them carries, so it reaches: the public CAS revision
// of every organization and person that owns a component row under the
// identity; the projection revision of the envelope's own person, of every
// owner, of both endpoints of every edge and the person of every review under
// it, and of everyone employed by an affected organization.
//
// Organizations are bumped before any person row is locked, matching the
// organization-then-person lock order organization profile replacement uses.
func (s *Store) bumpVCardSourceResourceOwnersTx(
	ctx context.Context, tx *loggedTx, envelopePersonID int64,
	sourceRef, sourceResourceUID string,
) error {
	organizationIDs, err := s.vcardSourceResourceIDsTx(ctx, tx,
		sourceResourceColumns(vcardOrganizationComponentTables, "organization_id"),
		sourceRef, sourceResourceUID)
	if err != nil {
		return err
	}
	if err := s.bumpOrganizationRevisionsTx(ctx, tx, organizationIDs...); err != nil {
		return err
	}
	ownerIDs, err := s.vcardSourceResourceIDsTx(ctx, tx,
		sourceResourceColumns(vcardPersonComponentTables, "person_id"),
		sourceRef, sourceResourceUID)
	if err != nil {
		return err
	}
	relatedIDs, err := s.vcardSourceResourceIDsTx(ctx, tx, []sourceResourceColumn{
		{table: "person_relationships", column: "source_person_id"},
		{table: "person_relationships", column: "target_person_id"},
		{table: "person_relationship_reviews", column: "person_id"},
	}, sourceRef, sourceResourceUID)
	if err != nil {
		return err
	}
	projectionIDs := append(append(relatedIDs, envelopePersonID), ownerIDs...)
	if err := s.bumpPersonVCardProjectionsTx(ctx, tx, projectionIDs...); err != nil {
		return err
	}
	if err := s.bumpEmployedPersonVCardProjectionsTx(ctx, tx, organizationIDs...); err != nil {
		return err
	}
	return s.bumpPersonRevisionsTx(ctx, tx, ownerIDs...)
}

// vcardSourceResourceIDsTx returns the distinct IDs named by the given columns
// across every row that carries the source resource identity.
func (s *Store) vcardSourceResourceIDsTx(
	ctx context.Context, tx *loggedTx, columns []sourceResourceColumn,
	sourceRef, sourceResourceUID string,
) ([]int64, error) {
	selects := make([]string, 0, len(columns))
	args := make([]any, 0, 2*len(columns))
	for _, column := range columns {
		selects = append(selects, `SELECT `+column.column+` FROM `+column.table+
			` WHERE source_ref = ? AND source_resource_uid = ?`)
		args = append(args, sourceRef, sourceResourceUID)
	}
	rows, err := tx.QueryContext(ctx, strings.Join(selects, " UNION "), args...)
	if err != nil {
		return nil, fmt.Errorf("find vCard source resource owners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0, 4)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan vCard source resource owner: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vCard source resource owners: %w", err)
	}
	return ids, nil
}

// bumpOrganizationRevisionsTx advances the public profile compare-and-swap
// token of each organization.
func (s *Store) bumpOrganizationRevisionsTx(
	ctx context.Context, tx *loggedTx, organizationIDs ...int64,
) error {
	placeholders, args := sortedIDPlaceholders(organizationIDs)
	if placeholders == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE organizations
		SET revision = revision + 1, updated_at = `+s.dialect.Now()+`
		WHERE id IN (`+placeholders+`)
	`, args...); err != nil {
		return fmt.Errorf("bump organization revisions: %w", err)
	}
	return nil
}

func (s *Store) rewriteVCardSourceResourceProvenanceTx(
	ctx context.Context, tx *loggedTx, sourceRef, oldUID, newUID string,
) error {
	tables := make([]string, 0, len(vcardPersonComponentTables)+
		len(vcardOrganizationComponentTables)+3)
	tables = append(tables, vcardPersonComponentTables...)
	tables = append(tables,
		"person_relationships", "person_relationship_reviews",
		"participant_contact_observations",
	)
	tables = append(tables, vcardOrganizationComponentTables...)
	for _, table := range tables {
		set := "source_resource_uid = ?, updated_at = " + s.dialect.Now()
		// Edges carry their own compare-and-swap revision; the other rows
		// are versioned through their owner.
		if table == "person_relationships" {
			set += ", revision = revision + 1"
		}
		_, err := tx.ExecContext(ctx, `UPDATE `+table+` SET `+set+`
			WHERE source_ref = ? AND source_resource_uid = ?`,
			newUID, sourceRef, oldUID,
		)
		if err != nil {
			if s.dialect.IsConflictError(err) {
				return ErrVCardResourceIdentityExists
			}
			return fmt.Errorf(
				"rewrite vCard source resource provenance in %s: %w", table, err,
			)
		}
	}
	return nil
}
