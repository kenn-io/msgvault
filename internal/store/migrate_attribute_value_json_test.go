package store

import (
	"database/sql"
	"encoding/json/jsontext"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttributeValueJSONSQLiteMigration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		t.Skip("requires SQLite")
	}
	st, person, _ := newPersonFactProjectionStore(t)
	def := AttributeDefinitionInput{UniversalID: "test-json-sqlite-migration", ObjectType: AttributeObjectPerson, Slug: "json_sqlite_migration", Label: "JSON migration", ValueType: AttributeValueJSON, FieldType: AttributeFieldJSON, Cardinality: AttributeCardinalitySingle, Ownership: AttributeOwnershipUser, APIMutable: true, IsDeletable: true}
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), def)
	require.NoError(err)
	input := PersonAttributeValueInput{PersonID: person, DefinitionSlug: def.Slug, Value: AttributeValue{Type: AttributeValueJSON, JSON: jsontext.Value("3456")}, Source: ProvenanceUser}
	first, err := st.SetPersonAttributeValueContext(t.Context(), input)
	require.NoError(err)
	participant, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)
	input.PersonID = absorbed.ID
	input.Value.JSON = jsontext.Value(`{"kind":"example"}`)
	second, err := st.SetPersonAttributeValueContext(t.Context(), input)
	require.NoError(err)
	organization, err := st.CreateOrganizationContext(t.Context(), OrganizationInput{Name: "Example Org"})
	require.NoError(err)
	orgDef := def
	orgDef.UniversalID = "test-org-json-sqlite-migration"
	orgDef.ObjectType = AttributeObjectOrganization
	_, err = st.CreateAttributeDefinitionContext(t.Context(), orgDef)
	require.NoError(err)
	_, err = st.SetOrganizationAttributeValueContext(t.Context(), OrganizationAttributeValueInput{OrganizationID: organization.ID, DefinitionSlug: orgDef.Slug, Value: AttributeValue{Type: AttributeValueJSON, JSON: jsontext.Value("7890")}, Source: ProvenanceUser})
	require.NoError(err)
	linkedOrganization, err := st.CreateOrganizationContext(t.Context(), OrganizationInput{Name: "Linked Org"})
	require.NoError(err)
	orgRefDef := AttributeDefinitionInput{
		UniversalID: "test-org-person-reference-sqlite-migration",
		ObjectType:  AttributeObjectOrganization, Slug: "related_person",
		Label: "Related person", ValueType: AttributeValueRecordReference,
		FieldType: AttributeFieldPerson, RecordTarget: new("person"),
		Cardinality: AttributeCardinalitySingle, Ownership: AttributeOwnershipUser,
		APIMutable: true, IsDeletable: true,
	}
	_, err = st.CreateAttributeDefinitionContext(t.Context(), orgRefDef)
	require.NoError(err)
	linkedValue, err := st.SetOrganizationAttributeValueContext(t.Context(), OrganizationAttributeValueInput{
		OrganizationID: linkedOrganization.ID, DefinitionSlug: orgRefDef.Slug,
		Value:  AttributeValue{Type: AttributeValueRecordReference, RecordType: new("person"), RecordID: new(absorbed.ID)},
		Source: ProvenanceUser,
	})
	require.NoError(err)
	survivor, err := st.GetPersonContext(t.Context(), person)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(t.Context(), PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "attribute-json-migration-merge", Actor: "test",
	})
	require.NoError(err)
	mergeID := merged.Merge.ID
	var currentRowID int64
	require.NoError(st.db.QueryRowContext(t.Context(), `SELECT current_row_id FROM person_merge_rows
		WHERE merge_id = ? AND table_name = 'organization_attribute_values' AND original_row_id = ?`,
		mergeID, linkedValue.Value.ID).Scan(&currentRowID))
	assert.Equal(linkedValue.Value.ID, currentRowID)
	var mergedRecordID int64
	require.NoError(st.db.QueryRowContext(t.Context(), `SELECT value_record_id
		FROM organization_attribute_values WHERE id = ?`, currentRowID).Scan(&mergedRecordID))
	assert.Equal(person, mergedRecordID)
	linkedOrganization, err = st.GetOrganizationContext(t.Context(), linkedOrganization.ID)
	require.NoError(err)
	require.NoError(st.DeleteOrganizationContext(t.Context(), linkedOrganization.ID, linkedOrganization.Revision))
	var oldRowExists bool
	require.NoError(st.db.QueryRowContext(t.Context(), `SELECT EXISTS (
		SELECT 1 FROM organization_attribute_values WHERE id = ?
	)`, linkedValue.Value.ID).Scan(&oldRowExists))
	assert.False(oldRowExists)
	var journalRowID int64
	require.NoError(st.db.QueryRowContext(t.Context(), `SELECT current_row_id FROM person_merge_rows
		WHERE merge_id = ? AND table_name = 'organization_attribute_values' AND original_row_id = ?`,
		mergeID, linkedValue.Value.ID).Scan(&journalRowID))
	assert.Equal(currentRowID, journalRowID)
	conn, err := st.db.Conn(t.Context())
	require.NoError(err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys = OFF`)
	require.NoError(err)
	for _, table := range []string{"person_attribute_values", "organization_attribute_values"} {
		var ddl string
		require.NoError(conn.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name = ?`, table).Scan(&ddl))
		ddl = strings.Replace(ddl, table+" (", table+"_old (", 1)
		ddl = strings.Replace(ddl, "value_json        TEXT", "value_json        JSON", 1)
		for _, sql := range []string{ddl, `INSERT INTO ` + table + `_old SELECT * FROM ` + table, `DROP TABLE ` + table, `ALTER TABLE ` + table + `_old RENAME TO ` + table} {
			_, err = conn.ExecContext(t.Context(), sql)
			require.NoError(err)
		}
	}
	// Deleted values may still be referenced by a merge journal.
	for _, table := range []string{"person_attribute_values", "organization_attribute_values"} {
		owner := "person_id"
		if table == "organization_attribute_values" {
			owner = "organization_id"
		}
		_, err = conn.ExecContext(t.Context(), `INSERT INTO `+table+` (id, `+owner+`, definition_id, ordinal, value_json, source)
			SELECT 100, `+owner+`, definition_id, 99, value_json, source FROM `+table+` LIMIT 1`)
		require.NoError(err)
		_, err = conn.ExecContext(t.Context(), `DELETE FROM `+table+` WHERE id = 100`)
		require.NoError(err)
	}
	_, err = conn.ExecContext(t.Context(), `UPDATE organization_attribute_values SET value_json = CAST(value_json AS BLOB) WHERE organization_id = ?`, organization.ID)
	require.NoError(err)
	for _, tc := range []struct {
		query string
		id    int64
		want  string
	}{
		{`SELECT typeof(value_json) FROM person_attribute_values WHERE id = ?`, first.Value.ID, "integer"},
		{`SELECT typeof(value_json) FROM person_attribute_values WHERE id = ?`, second.Value.ID, "text"},
		{`SELECT typeof(value_json) FROM organization_attribute_values WHERE organization_id = ?`, organization.ID, "blob"},
	} {
		var storage string
		require.NoError(conn.QueryRowContext(t.Context(), tc.query, tc.id).Scan(&storage))
		assert.Equal(tc.want, storage)
	}
	_, err = conn.ExecContext(t.Context(), `DELETE FROM applied_migrations WHERE name = 'attribute_value_json'`)
	require.NoError(err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys = ON`)
	require.NoError(err)
	require.NoError(conn.Close())
	path := st.dbPath
	require.NoError(st.Close())
	st, err = OpenForTest(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchemaContext(t.Context()))
	require.NoError(st.db.QueryRowContext(t.Context(), `SELECT current_row_id FROM person_merge_rows
		WHERE merge_id = ? AND table_name = 'organization_attribute_values' AND original_row_id = ?`,
		mergeID, linkedValue.Value.ID).Scan(&journalRowID))
	assert.Equal(currentRowID, journalRowID)

	newOrganization, err := st.CreateOrganizationContext(t.Context(), OrganizationInput{Name: "Replacement Org"})
	require.NoError(err)
	newValue, err := st.SetOrganizationAttributeValueContext(t.Context(), OrganizationAttributeValueInput{
		OrganizationID: newOrganization.ID, DefinitionSlug: orgRefDef.Slug,
		Value:  AttributeValue{Type: AttributeValueRecordReference, RecordType: new("person"), RecordID: new(person)},
		Source: ProvenanceUser,
	})
	require.NoError(err)
	assert.Greater(newValue.Value.ID, int64(100))
	assert.NotEqual(currentRowID, newValue.Value.ID)
	var survivorID, absorbedID int64
	var resolutionID sql.NullInt64
	require.NoError(st.db.QueryRowContext(t.Context(), `SELECT survivor_value_id, absorbed_value_id, resolution_value_id
		FROM person_merge_review_candidates WHERE merge_id = ? AND definition_id = ?`,
		mergeID, definition.ID).Scan(&survivorID, &absorbedID, &resolutionID))
	assert.Equal(first.Value.ID, survivorID)
	assert.Equal(second.Value.ID, absorbedID)
	assert.False(resolutionID.Valid)
	current, err := st.GetPersonContext(t.Context(), merged.Person.ID)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: mergeID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: current.Revision,
		IdempotencyKey:         "attribute-json-migration-split", Actor: "test",
	})
	require.NoError(err)
	require.NotNil(split)
	newValues, err := st.ListOrganizationAttributeValuesContext(t.Context(), newOrganization.ID,
		OrganizationAttributeQuery{DefinitionSlug: orgRefDef.Slug})
	require.NoError(err)
	require.Len(newValues, 1)
	assert.Equal(newValue.Value.ID, newValues[0].ID)
	require.NotNil(newValues[0].Value.RecordType)
	require.NotNil(newValues[0].Value.RecordID)
	assert.Equal("person", *newValues[0].Value.RecordType)
	assert.Equal(person, *newValues[0].Value.RecordID)

	for _, table := range []string{"person_attribute_values", "organization_attribute_values"} {
		owner := "person_id"
		if table == "organization_attribute_values" {
			owner = "organization_id"
		}
		result, err := st.db.ExecContext(t.Context(), `INSERT INTO `+table+` (`+owner+`, definition_id, ordinal, value_json, source)
			SELECT `+owner+`, definition_id, 99, value_json, source FROM `+table+` LIMIT 1`)
		require.NoError(err)
		id, err := result.LastInsertId()
		require.NoError(err)
		assert.Greater(id, int64(100), table)
		_, err = st.db.ExecContext(t.Context(), `DELETE FROM `+table+` WHERE id = ?`, id)
		require.NoError(err)
	}
	var storage string
	var value int
	require.NoError(st.db.QueryRowContext(t.Context(), `SELECT typeof(value_json), json_extract(value_json, '$') FROM person_attribute_values WHERE id = ?`, first.Value.ID).Scan(&storage, &value))
	assert.Equal("text", storage)
	assert.Equal(3456, value)
	require.NoError(st.db.QueryRowContext(t.Context(), `SELECT typeof(value_json), json_extract(value_json, '$') FROM organization_attribute_values WHERE organization_id = ?`, organization.ID).Scan(&storage, &value))
	assert.Equal("text", storage)
	assert.Equal(7890, value)
	var kind string
	require.NoError(st.db.QueryRowContext(t.Context(), `SELECT typeof(value_json), json_extract(value_json, '$.kind') FROM person_attribute_values WHERE id = ?`, second.Value.ID).Scan(&storage, &kind))
	assert.Equal("text", storage)
	assert.Equal("example", kind)
	input.PersonID = person
	input.Value.JSON = jsontext.Value("1e2")
	_, err = st.SetPersonAttributeValueContext(t.Context(), input)
	require.NoError(err)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person, PersonAttributeQuery{DefinitionSlug: def.Slug})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("1e2", string(values[0].Value.JSON))
}

func TestAttributeValueJSONMigration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if os.Getenv("MSGVAULT_TEST_DB") == "" {
		t.Skip("requires PostgreSQL")
	}
	st, person, _ := newPersonFactProjectionStore(t)
	require.True(st.IsPostgreSQL())
	def := AttributeDefinitionInput{UniversalID: "test-json-migration", ObjectType: AttributeObjectPerson, Slug: "json_migration", Label: "JSON migration", ValueType: AttributeValueJSON, FieldType: AttributeFieldJSON, Cardinality: AttributeCardinalitySingle, Ownership: AttributeOwnershipUser, APIMutable: true, IsDeletable: true}
	_, err := st.CreateAttributeDefinitionContext(t.Context(), def)
	require.NoError(err)
	input := PersonAttributeValueInput{PersonID: person, DefinitionSlug: def.Slug, Value: AttributeValue{Type: AttributeValueJSON, JSON: jsontext.Value("1e2")}, Source: ProvenanceUser}
	_, err = st.SetPersonAttributeValueContext(t.Context(), input)
	require.NoError(err)
	for _, table := range []string{"person_attribute_values", "organization_attribute_values"} {
		_, err = st.db.ExecContext(t.Context(), `ALTER TABLE `+table+` ALTER COLUMN value_json TYPE JSONB USING value_json::JSONB`)
		require.NoError(err)
	}
	_, err = st.db.ExecContext(t.Context(), `DELETE FROM applied_migrations WHERE name = ?`, "attribute_value_json")
	require.NoError(err)
	// JSONB normalized the old value; new writes retain their original token.
	require.NoError(st.InitSchemaContext(t.Context()))
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person, PersonAttributeQuery{DefinitionSlug: def.Slug})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("100", string(values[0].Value.JSON))
	_, err = st.SetPersonAttributeValueContext(t.Context(), input)
	require.NoError(err)
	values, err = st.ListPersonAttributeValuesContext(t.Context(), person, PersonAttributeQuery{DefinitionSlug: def.Slug})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("1e2", string(values[0].Value.JSON))
	for _, table := range []string{"person_attribute_values", "organization_attribute_values"} {
		var dataType string
		require.NoError(st.db.QueryRowContext(t.Context(), `SELECT data_type FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND column_name = 'value_json'`, table).Scan(&dataType))
		assert.Equal("json", dataType)
	}
}
