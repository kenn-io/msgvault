package update

import (
	"errors"
	"fmt"
)

// UpgradeCommand returns how to upgrade msgvault when a package manager owns
// the installed binary. It returns "" for builds that may replace themselves.
func UpgradeCommand(distribution string) string {
	switch distribution {
	case "":
		return ""
	case "homebrew":
		return "brew upgrade msgvault"
	default:
		return "upgrade msgvault through " + distribution
	}
}

// ManagedInstallError explains why a package-managed binary refuses to replace
// itself and how to upgrade it instead. It returns nil when distribution is
// empty.
func ManagedInstallError(distribution string) error {
	switch distribution {
	case "":
		return nil
	case "homebrew":
		return errors.New("this msgvault installation is managed by Homebrew; use `brew upgrade msgvault` instead")
	default:
		return fmt.Errorf("this msgvault installation is managed by %s; %s instead", distribution, UpgradeCommand(distribution))
	}
}

// ManagedUpdateNotice tells the user how to install an available update when a
// package manager owns the binary. It returns "" when distribution is empty.
func ManagedUpdateNotice(distribution string) string {
	switch distribution {
	case "":
		return ""
	case "homebrew":
		return "Homebrew manages this installation. Use `brew upgrade msgvault` once the formula has the new release."
	default:
		return fmt.Sprintf("%s manages this installation. Use `%s` once it has the new release.", distribution, UpgradeCommand(distribution))
	}
}
