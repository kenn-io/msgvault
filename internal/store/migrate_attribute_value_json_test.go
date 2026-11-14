package store

import (
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
	merge, err := st.db.ExecContext(t.Context(), `INSERT INTO person_merges
		(idempotency_key, request_hash, survivor_person_id_at_merge, absorbed_person_id, current_person_id,
		 survivor_uid, absorbed_uid, survivor_revision_before, absorbed_revision_before, survivor_revision_after,
		 actor, snapshot_version, snapshot_blob, snapshot_sha256)
		VALUES ('migration-test', ?, ?, ?, ?, 'survivor', 'absorbed', 1, 1, 2, 'test', 1, X'00', ?)`,
		strings.Repeat("a", 64), person, absorbed.ID, person, strings.Repeat("b", 64))
	require.NoError(err)
	mergeID, err := merge.LastInsertId()
	require.NoError(err)
	_, err = st.db.ExecContext(t.Context(), `INSERT INTO person_merge_review_candidates
		(merge_id, survivor_person_id, definition_id, survivor_value_id, absorbed_value_id, resolution_value_id)
		VALUES (?, ?, ?, ?, ?, ?)`, mergeID, person, definition.ID, first.Value.ID, second.Value.ID, second.Value.ID)
	require.NoError(err)
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
	require.NoError(st.InitSchemaContext(t.Context()))
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
	var survivorID, absorbedID, resolutionID int64
	require.NoError(st.db.QueryRowContext(t.Context(), `SELECT survivor_value_id, absorbed_value_id, resolution_value_id FROM person_merge_review_candidates WHERE merge_id = ?`, mergeID).Scan(&survivorID, &absorbedID, &resolutionID))
	assert.Equal(first.Value.ID, survivorID)
	assert.Equal(second.Value.ID, absorbedID)
	assert.Equal(second.Value.ID, resolutionID)
	var violations int
	require.NoError(st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_foreign_key_check('person_merge_review_candidates')`).Scan(&violations))
	assert.Zero(violations)
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
	// The first open converts JSONB's normalized 100; a second open is a no-op.
	for _, want := range []string{"100", "1e2"} {
		require.NoError(st.InitSchemaContext(t.Context()))
		values, err := st.ListPersonAttributeValuesContext(t.Context(), person, PersonAttributeQuery{DefinitionSlug: def.Slug})
		require.NoError(err)
		require.Len(values, 1)
		assert.Equal(want, string(values[0].Value.JSON))
		_, err = st.SetPersonAttributeValueContext(t.Context(), input)
		require.NoError(err)
	}
	for _, table := range []string{"person_attribute_values", "organization_attribute_values"} {
		var dataType string
		require.NoError(st.db.QueryRowContext(t.Context(), `SELECT data_type FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND column_name = 'value_json'`, table).Scan(&dataType))
		assert.Equal("json", dataType)
	}
}
