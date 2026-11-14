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
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		t.Skip("requires SQLite")
	}
	st, person, _ := newPersonFactProjectionStore(t)
	def := AttributeDefinitionInput{UniversalID: "test-json-sqlite-migration", ObjectType: AttributeObjectPerson, Slug: "json_sqlite_migration", Label: "JSON migration", ValueType: AttributeValueJSON, FieldType: AttributeFieldJSON, Cardinality: AttributeCardinalitySingle, Ownership: AttributeOwnershipUser, APIMutable: true, IsDeletable: true}
	_, err := st.CreateAttributeDefinitionContext(t.Context(), def)
	require.NoError(t, err)
	input := PersonAttributeValueInput{PersonID: person, DefinitionSlug: def.Slug, Value: AttributeValue{Type: AttributeValueJSON, JSON: jsontext.Value("3456")}, Source: ProvenanceUser}
	_, err = st.SetPersonAttributeValueContext(t.Context(), input)
	require.NoError(t, err)
	organization, err := st.CreateOrganizationContext(t.Context(), OrganizationInput{Name: "Example Org"})
	require.NoError(t, err)
	orgDef := def
	orgDef.UniversalID = "test-org-json-sqlite-migration"
	orgDef.ObjectType = AttributeObjectOrganization
	_, err = st.CreateAttributeDefinitionContext(t.Context(), orgDef)
	require.NoError(t, err)
	_, err = st.SetOrganizationAttributeValueContext(t.Context(), OrganizationAttributeValueInput{OrganizationID: organization.ID, DefinitionSlug: orgDef.Slug, Value: AttributeValue{Type: AttributeValueJSON, JSON: jsontext.Value("7890")}, Source: ProvenanceUser})
	require.NoError(t, err)
	conn, err := st.db.Conn(t.Context())
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	for _, table := range []string{"person_attribute_values", "organization_attribute_values"} {
		var ddl string
		require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name = ?`, table).Scan(&ddl))
		ddl = strings.Replace(ddl, table+" (", table+"_old (", 1)
		ddl = strings.Replace(ddl, "value_json        TEXT", "value_json        JSON", 1)
		for _, sql := range []string{ddl, `INSERT INTO ` + table + `_old SELECT * FROM ` + table, `UPDATE ` + table + `_old SET value_json = CAST(value_json AS BLOB) WHERE value_json IS NOT NULL`, `DROP TABLE ` + table, `ALTER TABLE ` + table + `_old RENAME TO ` + table} {
			_, err = conn.ExecContext(t.Context(), sql)
			require.NoError(t, err)
		}
	}
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys = ON`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.NoError(t, st.migrateAttributeValueJSONSQLite(t.Context()))
	require.NoError(t, st.migrateAttributeValueJSONSQLite(t.Context()))
	var storage string
	var value int
	require.NoError(t, st.db.QueryRowContext(t.Context(), `SELECT typeof(value_json), json_extract(value_json, '$') FROM person_attribute_values WHERE person_id = ?`, person).Scan(&storage, &value))
	assert.Equal(t, "text", storage)
	assert.Equal(t, 3456, value)
	require.NoError(t, st.db.QueryRowContext(t.Context(), `SELECT typeof(value_json), json_extract(value_json, '$') FROM organization_attribute_values WHERE organization_id = ?`, organization.ID).Scan(&storage, &value))
	assert.Equal(t, "text", storage)
	assert.Equal(t, 7890, value)
	input.Value.JSON = jsontext.Value("1e2")
	_, err = st.SetPersonAttributeValueContext(t.Context(), input)
	require.NoError(t, err)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person, PersonAttributeQuery{DefinitionSlug: def.Slug})
	require.NoError(t, err)
	require.Len(t, values, 1)
	assert.Equal(t, "1e2", string(values[0].Value.JSON))
}

func TestAttributeValueJSONMigration(t *testing.T) {
	if os.Getenv("MSGVAULT_TEST_DB") == "" {
		t.Skip("requires PostgreSQL")
	}
	st, person, _ := newPersonFactProjectionStore(t)
	require.True(t, st.IsPostgreSQL())
	def := AttributeDefinitionInput{UniversalID: "test-json-migration", ObjectType: AttributeObjectPerson, Slug: "json_migration", Label: "JSON migration", ValueType: AttributeValueJSON, FieldType: AttributeFieldJSON, Cardinality: AttributeCardinalitySingle, Ownership: AttributeOwnershipUser, APIMutable: true, IsDeletable: true}
	_, err := st.CreateAttributeDefinitionContext(t.Context(), def)
	require.NoError(t, err)
	input := PersonAttributeValueInput{PersonID: person, DefinitionSlug: def.Slug, Value: AttributeValue{Type: AttributeValueJSON, JSON: jsontext.Value("1e2")}, Source: ProvenanceUser}
	_, err = st.SetPersonAttributeValueContext(t.Context(), input)
	require.NoError(t, err)
	for _, table := range []string{"person_attribute_values", "organization_attribute_values"} {
		_, err = st.db.ExecContext(t.Context(), `ALTER TABLE `+table+` ALTER COLUMN value_json TYPE JSONB USING value_json::JSONB`)
		require.NoError(t, err)
	}
	_, err = st.db.ExecContext(t.Context(), `DELETE FROM applied_migrations WHERE name = ?`, "attribute_value_json")
	require.NoError(t, err)
	// The first open converts JSONB's normalized 100; a second open is a no-op.
	for _, want := range []string{"100", "1e2"} {
		require.NoError(t, st.InitSchemaContext(t.Context()))
		values, err := st.ListPersonAttributeValuesContext(t.Context(), person, PersonAttributeQuery{DefinitionSlug: def.Slug})
		require.NoError(t, err)
		require.Len(t, values, 1)
		assert.Equal(t, want, string(values[0].Value.JSON))
		_, err = st.SetPersonAttributeValueContext(t.Context(), input)
		require.NoError(t, err)
	}
	for _, table := range []string{"person_attribute_values", "organization_attribute_values"} {
		var dataType string
		require.NoError(t, st.db.QueryRowContext(t.Context(), `SELECT data_type FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND column_name = 'value_json'`, table).Scan(&dataType))
		assert.Equal(t, "json", dataType)
	}
}
