package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestServeRuntimeConfigCarriesVectorScopeBeforeInitialization(t *testing.T) {
	vectorCfg := config.NewDefaultConfig().Vector
	vectorCfg.Enabled = true
	vectorCfg.Embed.Scope.MessageTypes = []string{"teams"}
	opts := api.ServerOptions{}

	applyServerRuntimeConfig(&opts, &config.Config{Vector: vectorCfg})

	assert.Equal(t, []string{"teams"}, opts.VectorCfg.Embed.Scope.MessageTypes)
}

func TestStoreAPIAdapterExposesFileMetadataCatalog(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	adapter := &storeAPIAdapter{store: st}

	file, err := adapter.GetFileMetadata(t.Context(), 999999)
	requirements.NoError(err)
	assertions.Nil(file)
	files, err := adapter.GetFileMetadataBatch(t.Context(), nil)
	requirements.NoError(err)
	assertions.Empty(files)
}
