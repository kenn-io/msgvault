package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMergeAccountRequiresExplicitConfirmation(t *testing.T) {
	command := newMergeAccountCmd()
	command.SetArgs([]string{
		"--from", "source@example.test",
		"--into", "destination@example.test",
	})

	err := command.ExecuteContext(context.Background())
	require.ErrorContains(t, err, "merge retires the historical source")
	require.ErrorContains(t, err, "--yes")
}
