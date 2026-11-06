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
	req := require.New(t)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_") {
			t.Setenv(name, "")
			req.NoError(os.Unsetenv(name))
		}
	}
	temp := t.TempDir()
	globalConfig := filepath.Join(temp, "gitconfig")
	req.NoError(os.WriteFile(globalConfig, nil, 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GOWORK", "off")

	source := filepath.Join(temp, "source")
	req.NoError(os.MkdirAll(filepath.Join(source, "testdata"), 0o755))
	for name, content := range map[string]string{
		"go.mod": "module example.com/fixture\n",
		"input_test.go": `package fixture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInputs(t *testing.T) {
	_, _ = os.ReadFile("testdata/café.txt")
	_, _ = filepath.Glob("testdata/*.txt")
}
`,
		"testdata/café.txt": "abc",
	} {
		req.NoError(os.WriteFile(filepath.Join(source, name), []byte(content), 0o644))
	}
	for _, args := range [][]string{
		{"git", "init", "-q"},
		{"git", "add", "."},
		{"git", "-c", "user.name=Test User", "-c", "user.email=test@example.com", "commit", "-qm", "fixture"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = source
		output, err := cmd.CombinedOutput()
		req.NoError(err, "%s", output)
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
			require := require.New(t)
			cmd := exec.Command("git", "clone", "-q", source, checkout)
			output, err := cmd.CombinedOutput()
			require.NoError(err, "%s", output)
			t.Cleanup(func() { require.NoError(os.RemoveAll(checkout)) })
			t.Chdir(checkout)
			if tc.edit {
				require.NoError(os.WriteFile("testdata/café.txt", []byte("abd"), 0o644))
			}
			require.NoError(run())
			cmd = exec.Command("go", "test", "./...")
			output, err = cmd.CombinedOutput()
			require.NoError(err, "%s", output)
			assert.Equal(t, tc.cached, strings.Contains(string(output), "(cached)"), "%s", output)
		})
	}
}
