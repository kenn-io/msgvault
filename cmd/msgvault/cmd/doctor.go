package cmd

import "github.com/spf13/cobra"

func newDoctorCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "doctor",
		Short: "Run read-only health checks on the archive",
		Args:  cobra.NoArgs,
	}
	command.AddCommand(newDoctorFTSCmd())
	return command
}

func init() { rootCmd.AddCommand(newDoctorCmd()) }
