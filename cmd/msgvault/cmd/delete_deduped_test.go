package cmd

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteDeduped_NeitherFlag verifies that omitting both --batch and
// --all-hidden produces an error mentioning both flag names.
func TestDeleteDeduped_NeitherFlag(t *testing.T) {
	var batch []string
	var allHidden bool
	cmd := &cobra.Command{Use: "delete-test", SilenceErrors: true}
	sub := &cobra.Command{
		Use: "delete-deduped",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(batch) == 0 && !allHidden {
				return errors.New("must specify --batch or --all-hidden")
			}
			return nil
		},
	}
	sub.Flags().StringArrayVar(&batch, "batch", nil, "")
	sub.Flags().BoolVar(&allHidden, "all-hidden", false, "")
	sub.MarkFlagsMutuallyExclusive("batch", "all-hidden")
	cmd.AddCommand(sub)
	cmd.SetArgs([]string{"delete-deduped"})

	err := cmd.Execute()
	require.Error(t, err, "expected error when neither --batch nor --all-hidden is set")
	msg := err.Error()
	assert.Contains(t, msg, "--batch", "error should mention --batch flag name")
	assert.Contains(t, msg, "--all-hidden", "error should mention --all-hidden flag name")
}

// TestDeleteDeduped_MutualExclusion verifies that passing both --batch and
// --all-hidden is rejected by cobra.
func TestDeleteDeduped_MutualExclusion(t *testing.T) {
	var batch []string
	var allHidden bool
	cmd := &cobra.Command{Use: "delete-test", SilenceErrors: true}
	sub := &cobra.Command{Use: "delete-deduped", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
	sub.Flags().StringArrayVar(&batch, "batch", nil, "")
	sub.Flags().BoolVar(&allHidden, "all-hidden", false, "")
	sub.MarkFlagsMutuallyExclusive("batch", "all-hidden")
	cmd.AddCommand(sub)
	cmd.SetArgs([]string{"delete-deduped", "--batch", "some-id", "--all-hidden"})

	err := cmd.Execute()
	require.Error(t, err, "expected error when both --batch and --all-hidden are set")
	msg := err.Error()
	assert.Contains(t, msg, "batch", "error should mention batch flag name")
	assert.Contains(t, msg, "all-hidden", "error should mention all-hidden flag name")
	_ = batch
	_ = allHidden
}
