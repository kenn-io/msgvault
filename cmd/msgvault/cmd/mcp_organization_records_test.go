package cmd

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOrganizationReadsUseNativeStore(t *testing.T) {
	st := testutil.NewTestStore(t)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	for _, name := range []string{"get_organization", "get_organization_history"} {
		result, err := backend.ExecuteOperation(t.Context(), name, map[string]any{"organization_id": org.ID})
		require.NoError(t, err)
		profile := operationOutput[mcpserver.OrganizationRecord](t, result)
		assert.Equal(t, org.ID, profile.Profile.Organization.ID)
		assert.NotEmpty(t, profile.ETag)
	}
	result, err := backend.ExecuteOperation(t.Context(), "list_organization_attributes", map[string]any{"organization_id": org.ID})
	require.NoError(t, err)
	require.False(t, result.IsError)
	result, err = backend.ExecuteOperation(t.Context(), "get_organization", map[string]any{"organization_id": -1})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}
