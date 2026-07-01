package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
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
		installExecutablePath := filepath.Join(binDir, updateExecutableName())

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
			if !isYesAnswer(strings.ToLower(response)) {
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

		if err := performUpdateWithDaemonLifecycle(
			info,
			progressFn,
			loadDaemonConfigForUpdate,
			stopLocalDaemonsForUpdate,
			update.PerformUpdate,
			func(c *config.Config, result updateDaemonStopResult) error {
				return restartDaemonAfterUpdate(c, result, installExecutablePath)
			},
		); err != nil {
			return err
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

type updateDaemonStopResult struct {
	Stopped bool
}

func performUpdateWithDaemonLifecycle(
	info *update.UpdateInfo,
	progressFn func(downloaded, total int64),
	loadDaemonConfig func() (*config.Config, error),
	stopDaemons func(*config.Config) (updateDaemonStopResult, error),
	perform func(*update.UpdateInfo, func(int64, int64)) error,
	restartDaemon func(*config.Config, updateDaemonStopResult) error,
) error {
	daemonCfg, err := loadDaemonConfig()
	if err != nil {
		return fmt.Errorf("loading daemon config before update: %w", err)
	}
	stopResult, err := stopDaemons(daemonCfg)
	if err != nil {
		if stopResult.Stopped {
			if restartErr := restartDaemon(daemonCfg, stopResult); restartErr != nil {
				return fmt.Errorf(
					"stopping daemon before update: %w (also failed to restart daemon: %w)",
					err, restartErr,
				)
			}
		}
		return fmt.Errorf("stopping daemon before update: %w", err)
	}

	if err := perform(info, progressFn); err != nil {
		if stopResult.Stopped {
			if restartErr := restartDaemon(daemonCfg, stopResult); restartErr != nil {
				return fmt.Errorf(
					"update failed: %w (also failed to restart daemon: %w)",
					err, restartErr,
				)
			}
		}
		return fmt.Errorf("update failed: %w", err)
	}

	if stopResult.Stopped {
		if err := restartDaemon(daemonCfg, stopResult); err != nil {
			return fmt.Errorf("restarting daemon after update: %w", err)
		}
	}
	return nil
}

func loadDaemonConfigForUpdate() (*config.Config, error) {
	c, err := config.Load(cfgFile, homeDir)
	if err != nil {
		return nil, err
	}
	if err := c.EnsureHomeDir(); err != nil {
		return nil, fmt.Errorf("create data directory %s: %w", c.HomeDir, err)
	}
	return c, nil
}

func stopLocalDaemonsForUpdate(c *config.Config) (updateDaemonStopResult, error) {
	var result updateDaemonStopResult
	if c == nil {
		return result, errors.New("nil config")
	}
	records, err := listLiveDaemonRuntimeRecords(c.Data.DataDir)
	if err != nil {
		return result, err
	}
	for _, rec := range records {
		rt := daemonRuntimeFromRecord(rec)
		if err := stopDaemonRuntimeForUpgrade(*c, rt); err != nil {
			return result, err
		}
		result.Stopped = true
	}
	return result, nil
}

func restartDaemonAfterUpdate(c *config.Config, result updateDaemonStopResult, executablePath string) error {
	if !result.Stopped {
		return nil
	}
	cmd := &cobra.Command{Use: "msgvault update daemon-restart"}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return runServeStartWithOptions(cmd, c, backgroundServeStartOptions{ExecutablePath: executablePath})
}

func updateExecutableName() string {
	if runtime.GOOS == "windows" {
		return "msgvault.exe"
	}
	return "msgvault"
}
