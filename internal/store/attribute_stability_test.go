package store_test

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func assertSeedStability(t *testing.T, st *store.Store) {
	t.Helper()
	for _, tc := range []struct {
		slug string
		want store.AttributeStability
	}{
		{"birthplace", store.AttributeStabilityFixed},
		{"how_we_met", store.AttributeStabilityFixed},
		{"interests_fun_growing_up", store.AttributeStabilityFixed},
		{"interests_fun_now", store.AttributeStabilityVolatile},
		{"ask_me_about", store.AttributeStabilityVolatile},
		{"last_contacted", store.AttributeStabilityVolatile},
		{"location", store.AttributeStabilityDurable},
	} {
		d, err := st.GetAttributeDefinitionBySlugContext(t.Context(), store.AttributeObjectPerson, tc.slug)
		require.NoError(t, err)
		assert.Equal(t, tc.want, d.Stability, tc.slug)
	}
}

func TestAttributeStabilitySeedClasses(t *testing.T) {
	st := testutil.NewTestStore(t)
	assertSeedStability(t, st)
	d, err := st.CreateAttributeDefinitionContext(t.Context(), personTextDefinition("custom_stability"))
	require.NoError(t, err)
	assert.Equal(t, store.AttributeStabilityDurable, d.Stability)
}

func TestAttributeStabilityUpgradeBackfillsSeeds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.DB().ExecContext(t.Context(), `ALTER TABLE attribute_definitions DROP COLUMN stability`)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM applied_migrations WHERE name = 'attribute_definition_stability_v1'`)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE archive_metadata SET value = '11' WHERE key = 'schema_version'`)
	require.NoError(err)
	oldVersion, err := st.SchemaVersionContext(t.Context())
	require.NoError(err)
	require.Equal(11, oldVersion)
	require.NoError(st.InitSchemaContext(t.Context()))
	assertSeedStability(t, st)
	version, err := st.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(store.SchemaVersion, version)
	require.NoError(st.InitSchemaContext(t.Context()))
	assertSeedStability(t, st)
}

func TestSeededStabilityEditSurvivesReopen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	d, err := st.GetAttributeDefinitionBySlugContext(t.Context(), store.AttributeObjectPerson, "birthplace")
	require.NoError(err)
	stability := store.AttributeStabilityDurable
	_, err = st.UpdateAttributeDefinitionContext(t.Context(), d.ID, d.Revision, store.AttributeDefinitionUpdate{Stability: &stability})
	require.NoError(err)
	require.NoError(st.InitSchemaContext(t.Context()))
	require.NoError(st.EnsureSeededAttributeDefinitionsContext(t.Context()))
	got, err := st.GetAttributeDefinitionContext(t.Context(), d.ID)
	require.NoError(err)
	assert.Equal(stability, got.Stability)
	assert.Equal(d.Revision+1, got.Revision)
}

func TestAttributeStabilityRejectsEpisodicAndUnknown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	d, err := st.GetAttributeDefinitionBySlugContext(t.Context(), store.AttributeObjectPerson, "location")
	require.NoError(err)
	episodic := store.AttributeStability("episodic")
	_, err = st.UpdateAttributeDefinitionContext(t.Context(), d.ID, d.Revision, store.AttributeDefinitionUpdate{Stability: &episodic})
	require.ErrorIs(err, store.ErrAttributeDefinitionInvalid)
	in := personTextDefinition("unknown_stability")
	in.Stability = "weekly"
	_, err = st.CreateAttributeDefinitionContext(t.Context(), in)
	require.ErrorIs(err, store.ErrAttributeDefinitionInvalid)
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO attribute_definitions (universal_id, object_type, slug, label, value_type, field_type, stability) VALUES ('bad-stability', 'person', 'bad_stability', 'Bad stability', 'text', 'text', 'episodic')`)
	require.Error(err)
	assert.Contains(strings.ToLower(err.Error()), "check constraint")
	assert.Contains(strings.ToLower(err.Error()), "stability")
}

func TestAttributeStabilityChangeKeepsCatalogFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	before, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(err)
	d, err := st.GetAttributeDefinitionBySlugContext(t.Context(), store.AttributeObjectPerson, "location")
	require.NoError(err)
	stability := store.AttributeStabilityVolatile
	_, err = st.UpdateAttributeDefinitionContext(t.Context(), d.ID, d.Revision, store.AttributeDefinitionUpdate{Stability: &stability})
	require.NoError(err)
	after, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(err)
	assert.Equal(before, after)
	encoded, err := json.Marshal(after)
	require.NoError(err)
	assert.NotContains(string(encoded), `"stability"`)
}
