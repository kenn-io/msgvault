//go:build !windows

package scripts

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

func TestPostgresShardedPackages(t *testing.T) {
	require := require.New(t)
	data, err := os.ReadFile("../.github/workflows/ci.yml")
	require.NoError(err)
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []struct {
						Package string `yaml:"package"`
					} `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	require.NoError(yaml.Unmarshal(data, &workflow))
	entries := workflow.Jobs["test-postgres-sharded"].Strategy.Matrix.Include
	require.NotEmpty(entries)
	packages := make(map[string]bool)
	for _, entry := range entries {
		require.NotEmpty(entry.Package)
		packages[entry.Package] = true
	}

	cmd := exec.Command("make", "-s", "--no-print-directory", "-f", "Makefile", "-f", "-", "print-pg-sharded-packages")
	cmd.Dir = ".."
	cmd.Stdin = strings.NewReader("print-pg-sharded-packages:\n\t@echo $(PG_SHARDED_TEST_PKGS)\n")
	output, err := cmd.CombinedOutput()
	require.NoError(err, string(output))
	want := strings.Fields(string(output))
	require.NotEmpty(want)
	var got []string
	for pkg := range packages {
		got = append(got, pkg)
	}
	assert.ElementsMatch(t, want, got)
}
