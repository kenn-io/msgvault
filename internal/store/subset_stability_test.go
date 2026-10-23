package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

func TestCopySubsetPreservesStability(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "current"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srcDB := createTestSourceDB(t, t.TempDir(), 3)
			source, err := Open(srcDB)
			require.NoError(err)
			location, err := source.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugLocation)
			require.NoError(err)
			_, err = source.UpdateAttributeDefinitionContext(t.Context(), location.ID, location.Revision,
				AttributeDefinitionUpdate{Stability: new(AttributeStabilityVolatile)})
			require.NoError(err)
			input := subsetPersonDefinition("stable_custom")
			input.Stability = AttributeStabilityFixed
			_, err = source.CreateAttributeDefinitionContext(t.Context(), input)
			require.NoError(err)
			require.NoError(source.Close())
			if legacy {
				db, err := sql.Open(sqliteutil.DriverName(), srcDB+"?_foreign_keys=OFF")
				require.NoError(err)
				_, err = db.Exec(`ALTER TABLE attribute_definitions DROP COLUMN stability`)
				require.NoError(err)
				require.NoError(db.Close())
			}
			dstDir := filepath.Join(t.TempDir(), "subset")
			_, err = CopySubsetWithOptions(srcDB, dstDir, 3, CopySubsetOptions{IncludeAttributes: true})
			require.NoError(err)
			destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
			require.NoError(err)
			t.Cleanup(func() { require.NoError(destination.Close()) })
			locationStability := AttributeStabilityVolatile
			customStability := AttributeStabilityFixed
			if legacy {
				locationStability = AttributeStabilityDurable
				customStability = AttributeStabilityDurable
			}
			check := func() {
				t.Helper()
				for _, tc := range []struct {
					slug string
					want AttributeStability
				}{
					{AttributeSlugBirthplace, AttributeStabilityFixed},
					{AttributeSlugInterestsFunNow, AttributeStabilityVolatile},
					{AttributeSlugLocation, locationStability},
					{"stable_custom", customStability},
				} {
					got, err := destination.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, tc.slug)
					require.NoError(err)
					assert.Equal(tc.want, got.Stability, tc.slug)
				}
			}
			check()
			require.NoError(destination.InitSchemaContext(t.Context()))
			check()
		})
	}
}
