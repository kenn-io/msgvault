package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstagramSourceHelpGoldens(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	for _, path := range [][]string{
		{"source", "instagram-comments"},
		{"source", "instagram-comments", "add"}, {"source", "instagram-comments", "sync"},
	} {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			var output bytes.Buffer
			rootCmd.SetOut(&output)
			rootCmd.SetErr(&output)
			rootCmd.SetArgs(append(append([]string{}, path...), "--help"))
			require.NoError(executeRootContext(t.Context(), rootCmd))
			file := filepath.Join("testdata/help", strings.Join(path, "-")+".golden")
			if *updateGolden {
				require.NoError(os.WriteFile(file, output.Bytes(), 0o644))
			}
			want, err := os.ReadFile(file)
			require.NoError(err)
			assert.Equal(strings.ReplaceAll(string(want), "\r\n", "\n"), strings.ReplaceAll(output.String(), "\r\n", "\n"))
			assert.Contains(output.String(), "msgvault source")
		})
	}
}
