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
			srcDB := createTestSourceDB(t, t.TempDir(), 3)
			source, err := Open(srcDB)
			require.NoError(t, err)
			location, err := source.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugLocation)
			require.NoError(t, err)
			_, err = source.UpdateAttributeDefinitionContext(t.Context(), location.ID, location.Revision,
				AttributeDefinitionUpdate{Stability: new(AttributeStabilityVolatile)})
			require.NoError(t, err)
			input := subsetPersonDefinition("stable_custom")
			input.Stability = AttributeStabilityFixed
			_, err = source.CreateAttributeDefinitionContext(t.Context(), input)
			require.NoError(t, err)
			require.NoError(t, source.Close())
			if legacy {
				db, err := sql.Open(sqliteutil.DriverName(), srcDB+"?_foreign_keys=OFF")
				require.NoError(t, err)
				_, err = db.Exec(`ALTER TABLE attribute_definitions DROP COLUMN stability`)
				require.NoError(t, err)
				require.NoError(t, db.Close())
			}
			dstDir := filepath.Join(t.TempDir(), "subset")
			_, err = CopySubsetWithOptions(srcDB, dstDir, 3, CopySubsetOptions{IncludeAttributes: true})
			require.NoError(t, err)
			destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, destination.Close()) })
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
					require.NoError(t, err)
					assert.Equal(t, tc.want, got.Stability, tc.slug)
				}
			}
			check()
			require.NoError(t, destination.InitSchemaContext(t.Context()))
			check()
		})
	}
}
