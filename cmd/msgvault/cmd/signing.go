package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/requestsign"
)

func newSigningCommand() *cobra.Command {
	group := &cobra.Command{Use: "signing", Short: "Manage remote request signing state"}
	initCmd := &cobra.Command{Use: "init-state", Short: "Initialize a new private replay state file", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := cmd.Flags().GetString("file")
		if err != nil {
			return fmt.Errorf("read signing state flag: %w", err)
		}
		return requestsign.InitReplayState(path)
	}}
	initCmd.Flags().String("file", "", "New replay state file")
	_ = initCmd.MarkFlagRequired("file")
	group.AddCommand(initCmd)
	return group
}
func init() { rootCmd.AddCommand(newSigningCommand()) }
