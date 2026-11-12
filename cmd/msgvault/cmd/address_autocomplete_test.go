package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/placeautocomplete"
)

func TestAddressLookupRejectsIncompleteBiasAndMissingSession(t *testing.T) {
	t.Setenv(placeautocomplete.APIKeyEnv, "")
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"autocomplete", "12 Example", "--lat", "1"}, "--lat, --lng and --radius must be supplied together"},
		{[]string{"select", "candidate-1"}, "invalid session token"},
	} {
		command := newAddressLookupCommand()
		command.SetArgs(tc.args)
		err := command.ExecuteContext(t.Context())
		require.Error(t, err)
		assert.Equal(t, tc.message, err.Error())
	}
}
