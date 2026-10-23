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
			assertions := assert.New(t)
			requirements := require.New(t)
			srcDB := createTestSourceDB(t, t.TempDir(), 3)
			source, err := Open(srcDB)
			requirements.NoError(err)
			contact, err := source.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugContactFrequency)
			requirements.NoError(err)
			_, err = source.UpdateAttributeDefinitionContext(t.Context(), contact.ID, contact.Revision,
				AttributeDefinitionUpdate{InferenceEligible: new(true)})
			requirements.NoError(err)
			location, err := source.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugLocation)
			requirements.NoError(err)
			_, err = source.UpdateAttributeDefinitionContext(t.Context(), location.ID, location.Revision,
				AttributeDefinitionUpdate{InferenceEligible: new(false)})
			requirements.NoError(err)
			input := subsetPersonDefinition("private_fact")
			input.InferenceEligible = new(false)
			_, err = source.CreateAttributeDefinitionContext(t.Context(), input)
			requirements.NoError(err)
			requirements.NoError(source.Close())
			if legacy {
				db, err := sql.Open(sqliteutil.DriverName(), srcDB+"?_foreign_keys=OFF")
				requirements.NoError(err)
				_, err = db.Exec(`ALTER TABLE attribute_definitions DROP COLUMN inference_eligible`)
				requirements.NoError(err)
				requirements.NoError(db.Close())
			}
			dstDir := filepath.Join(t.TempDir(), "subset")
			_, err = CopySubsetWithOptions(srcDB, dstDir, 3, CopySubsetOptions{IncludeAttributes: true})
			requirements.NoError(err)
			destination, err := Open(filepath.Join(dstDir, "msgvault.db"))
			requirements.NoError(err)
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
				requirements.NoError(err)
				assertions.Equal(tc.want, got.InferenceEligible, tc.slug)
			}
			// Normal setup must keep copied owner choices and legacy backfill intact.
			requirements.NoError(destination.InitSchemaContext(t.Context()))
			got, err := destination.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugContactFrequency)
			requirements.NoError(err)
			assertions.Equal(!legacy, got.InferenceEligible)
		})
	}
}
