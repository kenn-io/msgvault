//go:build !windows

package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// expectedFinalBoundarySymlinkSwapError reports how a final-component symlink
// substitution surfaces on Unix-family platforms: the link resolves to the
// operator's byte-identical file, and the pinned identity mismatch is
// reported as ErrConfigConflict. See edit_race_windows_test.go for the
// Windows reparse-point behavior.
func expectedFinalBoundarySymlinkSwapError() error {
	return ErrConfigConflict
}

// replaceConfigTargetForFinalReadSwap publishes the operator's byte-identical
// replacement over the live config between publication and the final read.
// A plain rename is the native operator replacement on these platforms.
func replaceConfigTargetForFinalReadSwap(t *testing.T, replacement, target string) {
	t.Helper()
	require.NoError(t, os.Rename(replacement, target))
}
