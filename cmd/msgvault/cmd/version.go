package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/update"
)

// Version information - set via ldflags at build time.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
	// Distribution names who distributes this binary. Source builds leave it
	// empty and official direct downloads set "kenn"; both replace themselves
	// with `msgvault update`. Any other value, such as "homebrew", names the
	// package manager that owns the binary.
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
		if !update.SelfUpdating(Distribution) {
			fmt.Printf("  package: %s\n", Distribution)
		}
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
