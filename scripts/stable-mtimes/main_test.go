package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, nil, 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	binary := filepath.Join(t.TempDir(), "stable-mtimes.exe")
	output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput()
	require.NoError(t, err, string(output))
	for _, tc := range []struct {
		name string
		edit bool
	}{
		{"fresh checkouts", false},
		{"same size edit", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			repos := []string{t.TempDir(), t.TempDir()}
			paths := []string{"file.txt", "nested/with space.txt"}
			for _, repo := range repos {
				require.NoError(os.Mkdir(filepath.Join(repo, "nested"), 0o755))
				for _, path := range paths {
					require.NoError(os.WriteFile(filepath.Join(repo, path), []byte("abc"), 0o644))
				}
				for _, args := range [][]string{
					{"init", "-q"},
					{"add", "."},
					{"-c", "user.name=Test User", "-c", "user.email=test@example.com", "commit", "-qm", "fixture"},
				} {
					cmd := exec.Command("git", args...)
					cmd.Dir = repo
					output, err := cmd.CombinedOutput()
					require.NoError(err, string(output))
				}
			}
			later := time.Now().Add(time.Hour)
			for _, path := range paths {
				require.NoError(os.Chtimes(filepath.Join(repos[1], path), later, later))
			}
			before, err := os.Stat(filepath.Join(repos[0], paths[0]))
			require.NoError(err)
			after, err := os.Stat(filepath.Join(repos[1], paths[0]))
			require.NoError(err)
			require.NotEqual(before.ModTime(), after.ModTime())
			if tc.edit {
				require.NoError(os.WriteFile(filepath.Join(repos[1], paths[0]), []byte("abd"), 0o644))
			}
			for _, repo := range repos {
				cmd := exec.Command(binary)
				cmd.Dir = repo
				output, err := cmd.CombinedOutput()
				require.NoError(err, string(output))
			}
			for _, path := range paths {
				first, err := os.Stat(filepath.Join(repos[0], path))
				require.NoError(err)
				second, err := os.Stat(filepath.Join(repos[1], path))
				require.NoError(err)
				assert.Equal(first.Size(), second.Size())
				assert.Equal(first.Mode(), second.Mode())
				if tc.edit && path == paths[0] {
					assert.NotEqual(first.ModTime(), second.ModTime())
				} else {
					assert.Equal(first.ModTime(), second.ModTime())
				}
				assert.True(first.ModTime().Before(time.Now().Add(-2 * time.Second)))
				assert.True(second.ModTime().Before(time.Now().Add(-2 * time.Second)))
			}
		})
	}
}
