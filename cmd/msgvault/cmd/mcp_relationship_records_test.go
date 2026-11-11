package cmd

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPRelationshipTypesReadRealStore(t *testing.T) {
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "list_relationship_types", nil)
	require.NoError(t, err)
	types := operationOutput[generated.RelationshipTypesResponse](t, result)
	require.NotEmpty(t, types.RelationshipTypes)
	result, err = backend.ExecuteOperation(t.Context(), "get_relationship_type", map[string]any{"id": types.RelationshipTypes[0].ID})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.NotEmpty(t, result.ETag)
}
