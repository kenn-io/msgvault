package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStableMtimes(t *testing.T) {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_") {
			t.Setenv(name, "")
			require.NoError(t, os.Unsetenv(name))
		}
	}
	temp := t.TempDir()
	globalConfig := filepath.Join(temp, "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, nil, 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GOCACHE", filepath.Join(temp, "cache"))
	t.Setenv("GOWORK", "off")

	source := filepath.Join(temp, "source")
	require.NoError(t, os.MkdirAll(filepath.Join(source, "testdata"), 0o755))
	for name, content := range map[string]string{
		"go.mod": "module example.com/fixture\n\ngo 1.27.3\n\nrequire github.com/stretchr/testify v1.12.1\n",
		"input_test.go": `package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInputs(t *testing.T) {
	data, err := os.ReadFile("testdata/with space.txt")
	require.NoError(t, err)
	require.Len(t, data, 3)
	matches, err := filepath.Glob("testdata/*.txt")
	require.NoError(t, err)
	require.Len(t, matches, 1)
}
`,
		"testdata/with space.txt": "abc",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(source, name), []byte(content), 0o644))
	}
	for _, args := range [][]string{
		{"go", "mod", "tidy"},
		{"git", "init", "-q"},
		{"git", "add", "."},
		{"git", "-c", "user.name=Test User", "-c", "user.email=test@example.com", "commit", "-qm", "fixture"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = source
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}

	checkout := filepath.Join(temp, "checkout")
	for _, tc := range []struct {
		name   string
		edit   bool
		cached bool
	}{
		{"first clone", false, false},
		{"second clone", false, true},
		{"same size edit", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("git", "clone", "-q", source, checkout)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
			t.Cleanup(func() { require.NoError(t, os.RemoveAll(checkout)) })
			t.Chdir(checkout)
			if tc.edit {
				require.NoError(t, os.WriteFile("testdata/with space.txt", []byte("abd"), 0o644))
			}
			require.NoError(t, run())
			cmd = exec.Command("go", "test", "-tags", "fts5 sqlite_vec", "./...")
			output, err = cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
			assert.Equal(t, tc.cached, strings.Contains(string(output), "(cached)"), "%s", output)
		})
	}
}
