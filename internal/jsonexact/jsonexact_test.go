package jsonexact_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jsonexact"
)

type duplicateEnvelope struct {
	Name   string            `json:"name"`
	Nested *duplicateNested  `json:"nested"`
	Items  []duplicateNested `json:"items"`
}

type duplicateNested struct {
	Value string `json:"value"`
}

func TestValidateRejectsDuplicateMembersRecursively(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"root", `{"name":"first","name":"second"}`},
		{"nested object", `{"nested":{"value":"first","value":"second"}}`},
		{"object in array", `{"items":[{"value":"first","value":"second"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := jsonexact.Validate([]byte(test.data), duplicateEnvelope{})
			require.Error(t, err)
			assert.ErrorContains(t, err, "duplicate")
		})
	}
}

func TestValidateAcceptsUniqueMembersRecursively(t *testing.T) {
	err := jsonexact.Validate(
		[]byte(`{"name":"one","nested":{"value":"two"},"items":[{"value":"three"}]}`),
		duplicateEnvelope{},
	)
	require.NoError(t, err)
}
