//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package cmd

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestProxyDiagnosticIndependentOfChunkBoundariesProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		diagnostic := hegel.Draw(ht, hegel.SampledFrom([]string{
			"progress\nError: missing\nhint: run msgvault search\n",
			`{"error":{"code":"missing","message":"missing","hint":"run msgvault search","exit_code":4}}` + "\n",
		}))
		chunkSize := hegel.Draw(ht, hegel.Integers(1, len(diagnostic)))
		stderr := &childStderr{w: io.Discard}
		for start := 0; start < len(diagnostic); start += chunkSize {
			_, err := stderr.Write([]byte(diagnostic[start:min(start+chunkSize, len(diagnostic))]))
			require.NoError(ht, err)
		}
		assert.True(ht, (proxiedChildError{stderr: stderr}).alreadyReported())
	}, hegel.WithDatabase(t.TempDir()))
}
