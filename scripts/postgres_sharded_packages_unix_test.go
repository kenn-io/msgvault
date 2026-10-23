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
	must := require.New(t)
	data, err := os.ReadFile("../.github/workflows/ci.yml")
	must.NoError(err)
	type matrixEntry struct {
		Name    string `yaml:"name"`
		Package string `yaml:"package"`
		Part    int    `yaml:"part"`
		Parts   *int   `yaml:"parts"`
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
			Strategy struct {
				Matrix struct {
					Include []matrixEntry `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	must.NoError(yaml.Unmarshal(data, &workflow))
	for jobName, job := range workflow.Jobs {
		byPackage := make(map[string][]matrixEntry)
		for _, entry := range job.Strategy.Matrix.Include {
			byPackage[entry.Package] = append(byPackage[entry.Package], entry)
		}
		for pkg, entries := range byPackage {
			var parts *int
			for _, entry := range entries {
				if entry.Parts != nil {
					parts = entry.Parts
					break
				}
			}
			if parts == nil {
				continue
			}
			t.Run(jobName+"/"+pkg, func(t *testing.T) {
				require := require.New(t)
				require.NotEmpty(pkg)
				require.Positive(*parts)
				var got []int
				for _, entry := range entries {
					require.NotNil(entry.Parts, "%s must declare parts", entry.Name)
					require.Equal(*parts, *entry.Parts, "%s must agree on parts", entry.Name)
					got = append(got, entry.Part)
				}
				want := make([]int, *parts)
				for i := range want {
					want[i] = i + 1
				}
				require.ElementsMatch(want, got, "package must cover parts 1..%d exactly once", *parts)
			})
		}
	}
	entries := workflow.Jobs["test-postgres-sharded"].Strategy.Matrix.Include
	must.NotEmpty(entries)
	packages := make(map[string]bool)
	for _, entry := range entries {
		must.NotEmpty(entry.Package)
		packages[entry.Package] = true
	}

	var run string
	for _, step := range workflow.Jobs["test-postgres"].Steps {
		if step.Name == "Test (PostgreSQL)" {
			run = step.Run
			break
		}
	}
	must.NotEmpty(run, "jobs.test-postgres must have a Test (PostgreSQL) step with a run command")
	match := regexp.MustCompile(`\bSHARDED_TEST_PKGS=(?:"([^"]*)"|'([^']*)')`).FindStringSubmatch(run)
	must.Len(match, 3, "Test (PostgreSQL) must set an explicit quoted SHARDED_TEST_PKGS override")
	want := strings.Fields(match[1] + match[2])
	must.NotEmpty(want, "SHARDED_TEST_PKGS override must list the PostgreSQL sharded packages")
	var got []string
	for pkg := range packages {
		got = append(got, pkg)
	}
	assert.ElementsMatch(t, want, got)
}
