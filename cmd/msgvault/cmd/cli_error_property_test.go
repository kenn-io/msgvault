//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package cmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Hegel's native engines support Linux/Windows and macOS arm64; ordinary
// rendering regression tests also run on macOS amd64.
func TestCLIClassificationPreservesJoinedCausesProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		code := hegel.Draw(ht, hegel.SampledFrom([]int{1, 2, 3, 4, 5, 6, 7, 75, 130}))
		message := hegel.Draw(ht, hegel.Text())
		cause := errors.New(message)
		sibling := errors.New("independent cause")
		explicit := &cliError{Kind: "fixture", Code: "fixture", Message: message, ExitCode: code, Err: cause}
		original := fmt.Errorf("context: %w", errors.Join(explicit, sibling))
		classified := classifyCLIError(nil, original)
		assert.Equal(ht, code, classified.ExitCode)
		assert.Equal(ht, code, ExitCode(original))
		require.ErrorIs(ht, classified, cause)
		require.ErrorIs(ht, classified, sibling)
	}, hegel.WithDatabase(t.TempDir()))
}
