package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportantDateConsentIsBoundToDestination(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	personID := newTestPerson(t, st)
	person, err := st.GetPerson(personID)
	must.NoError(err)
	subject := "person/" + person.VCardUID
	active, err := st.HasImportantDateConsentContext(ctx, subject, "private-example", "owner/agent")
	must.NoError(err)
	checks.False(active)
	must.NoError(st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", true))
	active, err = st.HasImportantDateConsentContext(ctx, subject, "private-example", "owner/agent")
	must.NoError(err)
	checks.True(active)
	active, err = st.HasImportantDateConsentContext(ctx, subject, "other-project", "owner/agent")
	must.NoError(err)
	checks.False(active)
	active, err = st.HasImportantDateConsentContext(ctx, subject, "private-example", "other-agent")
	must.NoError(err)
	checks.False(active)
	must.NoError(st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", false))
	active, err = st.HasImportantDateConsentContext(ctx, subject, "private-example", "owner/agent")
	must.NoError(err)
	checks.False(active)
}

func TestImportantDateConsentDispatchRequiresGrant(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	personID := newTestPerson(t, st)
	person, err := st.GetPerson(personID)
	must.NoError(err)
	subject := "person/" + person.VCardUID
	calls := 0
	dispatch := func(fenced context.Context) error {
		active, err := st.HasImportantDateConsentContext(fenced, subject, "private-example", "owner/agent")
		must.NoError(err)
		checks.True(active)
		calls++
		return nil
	}
	active, err := st.WithImportantDateConsentContext(ctx, subject, subject, "private-example", "owner/agent", dispatch)
	must.NoError(err)
	checks.False(active)
	checks.Zero(calls)
	must.NoError(st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", true))
	active, err = st.WithImportantDateConsentContext(ctx, subject, subject, "private-example", "owner/agent", dispatch)
	must.NoError(err)
	checks.True(active)
	checks.Equal(1, calls)
}

func TestImportantDateConsentRefusesUnknownSubject(t *testing.T) {
	checks := assert.New(t)
	st := storetest.New(t).Store
	err := st.SetImportantDateConsentContext(t.Context(), "person/missing-example", "private-example", "owner/agent", true)
	checks.Error(err)
}
