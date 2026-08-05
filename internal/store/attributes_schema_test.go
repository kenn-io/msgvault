package store_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func insertRawDefinition(t *testing.T, st *store.Store, slug string) int64 {
	t.Helper()
	var id int64
	err := st.DB().QueryRow(st.Rebind(`
		INSERT INTO attribute_definitions
		    (universal_id, object_type, slug, label, value_type, field_type)
		VALUES (?, 'person', ?, ?, 'text', 'text')
		RETURNING id
	`), "uid-"+slug, slug, slug).Scan(&id)
	require.NoError(t, err)
	return id
}

func insertRawValue(
	t *testing.T, st *store.Store, personID, definitionID int64, columns string, args ...any,
) error {
	t.Helper()
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
	_, err := st.DB().Exec(st.Rebind(fmt.Sprintf(`
		INSERT INTO person_attribute_values
		    (person_id, definition_id, source, %s)
		VALUES (?, ?, 'user', %s)
	`, columns, placeholders)), append([]any{personID, definitionID}, args...)...)
	return err
}

func TestAttributeSchemaRejectsMultipleTypedValueColumns(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := mustSchemaTestPerson(t, st)
	definition := insertRawDefinition(t, st, "one_value_only")

	err := insertRawValue(t, st, person, definition,
		"value_text, value_integer", "alice", 7)
	require.Error(t, err)

	err = insertRawValue(t, st, person, definition, "value_text", "alice")
	require.NoError(t, err)
}

func TestAttributeSchemaRejectsNoTypedValueColumn(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := mustSchemaTestPerson(t, st)
	definition := insertRawDefinition(t, st, "needs_a_value")

	_, err := st.DB().Exec(st.Rebind(`
		INSERT INTO person_attribute_values (person_id, definition_id, source)
		VALUES (?, ?, 'user')
	`), person, definition)
	require.Error(t, err)
}

func TestAttributeSchemaRequiresBothRecordReferenceHalves(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := mustSchemaTestPerson(t, st)
	definition := insertRawDefinition(t, st, "record_ref")

	err := insertRawValue(t, st, person, definition, "value_record_id", person)
	require.Error(t, err)

	err = insertRawValue(t, st, person, definition,
		"value_record_id, value_record_type", person, "person")
	require.NoError(t, err)
}

func TestAttributeSchemaRejectsOutOfRangeConfidenceAndOrdinal(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := mustSchemaTestPerson(t, st)
	definition := insertRawDefinition(t, st, "bounds")

	err := insertRawValue(t, st, person, definition,
		"value_text, confidence", "alice", 1.5)
	require.Error(t, err)

	err = insertRawValue(t, st, person, definition,
		"value_text, ordinal", "alice", -1)
	require.Error(t, err)
}

func TestAttributeSchemaPinsValueDateSeparatorPositions(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := mustSchemaTestPerson(t, st)
	definition := insertRawDefinition(t, st, "date_shape")

	for ordinal, accepted := range []string{"2026-07-30", "abcd-ef-gh"} {
		err := insertRawValue(t, st, person, definition,
			"value_date, ordinal", accepted, ordinal)
		require.NoError(t, err,
			"%q is positionally valid, so the database accepts it", accepted)
	}

	for _, rejected := range []string{
		"abcdefghij", "2026-7-30", "20260730", "2026-07-30T00:00:00Z",
	} {
		err := insertRawValue(t, st, person, definition, "value_date", rejected)
		require.Error(t, err, "%q must violate the separator-position CHECK", rejected)
	}
}

func TestAttributeSchemaRejectsInvertedActiveInterval(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := mustSchemaTestPerson(t, st)
	definition := insertRawDefinition(t, st, "interval")

	from := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	err := insertRawValue(t, st, person, definition,
		"value_text, active_from, active_until", "alice", from, from.Add(-time.Hour))
	require.Error(t, err)

	err = insertRawValue(t, st, person, definition,
		"value_text, active_from, active_until", "alice", from, from.Add(time.Hour))
	require.NoError(t, err)
}

func TestAttributeSchemaAllowsOnlyOneCurrentValuePerOrdinal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := mustSchemaTestPerson(t, st)
	definition := insertRawDefinition(t, st, "single_current")
	from := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	require.NoError(insertRawValue(t, st, person, definition,
		"value_text, ordinal, active_from", "first", 0, from))

	err := insertRawValue(t, st, person, definition,
		"value_text, ordinal, active_from", "second", 0, from.Add(time.Hour))
	require.Error(err, "two active rows at the same ordinal must violate the unique index")

	_, err = st.DB().Exec(st.Rebind(`
		UPDATE person_attribute_values
		SET active_until = ?, superseded_at = ?
		WHERE person_id = ? AND definition_id = ? AND ordinal = 0 AND active_until IS NULL
	`), from.Add(time.Hour), from.Add(time.Hour), person, definition)
	require.NoError(err)

	require.NoError(insertRawValue(t, st, person, definition,
		"value_text, ordinal, active_from", "second", 0, from.Add(time.Hour)))

	var historyCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM person_attribute_values
		WHERE person_id = ? AND definition_id = ?
	`), person, definition).Scan(&historyCount))
	assert.Equal(2, historyCount, "supersede must preserve the historical row")

	require.NoError(insertRawValue(t, st, person, definition,
		"value_text, ordinal, active_from", "other slot", 1, from.Add(time.Hour)))
}

func TestAttributeSchemaAcceptsExactlyTheProvenanceVocabulary(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := mustSchemaTestPerson(t, st)
	definition := insertRawDefinition(t, st, "provenance_probe")

	for ordinal, provenance := range store.AllProvenances {
		_, err := st.DB().Exec(st.Rebind(`
			INSERT INTO person_attribute_values
			    (person_id, definition_id, ordinal, value_text, source)
			VALUES (?, ?, ?, ?, ?)
		`), person, definition, ordinal, "alice", string(provenance))
		require.NoError(t, err, "provenance %q must be accepted", provenance)
	}

	_, err := st.DB().Exec(st.Rebind(`
		INSERT INTO person_attribute_values
		    (person_id, definition_id, ordinal, value_text, source)
		VALUES (?, ?, ?, ?, ?)
	`), person, definition, len(store.AllProvenances), "alice", "guessed")
	require.Error(t, err, "a source outside the vocabulary must be rejected")
}

func TestAttributeSchemaRefusesDeletingADefinitionThatHasValues(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := mustSchemaTestPerson(t, st)
	definition := insertRawDefinition(t, st, "restricted")
	require.NoError(t, insertRawValue(t, st, person, definition, "value_text", "alice"))

	_, err := st.DB().Exec(st.Rebind(
		`DELETE FROM attribute_definitions WHERE id = ?`), definition)
	require.Error(t, err, "ON DELETE RESTRICT must refuse while values exist")
}

func TestAttributeSchemaCascadesValuesWhenAPersonIsDeleted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := mustSchemaTestPerson(t, st)
	definition := insertRawDefinition(t, st, "cascades")
	require.NoError(insertRawValue(t, st, person, definition, "value_text", "alice"))

	var revision int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT revision FROM persons WHERE id = ?`), person).Scan(&revision))
	require.NoError(st.DeletePerson(person, revision))

	var remaining int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM person_attribute_values WHERE person_id = ?`),
		person).Scan(&remaining))
	assert.Equal(0, remaining)
}

func mustSchemaTestPerson(t *testing.T, st *store.Store) int64 {
	t.Helper()
	participant, err := st.EnsureParticipant("alice@example.com", "alice", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	return person.ID
}
