package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/placeautocomplete"
)

func TestAddressLookupRejectsIncompleteBiasAndMissingSession(t *testing.T) {
	t.Setenv(placeautocomplete.APIKeyEnv, "")
	for _, args := range [][]string{
		{"autocomplete", "12 Example", "--lat", "1"},
		{"select", "candidate-1"},
	} {
		command := newAddressLookupCommand()
		command.SetArgs(args)
		err := command.ExecuteContext(t.Context())
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "Google Places request failed")
	}
}
