//go:build !windows

package scripts

import (
	"os"
	"regexp"
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
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
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

	var run string
	for _, step := range workflow.Jobs["test-postgres"].Steps {
		if step.Name == "Test (PostgreSQL)" {
			run = step.Run
			break
		}
	}
	require.NotEmpty(run, "jobs.test-postgres must have a Test (PostgreSQL) step with a run command")
	match := regexp.MustCompile(`\bSHARDED_TEST_PKGS="([^"]*)"`).FindStringSubmatch(run)
	require.Len(match, 2, "Test (PostgreSQL) must set an explicit quoted SHARDED_TEST_PKGS override")
	want := strings.Fields(match[1])
	require.NotEmpty(want, "SHARDED_TEST_PKGS override must list the PostgreSQL sharded packages")
	var got []string
	for pkg := range packages {
		got = append(got, pkg)
	}
	assert.ElementsMatch(t, want, got)
}
