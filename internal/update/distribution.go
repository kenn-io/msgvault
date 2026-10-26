package update

import (
	"errors"
	"fmt"
)

// officialDistribution marks official direct downloads, which update
// themselves like source builds.
const officialDistribution = "kenn"

// SelfUpdating reports whether a build with this distribution may replace
// itself. Source builds leave the distribution empty and official direct
// downloads set "kenn"; any other value names the package manager that owns
// the binary.
func SelfUpdating(distribution string) bool {
	return distribution == "" || distribution == officialDistribution
}

// UpgradeCommand returns how to upgrade msgvault when a package manager owns
// the installed binary. It returns "" for builds that may replace themselves.
func UpgradeCommand(distribution string) string {
	switch {
	case SelfUpdating(distribution):
		return ""
	case distribution == "homebrew":
		return "brew upgrade msgvault"
	default:
		return "upgrade msgvault through " + distribution
	}
}

// ManagedInstallError explains why a package-managed binary refuses to replace
// itself and how to upgrade it instead. It returns nil for builds that may
// replace themselves.
func ManagedInstallError(distribution string) error {
	switch {
	case SelfUpdating(distribution):
		return nil
	case distribution == "homebrew":
		return errors.New("this msgvault installation is managed by Homebrew; use `brew upgrade msgvault` instead")
	default:
		return fmt.Errorf("this msgvault installation is managed by %s; %s instead", distribution, UpgradeCommand(distribution))
	}
}

// ManagedUpdateNotice tells the user how to install an available update when a
// package manager owns the binary. It returns "" for builds that may replace
// themselves.
func ManagedUpdateNotice(distribution string) string {
	switch {
	case SelfUpdating(distribution):
		return ""
	case distribution == "homebrew":
		return "Homebrew manages this installation. Use `brew upgrade msgvault` once the formula has the new release."
	default:
		return fmt.Sprintf("%s manages this installation. Use `%s` once it has the new release.", distribution, UpgradeCommand(distribution))
	}
}
