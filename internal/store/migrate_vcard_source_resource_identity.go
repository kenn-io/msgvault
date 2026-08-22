package store

import (
	"context"
	"fmt"
)

// ensureVCardSourceResourceIdentityIndexes upgrades identity uniqueness after
// the legacy-column loop has made source_resource_uid available. The bootstrap
// schemas deliberately retain their pre-upgrade indexes because they execute
// before legacy columns are added on an existing archive.
func (s *Store) ensureVCardSourceResourceIdentityIndexes(ctx context.Context) error {
	return s.runOnceMigration(ctx, migrationVCardSourceResourceIdentity, false,
		func(ctx context.Context) error {
			return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
				for _, statement := range vCardSourceResourceIdentityIndexStatements {
					if _, err := tx.ExecContext(ctx, statement); err != nil {
						return fmt.Errorf("rebuild vCard resource identity index: %w", err)
					}
				}
				return nil
			})
		})
}

var vCardSourceResourceIdentityIndexStatements = []string{
	`DROP INDEX IF EXISTS idx_person_names_property_identity`,
	`CREATE UNIQUE INDEX idx_person_names_property_identity
		ON person_names(person_id, source, source_ref, COALESCE(source_resource_uid, ''), vcard_property, vcard_prop_id)
		WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL`,
	`DROP INDEX IF EXISTS idx_person_contact_points_property_identity`,
	`CREATE UNIQUE INDEX idx_person_contact_points_property_identity
		ON person_contact_points(person_id, source, source_ref, COALESCE(source_resource_uid, ''), vcard_property, vcard_prop_id)
		WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL`,
	`DROP INDEX IF EXISTS idx_person_addresses_property_identity`,
	`CREATE UNIQUE INDEX idx_person_addresses_property_identity
		ON person_addresses(person_id, source, source_ref, COALESCE(source_resource_uid, ''), vcard_property, vcard_prop_id)
		WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL`,
	`DROP INDEX IF EXISTS idx_person_dates_property_identity`,
	`CREATE UNIQUE INDEX idx_person_dates_property_identity
		ON person_dates(person_id, source, source_ref, COALESCE(source_resource_uid, ''), vcard_property, vcard_prop_id)
		WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL`,
	`DROP INDEX IF EXISTS idx_person_media_property_identity`,
	`CREATE UNIQUE INDEX idx_person_media_property_identity
		ON person_media(person_id, source, source_ref, COALESCE(source_resource_uid, ''), vcard_property, vcard_prop_id)
		WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL`,
	`DROP INDEX IF EXISTS idx_organization_names_property_identity`,
	`CREATE UNIQUE INDEX idx_organization_names_property_identity
		ON organization_names(organization_id, source, source_ref, COALESCE(source_resource_uid, ''), vcard_property, vcard_prop_id)
		WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL`,
	`DROP INDEX IF EXISTS idx_organization_identifiers_property_identity`,
	`CREATE UNIQUE INDEX idx_organization_identifiers_property_identity
		ON organization_identifiers(organization_id, source, source_ref, COALESCE(source_resource_uid, ''), vcard_property, vcard_prop_id)
		WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL`,
	`DROP INDEX IF EXISTS idx_organization_addresses_property_identity`,
	`CREATE UNIQUE INDEX idx_organization_addresses_property_identity
		ON organization_addresses(organization_id, source, source_ref, COALESCE(source_resource_uid, ''), vcard_property, vcard_prop_id)
		WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL`,
	`DROP INDEX IF EXISTS idx_organization_contact_points_property_identity`,
	`CREATE UNIQUE INDEX idx_organization_contact_points_property_identity
		ON organization_contact_points(organization_id, source, source_ref, COALESCE(source_resource_uid, ''), vcard_property, vcard_prop_id)
		WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL`,
	`DROP INDEX IF EXISTS idx_organization_media_property_identity`,
	`CREATE UNIQUE INDEX idx_organization_media_property_identity
		ON organization_media(organization_id, source, source_ref, COALESCE(source_resource_uid, ''), vcard_property, vcard_prop_id)
		WHERE source_ref IS NOT NULL AND vcard_prop_id IS NOT NULL AND superseded_at IS NULL`,
	`DROP INDEX IF EXISTS idx_person_relationship_reviews_occurrence_unique`,
	`CREATE UNIQUE INDEX idx_person_relationship_reviews_occurrence_unique
		ON person_relationship_reviews(
			person_id, raw_related_type, raw_related_value, source,
			COALESCE(source_ref, ''), COALESCE(source_resource_uid, ''),
			COALESCE(vcard_property, ''), COALESCE(vcard_group, ''),
			COALESCE(vcard_prop_id, ''), COALESCE(vcard_pid, ''),
			COALESCE(vcard_altid, '')
		)`,
}
