package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportantDateSubjectExplicitRecord(t *testing.T) {
	st := storetest.New(t).Store
	personID := newTestPerson(t, st)
	person, err := st.GetPerson(personID)
	require.NoError(t, err)
	input := store.ImportantDateSubjectInput{OwnerUID: person.VCardUID, Kind: "pet", DisplayName: "Example pet", Date: store.PartialDate{Month: new(10), Day: new(23)}}
	created, err := st.CreateImportantDateSubjectContext(t.Context(), input)
	require.NoError(t, err)
	assert.NotEmpty(t, created.UID)
	assert.Equal(t, input.Date, created.Date)
	items, err := st.ListImportantDateSubjectsContext(t.Context(), person.VCardUID)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, created.UID, items[0].UID)
	input.Date.Day = nil
	_, err = st.CreateImportantDateSubjectContext(t.Context(), input)
	assert.Error(t, err)
}
