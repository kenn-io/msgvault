package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirectAttributeAdvancesInferenceExportRevisionOnlyForChangedPortableProjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	value := "inferred note"

	input := PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: AttributeSlugNotes,
		Value:  AttributeValue{Type: AttributeValueText, Text: &value},
		Source: ProvenanceExtraction,
	}
	_, err := st.SetPersonAttributeValueContext(t.Context(), input)
	require.NoError(err)

	var firstRevision int64
	err = st.db.QueryRow(`SELECT inference_revision
		FROM person_carddav_inference_state WHERE person_id = ?`, personID).Scan(&firstRevision)
	require.NoError(err)
	assert.Equal(int64(1), firstRevision)

	_, err = st.SetPersonAttributeValueContext(t.Context(), input)
	require.NoError(err)

	var replayRevision int64
	err = st.db.QueryRow(`SELECT inference_revision
		FROM person_carddav_inference_state WHERE person_id = ?`, personID).Scan(&replayRevision)
	require.NoError(err)
	assert.Equal(firstRevision, replayRevision)
}

func TestInferenceExportRevisionIgnoresSystemAndNonportableAttributeWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	value := "chat"

	_, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
		Value: AttributeValue{Type: AttributeValueText, Text: &value}, Source: ProvenanceExtraction,
	})
	require.NoError(err)
	_, err = st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: AttributeSlugNotes,
		Value: AttributeValue{Type: AttributeValueText, Text: &value}, Source: ProvenanceSystem,
	})
	require.NoError(err)

	var revision int64
	err = st.db.QueryRow(`SELECT inference_revision FROM person_carddav_inference_state
		WHERE person_id = ?`, personID).Scan(&revision)
	assert.ErrorIs(err, sql.ErrNoRows)
}

func TestInferenceExportRevisionRollsBackWithFailedAttributeWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	value := "discarded inferred note"

	_, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: AttributeSlugNotes,
		Value:  AttributeValue{Type: AttributeValueText, Text: &value},
		Source: ProvenanceExtraction, DryRun: true,
	})
	require.NoError(err)

	var revision int64
	err = st.db.QueryRow(`SELECT inference_revision FROM person_carddav_inference_state
		WHERE person_id = ?`, personID).Scan(&revision)
	assert.ErrorIs(err, sql.ErrNoRows)
}
