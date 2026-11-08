package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportantDateConsentIsBoundToDestination(t *testing.T) {
	st := storetest.New(t).Store
	ctx := t.Context()
	personID := newTestPerson(t, st)
	person, err := st.GetPerson(personID)
	require.NoError(t, err)
	subject := "person/" + person.VCardUID
	active, err := st.HasImportantDateConsentContext(ctx, subject, "private-example", "owner/agent")
	require.NoError(t, err)
	assert.False(t, active)
	require.NoError(t, st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", true))
	active, err = st.HasImportantDateConsentContext(ctx, subject, "private-example", "owner/agent")
	require.NoError(t, err)
	assert.True(t, active)
	active, err = st.HasImportantDateConsentContext(ctx, subject, "other-project", "owner/agent")
	require.NoError(t, err)
	assert.False(t, active)
	active, err = st.HasImportantDateConsentContext(ctx, subject, "private-example", "other-agent")
	require.NoError(t, err)
	assert.False(t, active)
	require.NoError(t, st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", false))
	active, err = st.HasImportantDateConsentContext(ctx, subject, "private-example", "owner/agent")
	require.NoError(t, err)
	assert.False(t, active)
}

func TestImportantDateConsentDispatchRequiresGrant(t *testing.T) {
	st := storetest.New(t).Store
	ctx := t.Context()
	personID := newTestPerson(t, st)
	person, err := st.GetPerson(personID)
	require.NoError(t, err)
	subject := "person/" + person.VCardUID
	calls := 0
	dispatch := func(fenced context.Context) error {
		active, err := st.HasImportantDateConsentContext(fenced, subject, "private-example", "owner/agent")
		require.NoError(t, err)
		assert.True(t, active)
		calls++
		return nil
	}
	active, err := st.WithImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", dispatch)
	require.NoError(t, err)
	assert.False(t, active)
	assert.Zero(t, calls)
	require.NoError(t, st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", true))
	active, err = st.WithImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", dispatch)
	require.NoError(t, err)
	assert.True(t, active)
	assert.Equal(t, 1, calls)
}

func TestImportantDateConsentRefusesUnknownSubject(t *testing.T) {
	st := storetest.New(t).Store
	err := st.SetImportantDateConsentContext(t.Context(), "person/missing-example", "private-example", "owner/agent", true)
	assert.Error(t, err)
}
