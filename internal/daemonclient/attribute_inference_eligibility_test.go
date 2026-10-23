package daemonclient

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestAttributeDefinitionFromGeneratedPreservesInferenceEligibility(t *testing.T) {
	for _, eligible := range []bool{false, true} {
		got := attributeDefinitionFromGenerated(generated.AttributeDefinition{InferenceEligible: eligible})
		assert.Equal(t, eligible, got.InferenceEligible)
	}
}
