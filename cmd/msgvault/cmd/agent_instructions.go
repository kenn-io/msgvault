package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/agentguide"
)

var agentInstructionsCmd = &cobra.Command{
	Use:   "agent-instructions",
	Short: "Print or explicitly write MCP-only agent instructions",
	Long: `Print a small MCP-only instruction block without loading configuration or an archive.

Use --write PATH to add or update only msgvault's managed block in a local
instruction file. Unrelated text and existing file permissions are preserved.
Repeated writes are unchanged. Symlinks, nonregular files and malformed
managed markers are rejected. Parent directories must already exist.

Examples:
  msgvault agent-instructions
  msgvault agent-instructions --write ./AGENTS.md`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if !cmd.Flags().Changed("write") {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), agentguide.ManagedBlock())
			return err
		}
		path, _ := cmd.Flags().GetString("write")
		if path == "" {
			return usageErr(cmd, errors.New("--write requires a nonempty local path"))
		}
		changed, err := agentguide.WriteInstructions(path)
		if err != nil {
			return err
		}
		status := "Unchanged"
		if changed {
			status = "Updated"
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", status, path)
		return err
	},
}

func init() {
	agentInstructionsCmd.Flags().String("write", "", "Explicit local instruction file to create or update")
	rootCmd.AddCommand(agentInstructionsCmd)
}
