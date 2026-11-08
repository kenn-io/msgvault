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

func TestImportantDateSubjectUpdateAndWithdrawal(t *testing.T) {
	st := storetest.New(t).Store
	ctx := t.Context()
	id := newTestPerson(t, st)
	person, err := st.GetPerson(id)
	require.NoError(t, err)
	subject, err := st.CreateImportantDateSubjectContext(ctx, store.ImportantDateSubjectInput{OwnerUID: person.VCardUID, Kind: "dependent", DisplayName: "Example child", Date: store.PartialDate{Month: new(10), Day: new(23)}})
	require.NoError(t, err)
	require.NoError(t, st.SetImportantDateConsentContext(ctx, "owned/"+subject.UID, "private-example", "owner/agent", true))
	changed, err := st.UpdateImportantDateSubjectContext(ctx, subject.UID, subject.Revision, "Example child", store.PartialDate{Month: new(11), Day: new(5)}, false)
	require.NoError(t, err)
	assert.True(t, changed)
	active, err := st.HasImportantDateConsentContext(ctx, "owned/"+subject.UID, "private-example", "owner/agent")
	require.NoError(t, err)
	assert.False(t, active)
	changed, err = st.UpdateImportantDateSubjectContext(ctx, subject.UID, subject.Revision, "Example child", store.PartialDate{Month: new(11), Day: new(6)}, false)
	require.NoError(t, err)
	assert.False(t, changed)
	changed, err = st.UpdateImportantDateSubjectContext(ctx, subject.UID, subject.Revision+1, "", store.PartialDate{}, true)
	require.NoError(t, err)
	assert.True(t, changed)
	items, err := st.ListImportantDateSubjectsContext(ctx, person.VCardUID)
	require.NoError(t, err)
	assert.Empty(t, items)
}
