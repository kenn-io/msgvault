package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

func TestCopySubsetPreservesInferenceEligibility(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "current"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			srcDB := createTestSourceDB(t, t.TempDir(), 3)
			source, err := Open(srcDB)
			require.NoError(t, err)
			contact, err := source.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugContactFrequency)
			require.NoError(t, err)
			_, err = source.UpdateAttributeDefinitionContext(t.Context(), contact.ID, contact.Revision,
				AttributeDefinitionUpdate{InferenceEligible: new(true)})
			require.NoError(t, err)
			location, err := source.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugLocation)
			require.NoError(t, err)
			_, err = source.UpdateAttributeDefinitionContext(t.Context(), location.ID, location.Revision,
				AttributeDefinitionUpdate{InferenceEligible: new(false)})
			require.NoError(t, err)
			input := subsetPersonDefinition("private_fact")
			input.InferenceEligible = new(false)
			_, err = source.CreateAttributeDefinitionContext(t.Context(), input)
			require.NoError(t, err)
			require.NoError(t, source.Close())
			if legacy {
				db, err := sql.Open(sqliteutil.DriverName(), srcDB+"?_foreign_keys=OFF")
				require.NoError(t, err)
				_, err = db.Exec(`ALTER TABLE attribute_definitions DROP COLUMN inference_eligible`)
				require.NoError(t, err)
				require.NoError(t, db.Close())
			}
			dstDir := filepath.Join(t.TempDir(), "subset")
			_, err = CopySubsetWithOptions(srcDB, dstDir, 3, CopySubsetOptions{IncludeAttributes: true})
			require.NoError(t, err)
			destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, destination.Close()) })
			for _, tc := range []struct {
				slug string
				want bool
			}{
				{AttributeSlugPersonality, false},
				{AttributeSlugContactFrequency, !legacy},
				{AttributeSlugLocation, legacy},
				{"private_fact", legacy},
			} {
				got, err := destination.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, tc.slug)
				require.NoError(t, err)
				assert.Equal(t, tc.want, got.InferenceEligible, tc.slug)
			}
			// Normal setup must keep copied owner choices and legacy backfill intact.
			require.NoError(t, destination.InitSchemaContext(t.Context()))
			got, err := destination.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugContactFrequency)
			require.NoError(t, err)
			assert.Equal(t, !legacy, got.InferenceEligible)
		})
	}
}
