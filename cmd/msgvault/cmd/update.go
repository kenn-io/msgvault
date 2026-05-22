package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/update"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update msgvault to the latest version",
	Long: `Check for and install msgvault updates.

Shows exactly what will be downloaded and where it will be installed.
Requires confirmation before making changes (use --yes to skip).

Dev builds are not replaced by default. Use --force to install the latest
official release over a dev build.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		checkOnly, _ := cmd.Flags().GetBool("check")
		yes, _ := cmd.Flags().GetBool("yes")
		force, _ := cmd.Flags().GetBool("force")

		fmt.Println("Checking for updates...")

		info, err := update.CheckForUpdate(Version, true)
		if err != nil {
			return fmt.Errorf("check for updates: %w", err)
		}

		if info == nil {
			fmt.Printf("Already running latest version (%s)\n", Version)
			return nil
		}

		fmt.Printf("\n  Current version: %s\n", info.CurrentVersion)
		fmt.Printf("  Latest version:  %s\n", info.LatestVersion)
		if info.IsDevBuild {
			fmt.Println("\nYou're running a dev build. Latest official release available.")
		} else {
			fmt.Println("\nUpdate available!")
		}
		fmt.Println("\nDownload:")
		fmt.Printf("  URL:  %s\n", info.DownloadURL)
		fmt.Printf("  Size: %s\n", update.FormatSize(info.Size))
		if info.Checksum != "" {
			fmt.Printf("  SHA256: %s\n", info.Checksum)
		}

		currentExe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("find executable: %w", err)
		}
		currentExe, _ = filepath.EvalSymlinks(currentExe)
		binDir := filepath.Dir(currentExe)

		fmt.Println("\nInstall location:")
		fmt.Printf("  %s\n", binDir)

		if checkOnly {
			if info.IsDevBuild {
				fmt.Println("\nUse --force to install the latest official release.")
			}
			return nil
		}

		if info.IsDevBuild && !force {
			fmt.Println("\nUse --force to install the latest official release.")
			return nil
		}

		if !yes {
			fmt.Print("\nProceed with update? [y/N] ")
			var response string
			_, _ = fmt.Scanln(&response)
			if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
				fmt.Println("Update cancelled")
				return nil
			}
		}

		fmt.Println()

		var lastPercent int
		progressFn := func(downloaded, total int64) {
			if total > 0 {
				percent := int(downloaded * 100 / total)
				if percent != lastPercent {
					fmt.Printf("\rDownloading... %d%% (%s / %s)",
						percent, update.FormatSize(downloaded), update.FormatSize(total))
					lastPercent = percent
				}
			}
		}

		if err := update.PerformUpdate(info, progressFn); err != nil {
			return fmt.Errorf("update failed: %w", err)
		}

		fmt.Printf("\nUpdated to %s\n", info.LatestVersion)
		return nil
	},
}

func init() {
	updateCmd.Flags().Bool("check", false, "only check for updates, don't install")
	updateCmd.Flags().BoolP("yes", "y", false, "skip confirmation prompt")
	updateCmd.Flags().BoolP("force", "f", false, "replace dev build with latest official release")
	rootCmd.AddCommand(updateCmd)
}
