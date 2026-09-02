//go:build windows

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// expectedFinalBoundarySymlinkSwapError reports how a final-component symlink
// substitution surfaces on Windows. Windows never follows a config symlink:
// readConfigFileSnapshot rejects the reparse point as a non-regular file
// (ErrUnsafeConfigTarget) before any identity comparison can run, while
// Unix-family platforms resolve the link and report the identity mismatch as
// ErrConfigConflict.
func expectedFinalBoundarySymlinkSwapError() error {
	return ErrUnsafeConfigTarget
}

// replaceConfigTargetForFinalReadSwap publishes the operator's byte-identical
// replacement over the live config between publication and the final read.
// os.Rename (MoveFileExW) fails here with Access denied: the live target is
// still pinned by the transaction's retained identity handle, which blocks
// the replace-style rename. ReplaceFileW is the established Windows primitive
// for swapping the live config under exactly that retention, so the operator
// race must use it too.
func replaceConfigTargetForFinalReadSwap(t *testing.T, replacement, target string) {
	t.Helper()
	backup := replacement + ".displaced"
	require.NoError(t, replaceFile(target, replacement, backup))
}
