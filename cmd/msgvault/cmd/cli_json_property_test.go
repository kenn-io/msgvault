//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestCLIJSONSelectorAgreesWithPflagProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		cmd := &cobra.Command{Use: "probe"}
		cmd.Flags().Bool("json", false, "")
		cmd.Flags().BoolP("verbose", "v", false, "")
		cmd.Flags().StringP("body", "b", "", "")
		choices := [][]string{{"--json"}, {"--json=false"}, {"--json=true"}, {"-v"}, {"-vb", "--json"}, {"--body", "--json"}, {"-b--json"}, {"--body=value"}, {"positional"}}
		var args []string
		for range hegel.Draw(ht, hegel.Integers(1, 12)) {
			args = append(args, hegel.Draw(ht, hegel.SampledFrom(choices))...)
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			args = append(args, "--", "--json")
		}
		got := cliJSONRequested(cmd, args)
		require.NoError(ht, cmd.Flags().Parse(args))
		want, err := cmd.Flags().GetBool("json")
		require.NoError(ht, err)
		assert.Equal(ht, want, got, "%q", args)
	}, hegel.WithDatabase(t.TempDir()))
}
