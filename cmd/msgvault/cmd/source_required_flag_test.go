package cmd

import (
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSourceRequiredStringFlagErrors(t *testing.T) {
	for _, hasWrongType := range []bool{false, true} {
		command := &cobra.Command{Use: "example"}
		if hasWrongType {
			command.Flags().Bool("identifier", false, "wrong type")
		}
		err := sourceRequiredString(command, "identifier", "archive identifier", "msgvault account list --json")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "identifier")
	}
}
