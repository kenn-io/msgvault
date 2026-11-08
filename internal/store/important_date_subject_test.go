package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportantDateSubjectExplicitRecord(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	personID := newTestPerson(t, st)
	person, err := st.GetPerson(personID)
	must.NoError(err)
	input := store.ImportantDateSubjectInput{OwnerUID: person.VCardUID, Kind: "pet", DisplayName: "Example pet", Date: store.PartialDate{Month: new(10), Day: new(23)}}
	created, err := st.CreateImportantDateSubjectContext(t.Context(), input)
	must.NoError(err)
	checks.NotEmpty(created.UID)
	checks.Equal(input.Date, created.Date)
	items, err := st.ListImportantDateSubjectsContext(t.Context(), person.VCardUID)
	must.NoError(err)
	must.Len(items, 1)
	checks.Equal(created.UID, items[0].UID)
	input.Date.Day = nil
	_, err = st.CreateImportantDateSubjectContext(t.Context(), input)
	checks.Error(err)
}

func TestImportantDateSubjectUpdateAndWithdrawal(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	id := newTestPerson(t, st)
	person, err := st.GetPerson(id)
	must.NoError(err)
	subject, err := st.CreateImportantDateSubjectContext(ctx, store.ImportantDateSubjectInput{OwnerUID: person.VCardUID, Kind: "dependent", DisplayName: "Example child", Date: store.PartialDate{Month: new(10), Day: new(23)}})
	must.NoError(err)
	must.NoError(st.SetImportantDateConsentContext(ctx, "owned/"+subject.UID, "private-example", "owner/agent", true))
	changed, err := st.UpdateImportantDateSubjectContext(ctx, subject.UID, subject.Revision, "Example child", store.PartialDate{Month: new(11), Day: new(5)}, false)
	must.NoError(err)
	checks.True(changed)
	active, err := st.HasImportantDateConsentContext(ctx, "owned/"+subject.UID, "private-example", "owner/agent")
	must.NoError(err)
	checks.False(active)
	changed, err = st.UpdateImportantDateSubjectContext(ctx, subject.UID, subject.Revision, "Example child", store.PartialDate{Month: new(11), Day: new(6)}, false)
	must.NoError(err)
	checks.False(changed)
	changed, err = st.UpdateImportantDateSubjectContext(ctx, subject.UID, subject.Revision+1, "", store.PartialDate{}, true)
	must.NoError(err)
	checks.True(changed)
	items, err := st.ListImportantDateSubjectsContext(ctx, person.VCardUID)
	must.NoError(err)
	checks.Empty(items)
}
