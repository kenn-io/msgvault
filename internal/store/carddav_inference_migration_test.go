package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/vcard"
)

func inferenceMigrationBook(t *testing.T, st *Store) CardDAVAddressBook {
	t.Helper()
	allowed := true
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example.test/dav", Username: "example", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/",
		Books: []CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/personal/", DisplayName: "Personal", CanCreate: &allowed}},
	})
	require.NoError(t, err)
	require.Len(t, books, 1)
	return books[0]
}

func inferenceMigrationPublication(t *testing.T, st *Store, id int64, book CardDAVAddressBook, pending bool) {
	t.Helper()
	if pending {
		snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), id)
		require.NoError(t, err)
		// Persist the legacy fixture directly: current preparation now correctly
		// refuses the historical unreviewed inference this upgrade test models.
		_, err = st.db.Exec(`INSERT INTO carddav_publications
   (person_id, desired, address_book_id, href, pending_operation, outgoing_body,
    outgoing_semantic_hash, local_hash, connection_generation, book_sync_revision,
    mapping_revision, mutation_revision, pending_started_at)
   SELECT ?, TRUE, ?, ?, 'create', ?, 'legacy', ?, connection_generation, ?, 0, 1, CURRENT_TIMESTAMP
   FROM carddav_accounts WHERE id = 1`, id, book.ID, fmt.Sprintf("%s%d.vcf", book.CanonicalURL, id),
			[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Example\r\nEND:VCARD\r\n"), snapshot.Fingerprint, book.SyncRevision)
		require.NoError(t, err)
		return
	}
	_, err := st.db.Exec(`INSERT INTO carddav_publications (person_id, desired, address_book_id, href) VALUES (?, TRUE, ?, ?)`, id, book.ID, fmt.Sprintf("%s%d.vcf", book.CanonicalURL, id))
	require.NoError(t, err)
}

func runLegacyInferenceUpgrade(t *testing.T, st *Store) {
	t.Helper()
	// Remove only the new table and ledger entry, then let actual InitSchema
	// recreate the table and run the migration against persisted legacy data.
	_, err := st.db.Exec(`DROP TABLE person_carddav_inference_state`)
	require.NoError(t, err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationCardDAVInferenceExportState)
	require.NoError(t, err)
	require.NoError(t, st.InitSchema())
}

func TestCardDAVInferenceMigrationCurrentAndHistoricalContributors(t *testing.T) {
	for _, scenario := range []string{"current", "desired history", "pending history", "unpublished history", "nonportable history", "rejected", "deleted employment", "unpublished deleted employment", "legacy mapped attribute"} {
		t.Run(scenario, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, id, targets := newPersonFactProjectionStore(t)
			book := inferenceMigrationBook(t, st)
			want := int64(1)
			switch scenario {
			case "deleted employment", "unpublished deleted employment":
				org := createPersonFactOrganization(t, st, "Example Org", "employer.example")
				target := projectionTargetBySlug(t, st, "employment")
				claim := personFactProjectionClaim(id, target, fmt.Sprintf(`{"organization":{"id":%d,"name":"Example Org","domain":"employer.example"},"title":"Engineer"}`, org.ID), "employment")
				result, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(id, "employment", []personfacts.ProposedClaim{claim}, nil), nil)
				require.NoError(err)
				require.Len(result.Projections, 1)
				emp, err := st.GetEmploymentContext(t.Context(), result.Projections[0].RowID)
				require.NoError(err)
				require.NoError(st.DeleteEmploymentContext(t.Context(), emp.ID, emp.Revision))
				if scenario == "unpublished deleted employment" {
					want = 0
				}
			case "rejected":
				claim := personFactProjectionClaim(id, targets[AttributeSlugNotes], `42`, "invalid")
				result, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(id, "invalid", []personfacts.ProposedClaim{claim}, nil), nil)
				require.NoError(err)
				require.Empty(result.Projections)
				want = 0
			case "nonportable history":
				_, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{PersonID: id, DefinitionSlug: AttributeSlugPrimaryChannel, Value: AttributeValue{Type: AttributeValueText, Text: new("chat")}, Source: ProvenanceExtraction})
				require.NoError(err)
				want = 0
			default:
				slug := AttributeSlugNotes
				if scenario == "legacy mapped attribute" {
					definition, err := st.CreateAttributeDefinitionContext(t.Context(), AttributeDefinitionInput{
						UniversalID: "legacy-review-field", ObjectType: AttributeObjectPerson, Slug: "legacy_review_field", Label: "Legacy review field", ValueType: AttributeValueText, FieldType: AttributeFieldText, Cardinality: AttributeCardinalitySingle, Ownership: AttributeOwnershipUser, APIMutable: true, VCardProperty: new("X-LEGACY"),
					})
					require.NoError(err)
					slug = definition.Slug
				}
				_, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{PersonID: id, DefinitionSlug: slug, Value: AttributeValue{Type: AttributeValueText, Text: new("Earlier note")}, Source: ProvenanceExtraction})
				require.NoError(err)
				if scenario == "legacy mapped attribute" {
					values, err := st.ListPersonAttributeValuesContext(t.Context(), id, PersonAttributeQuery{DefinitionSlug: slug})
					require.NoError(err)
					require.Len(values, 1)
					envelope, err := vcard.ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:example\r\nFN:Example\r\nNOTE:Earlier note\r\nEND:VCARD\r\n"))
					require.NoError(err)
					envelope.SourceRef = fmt.Sprintf("carddav:%d", book.ID)
					envelope.SourceResourceUID = fmt.Sprintf("%s%d.vcf", book.CanonicalURL, id)
					for _, property := range envelope.PropertyTree {
						if property.Property.Name == "NOTE" {
							envelope.NativeMappings = []vcard.NativeMapping{{Identity: property.Identity, Table: "person_attribute_values", RowID: values[0].ID, Field: "value", Kind: vcard.HandlingNative}}
						}
					}
					envelope.Residue = vcard.ResidueWithMappings(envelope.PropertyTree, envelope.NativeMappings)
					_, err = st.PutVCardResourceEnvelopeContext(t.Context(), VCardResourceEnvelopeInput{PersonID: id, Envelope: envelope})
					require.NoError(err)
					// Model an old custom mapping whose definition no longer exports.
					_, err = st.db.Exec(`UPDATE attribute_definitions SET vcard_property = NULL, is_active = FALSE WHERE id = ?`, values[0].DefinitionID)
					require.NoError(err)
				}
				if scenario != "current" && scenario != "legacy mapped attribute" {
					inferenceNote(t, st, id, ProvenanceUser, "Declared replacement")
				}
				if scenario == "unpublished history" {
					want = 0
				}
			}
			if scenario != "current" && scenario != "unpublished history" && scenario != "unpublished deleted employment" {
				inferenceMigrationPublication(t, st, id, book, scenario == "pending history")
			}
			var valuesBefore, decisionsBefore int
			require.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM person_attribute_values`).Scan(&valuesBefore))
			require.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM person_fact_decisions`).Scan(&decisionsBefore))
			runLegacyInferenceUpgrade(t, st)
			assert.Equal(want, inferenceRevision(t, st, id))
			require.NoError(st.InitSchema())
			assert.Equal(want, inferenceRevision(t, st, id))
			var valuesAfter, decisionsAfter int
			require.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM person_attribute_values`).Scan(&valuesAfter))
			require.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM person_fact_decisions`).Scan(&decisionsAfter))
			assert.Equal(valuesBefore, valuesAfter)
			assert.Equal(decisionsBefore, decisionsAfter)
		})
	}
}

func TestCardDAVInferenceMigrationPreservesNewerState(t *testing.T) {
	require := require.New(t)
	st, id, _ := newPersonFactProjectionStore(t)
	book := inferenceMigrationBook(t, st)
	inferenceNote(t, st, id, ProvenanceExtraction, "Current")
	_, err := st.db.Exec(`UPDATE person_carddav_inference_state SET inference_revision=9, approved_revision=8, approved_connection_generation=1, approved_address_book_id=? WHERE person_id=?`, book.ID, id)
	require.NoError(err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name=?`, migrationCardDAVInferenceExportState)
	require.NoError(err)
	for range 2 {
		require.NoError(st.InitSchema())
		require.NoError(st.withReadSnapshotContext(t.Context(), func(tx *loggedTx) error {
			state, err := st.getCardDAVInferenceExportStateTx(t.Context(), tx, id)
			require.NoError(err)
			assert.Equal(t, CardDAVInferenceExportState{PersonID: id, InferenceRevision: 9, ApprovedRevision: 8, ApprovedConnectionGeneration: new(int64(1)), ApprovedAddressBookID: &book.ID}, state)
			return nil
		}))
	}
}

func TestCardDAVInferenceMigrationRunsAfterLegacyColumnMigrations(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "legacy.db")
	st, err := Open(path)
	require.NoError(err)
	require.NoError(st.InitSchema())
	_, err = st.db.Exec(`INSERT INTO persons(vcard_uid, display_name) VALUES ('legacy', 'Legacy Person')`)
	require.NoError(err)
	// An archive from before is_sensitive existed gains that column only from
	// LegacyColumnMigrations, so the backfill's projection must not run first.
	_, err = st.db.Exec(`ALTER TABLE attribute_definitions DROP COLUMN is_sensitive`)
	require.NoError(err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationCardDAVInferenceExportState)
	require.NoError(err)
	require.NoError(st.Close())

	reopened, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(reopened.InitSchema())
	applied, err := reopened.IsMigrationAppliedContext(t.Context(), migrationCardDAVInferenceExportState, 1)
	require.NoError(err)
	require.True(applied)
}
