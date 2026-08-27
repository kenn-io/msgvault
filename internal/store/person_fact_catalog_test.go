package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestStoreBuildPersonFactCatalogCarriesMaxLength(t *testing.T) {
	st := testutil.NewTestStore(t)
	input := personTextDefinition("catalog_max_length")
	description := "Short biographical note"
	input.Description = &description
	input.Options = &store.AttributeOptions{MaxLength: 12}
	created, err := st.CreateAttributeDefinitionContext(t.Context(), input)
	require.NoError(t, err)

	first, err := st.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(t, err)
	firstTarget := personFactCatalogTarget(t, first, created.UniversalID)
	assert.Equal(t, 12, firstTarget.MaxLength)
}

func personFactCatalogTarget(
	t *testing.T, catalog personfacts.Catalog, universalID string,
) personfacts.TargetDescriptor {
	t.Helper()
	for _, target := range catalog.Targets {
		if target.Key == universalID {
			return target
		}
	}
	require.FailNow(t, "catalog target not found", universalID)
	return personfacts.TargetDescriptor{}
}

func TestStoreBuildPersonFactCatalogExcludesOverLimitDescription(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	input := personTextDefinition("over_limit_catalog_description")
	description := strings.Repeat("🙂", 281)
	input.Description = &description
	created, err := st.CreateAttributeDefinitionContext(t.Context(), input)
	require.NoError(err)

	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(err)
	for _, target := range catalog.Targets {
		assert.NotEqual(t, created.UniversalID, target.Key,
			"legacy description must not appear as an inference target")
	}
}
