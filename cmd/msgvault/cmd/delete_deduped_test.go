package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
				return fmt.Errorf("must specify --batch or --all-hidden")
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
	if err == nil {
		t.Fatal("expected error when neither --batch nor --all-hidden is set, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--batch") || !strings.Contains(msg, "--all-hidden") {
		t.Errorf("error should mention both flag names; got: %q", msg)
	}
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
	if err == nil {
		t.Fatal("expected error when both --batch and --all-hidden are set, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "batch") || !strings.Contains(msg, "all-hidden") {
		t.Errorf("error should mention both flag names; got: %q", msg)
	}
	_ = batch
	_ = allHidden
}
