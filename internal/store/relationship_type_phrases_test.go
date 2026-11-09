package store

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Add connection groups each canonical seeded type and reads its labels as a sentence from a copy in the web app.
func TestRelationshipTypeSeedsHaveWebPhrases(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	data, err := os.ReadFile("../../web/src/lib/directory/relationship-phrases.json")
	require.NoError(err)
	var web struct {
		Groups  map[string][]string  `json:"groups"`
		Phrases map[string][2]string `json:"phrases"`
	}
	require.NoError(json.Unmarshal(data, &web))
	for _, seed := range systemRelationshipTypes {
		if !seed.IsCanonical {
			continue
		}
		grouped := false
		for _, slugs := range web.Groups {
			grouped = grouped || slices.Contains(slugs, seed.Slug)
		}
		assert.True(grouped, "%s has no group", seed.Slug)
		assert.Equal(seed.ForwardLabel, web.Phrases[seed.Slug+":outgoing"][0], "%s outgoing phrase", seed.Slug)
		if !seed.IsSymmetric {
			assert.Equal(seed.ReverseLabel, web.Phrases[seed.Slug+":incoming"][0], "%s incoming phrase", seed.Slug)
		}
	}
}
