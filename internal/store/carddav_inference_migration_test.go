package store

import (
	"fmt"
	"path/filepath"
	"strconv"
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

func TestCardDAVInferenceMigrationV2MarksInferredContactProfilesForReview(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "upgraded.db")
	st, err := Open(path)
	require.NoError(err)
	require.NoError(st.InitSchema())
	var contactOnly, declared int64
	require.NoError(st.db.QueryRow(`INSERT INTO persons(vcard_uid, display_name) VALUES ('contact-only', 'Contact Only') RETURNING id`).Scan(&contactOnly))
	require.NoError(st.db.QueryRow(`INSERT INTO persons(vcard_uid, display_name) VALUES ('declared', 'Declared') RETURNING id`).Scan(&declared))
	require.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
		for _, person := range []struct {
			id     int64
			source Provenance
		}{{contactOnly, ProvenanceEnrichment}, {declared, ProvenanceUser}} {
			if _, err := st.addPersonContactPointTx(t.Context(), tx, person.id, PersonContactPointInput{
				AddressKind: ContactAddressEmail, OriginalValue: "person" + strconv.FormatInt(person.id, 10) + "@example.com",
				Envelope: ValueEnvelopeInput{Source: person.source},
			}); err != nil {
				return err
			}
		}
		return nil
	}))
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationCardDAVInferenceExportStateV2)
	require.NoError(err)
	require.NoError(st.Close())

	reopened, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(reopened.InitSchema())
	for _, name := range []string{migrationCardDAVInferenceExportState, migrationCardDAVInferenceExportStateV2} {
		applied, err := reopened.IsMigrationAppliedContext(t.Context(), name, 1)
		require.NoError(err)
		assert.True(applied, name)
	}
	assert.Equal(int64(1), inferenceRevision(t, reopened, contactOnly))
	assert.Zero(inferenceRevision(t, reopened, declared))
}

func TestCardDAVInferenceMigrationV2FindsPendingUpdateMappingsForRetiredInferredContacts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	book := inferenceMigrationBook(t, st)
	contact, err := st.AddPersonContactPointContext(t.Context(), personID, PersonContactPointInput{
		AddressKind: ContactAddressEmail, OriginalValue: "retired@example.test",
		Envelope: ValueEnvelopeInput{Source: ProvenanceEnrichment},
	})
	require.NoError(err)
	envelope, err := vcard.ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:person\r\nFN:Retired Contact\r\nEMAIL:retired@example.test\r\nEND:VCARD\r\n"))
	require.NoError(err)
	var emailIdentity vcard.PropertyIdentity
	for _, occurrence := range envelope.PropertyTree {
		if occurrence.Property.Name == "EMAIL" {
			emailIdentity = occurrence.Identity
			break
		}
	}
	require.NotEmpty(emailIdentity)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: emailIdentity, Table: personContactPointsTableName, RowID: contact.Envelope.ID,
		Field: "original_value", Kind: vcard.HandlingNative,
	}}
	envelope.Residue = vcard.ResidueWithMappings(envelope.PropertyTree, envelope.NativeMappings)
	metadata, err := vcard.MarshalResourceMetadata(envelope)
	require.NoError(err)
	inferenceMigrationPublication(t, st, personID, book, false)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.db.Exec(`UPDATE carddav_publications
        SET pending_operation=?, outgoing_body=?, outgoing_semantic_hash=?, local_hash=?,
            connection_generation=(SELECT connection_generation FROM carddav_accounts WHERE id=1),
            book_sync_revision=?, mapping_revision=1, mutation_revision=1,
            outgoing_envelope_metadata=?, pending_started_at=CURRENT_TIMESTAMP
        WHERE person_id=?`, CardDAVMutationUpdate, envelope.StoredBody, "legacy-update", snapshot.Fingerprint,
		book.SyncRevision, metadata, personID)
	require.NoError(err)
	require.NoError(st.SupersedePersonContactPointContext(t.Context(), personID, contact.Envelope.ID, nil))
	// Model the pre-V2 archive: its inferred contact and pending intent exist,
	// but writer mutations did not create inference export state.
	_, err = st.db.Exec(`DELETE FROM person_carddav_inference_state WHERE person_id=?`, personID)
	require.NoError(err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name=?`, migrationCardDAVInferenceExportStateV2)
	require.NoError(err)

	require.NoError(st.InitSchema())
	assert.Equal(int64(1), inferenceRevision(t, st, personID))
	publication, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(CardDAVMutationUpdate, publication.PendingOperation)
	assert.Equal(metadata, publication.OutgoingEnvelopeMetadata)
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	require.NoError(err)
	assert.True(source.Inference.ReviewRequired(source.ConnectionGeneration, book.ID))
}

func TestCardDAVInferenceMigrationV2InvalidatesApprovalForNewContributors(t *testing.T) {
	for _, kind := range []string{"contact", "name", "address", "declared contact", "nonrendering contact", "notes only"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, id, _ := newPersonFactProjectionStore(t)
			book := inferenceMigrationBook(t, st)
			inferenceNote(t, st, id, ProvenanceExtraction, "Approved note")
			source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), id)
			require.NoError(err)
			require.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
				if err := st.approveCardDAVInferenceTx(t.Context(), tx, source.Inference, source.ConnectionGeneration, book.ID); err != nil {
					return err
				}
				// Old writers did not track these rows in the inference revision.
				envelope := ValueEnvelopeInput{Source: ProvenanceEnrichment}
				switch kind {
				case "name":
					_, err = st.addPersonNameTx(t.Context(), tx, id, PersonNameInput{
						NameKind: PersonNameFormatted, Formatted: new("Alex Example"), OriginalValue: "Alex Example", Envelope: envelope,
					})
				case "address":
					_, err = st.addPersonAddressTx(t.Context(), tx, id, PersonAddressInput{
						AddressKind: PersonAddressPostal, StreetAddress: new("1 Example Street"), OriginalValue: "1 Example Street", Envelope: envelope,
					})
				case "contact", "declared contact", "nonrendering contact":
					input := PersonContactPointInput{AddressKind: ContactAddressEmail, OriginalValue: "alex@example.com", Envelope: envelope}
					switch kind {
					case "declared contact":
						input.Envelope.Source = ProvenanceUser
					case "nonrendering contact":
						input.AddressKind, input.OriginalValue = ContactAddressUsername, "@alice"
					}
					_, err = st.addPersonContactPointTx(t.Context(), tx, id, input)
				}
				return err
			}))
			_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationCardDAVInferenceExportStateV2)
			require.NoError(err)
			wantReview := kind == "contact" || kind == "name" || kind == "address"
			for range 2 {
				require.NoError(st.InitSchema())
				after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), id)
				require.NoError(err)
				assert.Equal(wantReview, after.Inference.ReviewRequired(after.ConnectionGeneration, book.ID))
				if wantReview {
					assert.Equal(source.Inference.InferenceRevision+1, after.Inference.InferenceRevision)
				} else {
					assert.Equal(source.Inference.InferenceRevision, after.Inference.InferenceRevision)
				}
			}
		})
	}
}

func TestCardDAVInferenceMigrationV2IgnoresMalformedRetiredDeclaredAddress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	address, err := st.AddPersonAddressContext(t.Context(), personID, PersonAddressInput{
		AddressKind: PersonAddressPostal, StreetAddress: new("10 Example Street"),
		OriginalValue: "10 Example Street",
		Envelope:      ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(st.SupersedePersonAddressContext(t.Context(), personID, address.Envelope.ID, nil))
	_, err = st.db.Exec(`UPDATE person_addresses SET extended_components = ? WHERE id = ?`, "apartment\rline", address.Envelope.ID)
	require.NoError(err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationCardDAVInferenceExportStateV2)
	require.NoError(err)

	require.NoError(st.InitSchema())
	applied, err := st.IsMigrationAppliedContext(t.Context(), migrationCardDAVInferenceExportStateV2, 1)
	require.NoError(err)
	assert.True(applied)
	assert.Zero(inferenceRevision(t, st, personID))
}

func TestCardDAVInferenceMigrationV2AllowsRepairOfMalformedCurrentInferredAddress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	address, err := st.AddPersonAddressContext(t.Context(), personID, PersonAddressInput{
		AddressKind: PersonAddressPostal, StreetAddress: new("10 Example Street"),
		ExtendedComponents: new("Apartment 2;Floor 3"), OriginalValue: "10 Example Street",
		Envelope: ValueEnvelopeInput{Source: ProvenanceEnrichment},
	})
	require.NoError(err)
	_, err = st.db.Exec(`UPDATE person_addresses SET extended_components = ? WHERE id = ?`, "apartment\rline", address.Envelope.ID)
	require.NoError(err)
	addresses, err := st.ListPersonAddressesContext(t.Context(), personID, true)
	require.NoError(err)
	require.Len(addresses, 1)
	_, _, err = addresses[0].VCardProjection()
	require.Error(err, "publication projection must still reject malformed stored values")

	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationCardDAVInferenceExportStateV2)
	require.NoError(err)
	require.NoError(st.InitSchema())
	applied, err := st.IsMigrationAppliedContext(t.Context(), migrationCardDAVInferenceExportStateV2, 1)
	require.NoError(err)
	assert.True(applied)
	assert.GreaterOrEqual(inferenceRevision(t, st, personID), int64(1))

	require.NoError(st.SupersedePersonAddressContext(t.Context(), personID, address.Envelope.ID, nil))
}

func TestCardDAVInferenceMigrationV2AllowsRepairOfMalformedCurrentInferredPhoneticName(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "carriage return", value: "Alice\rExample"},
		{name: "NUL", value: "Alice\x00Example"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			if test.name == "NUL" && st.dialect.DriverName() == postgresDriverName {
				t.Skip("PostgreSQL TEXT rejects embedded NUL bytes")
			}
			name, err := st.AddPersonNameContext(t.Context(), personID, PersonNameInput{
				NameKind: PersonNamePhonetic, FamilyName: new("Example"), GivenName: new("Alice"),
				OriginalValue: "Example;Alice",
				Envelope:      ValueEnvelopeInput{Source: ProvenanceEnrichment},
			})
			require.NoError(err)
			_, err = st.db.Exec(`UPDATE person_names SET given_name = ? WHERE id = ?`, test.value, name.Envelope.ID)
			require.NoError(err)
			_, err = st.db.Exec(`DELETE FROM person_carddav_inference_state WHERE person_id = ?`, personID)
			require.NoError(err)
			_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationCardDAVInferenceExportStateV2)
			require.NoError(err)

			require.NoError(st.InitSchema())
			applied, err := st.IsMigrationAppliedContext(t.Context(), migrationCardDAVInferenceExportStateV2, 1)
			require.NoError(err)
			assert.True(applied)
			assert.GreaterOrEqual(inferenceRevision(t, st, personID), int64(1),
				"malformed inferred names still carry publication review debt")

			require.NoError(st.SupersedePersonNameContext(t.Context(), personID, name.Envelope.ID, nil),
				"a profile edit must be able to retire and repair the malformed legacy name")
		})
	}
}

func TestMalformedCurrentInferredEmailCanBeRetired(t *testing.T) {
	for _, operation := range []string{"direct supersede", "profile patch"} {
		t.Run(operation, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			contact, err := st.AddPersonContactPointContext(t.Context(), personID, PersonContactPointInput{
				AddressKind: ContactAddressEmail, OriginalValue: "legacy@example.test",
				Envelope: ValueEnvelopeInput{Source: ProvenanceEnrichment},
			})
			require.NoError(err)
			profile, err := st.GetPersonProfileContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.db.Exec(st.Rebind(`UPDATE persons SET display_name = NULL WHERE id = ?`), personID)
			require.NoError(err)
			_, err = st.db.Exec(st.Rebind(`UPDATE person_contact_points SET original_value = ? WHERE id = ?`),
				"legacy\r@example.test", contact.Envelope.ID)
			require.NoError(err)

			if operation == "direct supersede" {
				err = st.SupersedePersonContactPointContext(
					t.Context(), personID, contact.Envelope.ID, nil,
				)
			} else {
				_, err = st.ApplyPersonProfilePatchContext(
					t.Context(), personID, profile.Person.Revision,
					PersonProfilePatch{ContactPoints: &PersonContactPointPatch{
						Supersede: []int64{contact.Envelope.ID},
					}},
				)
			}
			require.NoError(err, "a malformed legacy email must remain repairable through profile edits")

			current, err := st.ListPersonContactPointsContext(t.Context(), personID, true)
			require.NoError(err)
			assert.Empty(current)
		})
	}
}
