package daemonclient

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestAttributeDefinitionFromGeneratedPreservesStability(t *testing.T) {
	for _, stability := range []store.AttributeStability{store.AttributeStabilityFixed, store.AttributeStabilityDurable, store.AttributeStabilityVolatile} {
		definition := generated.AttributeDefinition{}
		require.NoError(t, json.Unmarshal([]byte(`{"stability":"`+string(stability)+`"}`), &definition))
		got := attributeDefinitionFromGenerated(definition)
		assert.Equal(t, stability, got.Stability)
	}
}
