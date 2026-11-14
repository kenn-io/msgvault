package store

import (
	"encoding/json/jsontext"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
