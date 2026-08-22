package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAppendPersonNoteCreatesAndHistorizesOrdinalZero(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createNotesPerson(t, st, "notes-history@example.test")

	first, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{
		PersonID: person.ID, Text: "first fact", Source: store.ProvenanceUser,
	})
	require.NoError(err)
	require.NotNil(first.Value)
	assert.Zero(first.Value.Ordinal)
	require.NotNil(first.Value.Value.Text)
	assert.Equal("first fact", *first.Value.Value.Text)
	assert.Nil(first.Superseded)

	second, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{
		PersonID: person.ID, Text: "second fact", Source: store.ProvenanceUser,
	})
	require.NoError(err)
	require.NotNil(second.Value)
	require.NotNil(second.Value.Value.Text)
	assert.Equal("first fact\nsecond fact", *second.Value.Value.Text)
	require.NotNil(second.Superseded)
	assert.Equal(first.Value.ID, second.Superseded.ID)

	history, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID,
		store.PersonAttributeQuery{
			DefinitionSlug: store.AttributeSlugNotes, IncludeHistory: true,
		})
	require.NoError(err)
	assert.Len(history, 2)
}

func TestAppendPersonNoteSerializesConcurrentFragments(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createNotesPerson(t, st, "notes-concurrent@example.test")
	fragments := []string{"alpha fragment", "beta fragment"}
	errs := make(chan error, len(fragments))

	for _, fragment := range fragments {
		go func() {
			_, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{
				PersonID: person.ID, Text: fragment, Source: store.ProvenanceUser,
			})
			errs <- err
		}()
	}
	for range fragments {
		require.NoError(<-errs)
	}

	values, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID,
		store.PersonAttributeQuery{
			DefinitionSlug: store.AttributeSlugNotes, IncludeHistory: true,
		})
	require.NoError(err)
	require.Len(values, 2)
	current := values[0]
	require.Nil(current.SupersededAt)
	require.NotNil(current.Value.Text)
	assert.Contains([]string{
		"alpha fragment\nbeta fragment",
		"beta fragment\nalpha fragment",
	}, *current.Value.Text)
	for _, fragment := range fragments {
		assert.Equal(1, strings.Count(*current.Value.Text, fragment))
	}
}

func TestAppendPersonNoteRejectsBlankAndMissingPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createNotesPerson(t, st, "notes-errors@example.test")

	_, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{
		PersonID: person.ID, Text: " \n\t ", Source: store.ProvenanceUser,
	})
	assert.ErrorIs(err, store.ErrAttributeValueInvalid)
	values, listErr := st.ListPersonAttributeValuesContext(t.Context(), person.ID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugNotes, IncludeHistory: true})
	require.NoError(listErr)
	assert.Empty(values)

	_, err = st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{
		PersonID: 999_999, Text: "missing", Source: store.ProvenanceUser,
	})
	assert.ErrorIs(err, store.ErrPersonNotFound)
}

func TestAppendPersonNoteDryRunPreviewsWithoutWriting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createNotesPerson(t, st, "notes-dry-run@example.test")
	first, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{
		PersonID: person.ID, Text: "stored fact", Source: store.ProvenanceUser,
	})
	require.NoError(err)

	preview, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{
		PersonID: person.ID, Text: "preview fact", Source: store.ProvenanceUser, DryRun: true,
	})
	require.NoError(err)
	assert.True(preview.DryRun)
	require.NotNil(preview.Value)
	assert.Zero(preview.Value.ID)
	require.NotNil(preview.Value.Value.Text)
	assert.Equal("stored fact\npreview fact", *preview.Value.Value.Text)
	require.NotNil(preview.Superseded)
	assert.Equal(first.Value.ID, preview.Superseded.ID)

	values, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID,
		store.PersonAttributeQuery{
			DefinitionSlug: store.AttributeSlugNotes, IncludeHistory: true,
		})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal(first.Value.ID, values[0].ID)
	require.NotNil(values[0].Value.Text)
	assert.Equal("stored fact", *values[0].Value.Text)
}

func createNotesPerson(
	t *testing.T, st *store.Store, address string,
) *store.Person {
	t.Helper()
	participantID, err := st.EnsureParticipant(address, "Notes Person", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	return person
}
