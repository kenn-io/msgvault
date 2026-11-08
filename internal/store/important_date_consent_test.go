package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportantDateConsentIsBoundToDestination(t *testing.T) {
	st := storetest.New(t).Store
	ctx := t.Context()
	active, err := st.HasImportantDateConsentContext(ctx, "subject-example", "private-example", "owner/agent")
	require.NoError(t, err)
	assert.False(t, active)
	require.NoError(t, st.SetImportantDateConsentContext(ctx, "subject-example", "private-example", "owner/agent", true))
	active, err = st.HasImportantDateConsentContext(ctx, "subject-example", "private-example", "owner/agent")
	require.NoError(t, err)
	assert.True(t, active)
	active, err = st.HasImportantDateConsentContext(ctx, "subject-example", "other-project", "owner/agent")
	require.NoError(t, err)
	assert.False(t, active)
	active, err = st.HasImportantDateConsentContext(ctx, "subject-example", "private-example", "other-agent")
	require.NoError(t, err)
	assert.False(t, active)
	require.NoError(t, st.SetImportantDateConsentContext(ctx, "subject-example", "private-example", "owner/agent", false))
	active, err = st.HasImportantDateConsentContext(ctx, "subject-example", "private-example", "owner/agent")
	require.NoError(t, err)
	assert.False(t, active)
}

func TestImportantDateConsentDispatchRequiresGrant(t *testing.T) {
	st := storetest.New(t).Store
	ctx := t.Context()
	calls := 0
	dispatch := func() error { calls++; return nil }
	active, err := st.WithImportantDateConsentContext(ctx, "subject-example", "private-example", "owner/agent", dispatch)
	require.NoError(t, err)
	assert.False(t, active)
	assert.Zero(t, calls)
	require.NoError(t, st.SetImportantDateConsentContext(ctx, "subject-example", "private-example", "owner/agent", true))
	active, err = st.WithImportantDateConsentContext(ctx, "subject-example", "private-example", "owner/agent", dispatch)
	require.NoError(t, err)
	assert.True(t, active)
	assert.Equal(t, 1, calls)
}
