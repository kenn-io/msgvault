package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopySubsetPreservesInferenceDebtWithoutApproval(t *testing.T) {
	tests := []struct {
		name        string
		options     CopySubsetOptions
		legacy      string
		employment  bool
		historical  bool
		manual      bool
		nonportable bool
		revision    int64
		want        int64
	}{
		{name: "approved attributes", options: CopySubsetOptions{IncludeAttributes: true}, revision: 9, want: 9},
		{name: "historical debt", options: CopySubsetOptions{IncludeAttributes: true}, historical: true, revision: 7, want: 7},
		{name: "legacy attributes without table", options: CopySubsetOptions{IncludeAttributes: true}, legacy: "table", want: 1},
		{name: "legacy attributes without state", options: CopySubsetOptions{IncludeAttributes: true}, legacy: "row", want: 1},
		{name: "approved employment", options: CopySubsetOptions{IncludeProfiles: true}, employment: true, revision: 6, want: 6},
		{name: "legacy employment", options: CopySubsetOptions{IncludeProfiles: true}, employment: true, legacy: "table", want: 1},
		{name: "manual profiles", options: CopySubsetOptions{IncludeProfiles: true, IncludeAttributes: true}, employment: true, manual: true},
		{name: "nonportable inferred attribute", options: CopySubsetOptions{IncludeAttributes: true}, nonportable: true},
		{name: "identity only", revision: 9},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			sourcePath := createTestSourceDB(t, t.TempDir(), 5)
			source, err := Open(sourcePath)
			require.NoError(err)
			person, _, err := source.CreatePersonFromParticipant(1)
			require.NoError(err)
			provenance := ProvenanceExtraction
			if test.manual {
				provenance = ProvenanceUser
			}
			slug := AttributeSlugNotes
			value := "Example value"
			if test.nonportable {
				slug = AttributeSlugPrimaryChannel
				value = "chat"
			}
			_, err = source.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
				PersonID: person.ID, DefinitionSlug: slug, Value: AttributeValue{Type: AttributeValueText, Text: &value}, Source: provenance,
			})
			require.NoError(err)
			if test.historical {
				inferenceNote(t, source, person.ID, ProvenanceUser, "Declared replacement")
			}
			if test.employment {
				org, err := source.CreateOrganizationContext(t.Context(), OrganizationInput{Name: "Example Org"})
				require.NoError(err)
				_, err = source.AddEmploymentContext(t.Context(), EmploymentInput{PersonID: person.ID, OrganizationID: org.ID, Title: new("Engineer"), Source: provenance})
				require.NoError(err)
			}
			if test.revision > 0 {
				book := inferenceMigrationBook(t, source)
				_, err = source.db.Exec(`UPDATE person_carddav_inference_state SET inference_revision=?, approved_revision=?,
 approved_connection_generation=1, approved_address_book_id=? WHERE person_id=?`, test.revision, test.revision, book.ID, person.ID)
				require.NoError(err)
			}
			// This complete person has no selected message and must not cross the
			// subset's identity boundary merely because they have inference state.
			excluded := inferencePerson(t, source, "excluded-subset@example.test")
			inferenceNote(t, source, excluded.ID, ProvenanceExtraction, "Excluded inferred note")
			switch test.legacy {
			case "table":
				_, err = source.db.Exec(`DROP TABLE person_carddav_inference_state`)
				require.NoError(err)
			case "row":
				_, err = source.db.Exec(`DELETE FROM person_carddav_inference_state WHERE person_id=?`, person.ID)
				require.NoError(err)
			}
			require.NoError(source.Close())
			destinationDir := filepath.Join(t.TempDir(), "subset")
			_, err = CopySubsetWithOptions(sourcePath, destinationDir, 5, test.options)
			require.NoError(err)
			for range 2 {
				destination, err := Open(filepath.Join(destinationDir, "msgvault.db"))
				require.NoError(err)
				// Check before InitSchema: the completed transfer itself must establish
				// debt, including when the empty destination already ledgered migration.
				assert.Equal(test.want, inferenceRevision(t, destination, person.ID))
				require.NoError(destination.withReadSnapshotContext(t.Context(), func(tx *loggedTx) error {
					state, err := destination.getCardDAVInferenceExportStateTx(t.Context(), tx, person.ID)
					require.NoError(err)
					assert.Zero(state.ApprovedRevision)
					assert.Nil(state.ApprovedConnectionGeneration)
					assert.Nil(state.ApprovedAddressBookID)
					return nil
				}))
				_, err = destination.GetPersonContext(t.Context(), excluded.ID)
				require.ErrorIs(err, ErrPersonNotFound)
				var stateRows, accounts, books int
				require.NoError(destination.db.QueryRow(`SELECT COUNT(*) FROM person_carddav_inference_state`).Scan(&stateRows))
				wantRows := 0
				if test.want > 0 {
					wantRows = 1
				}
				assert.Equal(wantRows, stateRows)
				require.NoError(destination.db.QueryRow(`SELECT COUNT(*) FROM carddav_accounts`).Scan(&accounts))
				require.NoError(destination.db.QueryRow(`SELECT COUNT(*) FROM carddav_address_books`).Scan(&books))
				assert.Zero(accounts)
				assert.Zero(books)
				require.NoError(destination.InitSchema())
				assert.Equal(test.want, inferenceRevision(t, destination, person.ID))
				require.NoError(destination.Close())
			}
			if test.revision > 0 {
				unchanged, err := Open(sourcePath)
				require.NoError(err)
				assert.Equal(test.revision, inferenceRevision(t, unchanged, person.ID))
				var approved int64
				require.NoError(unchanged.db.QueryRow(`SELECT approved_revision FROM person_carddav_inference_state WHERE person_id=?`, person.ID).Scan(&approved))
				assert.Equal(test.revision, approved, "copy must not revoke source approval")
				require.NoError(unchanged.Close())
			}
		})
	}
}
