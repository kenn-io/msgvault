package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Version information - set via ldflags at build time.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
	// Distribution names the package manager that owns this binary, such as
	// "homebrew". Empty for release archives and source builds, which may
	// replace themselves with `msgvault update`.
	Distribution = ""
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("msgvault %s\n", Version)
		fmt.Printf("  commit:  %s\n", Commit)
		fmt.Printf("  built:   %s\n", BuildDate)
		fmt.Printf("  go:      %s\n", runtime.Version())
		fmt.Printf("  os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
		if Distribution != "" {
			fmt.Printf("  package: %s\n", Distribution)
		}
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
