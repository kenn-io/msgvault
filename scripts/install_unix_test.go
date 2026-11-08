//go:build !windows

package scripts

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallPath(t *testing.T) {
	script, err := filepath.Abs("install.sh")
	require.NoError(t, err)
	_, err = os.ReadFile(script) // Include the child script in Go's test cache inputs.
	require.NoError(t, err)
	bash, err := exec.LookPath("bash")
	require.NoError(t, err)
	fixture := t.TempDir()
	transport := filepath.Join(fixture, "transport")
	require.NoError(t, os.Mkdir(transport, 0o700))
	tools := filepath.Join(fixture, "tools")
	require.NoError(t, os.Mkdir(tools, 0o700))
	for _, name := range strings.Fields("uname mkdir mktemp tar gzip awk sha256sum shasum cut mv chmod grep dirname rm cp id run-parts head tr") {
		path, err := exec.LookPath(name)
		if err == nil {
			require.NoError(t, os.Symlink(path, filepath.Join(tools, name)))
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(fixture, "msgvault"), []byte("#!/bin/sh\nprintf 'installed sentinel\\n'\n"), 0o700))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	archive := filepath.Join(fixture, "release.tar.gz")
	output, err := exec.CommandContext(ctx, "tar", "-czf", archive, "-C", fixture, "msgvault").CombinedOutput()
	require.NoError(t, err, string(output))
	data, err := os.ReadFile(archive)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(transport, "curl"), []byte(fmt.Sprintf(`#!/bin/sh
case "$1:$2" in
    -fsSLI:-o) [ "$6" = https://github.com/kenn-io/msgvault/releases/latest ] || exit 1
        printf https://github.com/kenn-io/msgvault/releases/tag/v1.0.0 ;;
    -fsSL:https://github.com/kenn-io/msgvault/releases/download/v1.0.0/msgvault_1.0.0_*.tar.gz)
        cp "$FIXTURE/release.tar.gz" "$4" ;;
    -fsSL:https://github.com/kenn-io/msgvault/releases/download/v1.0.0/SHA256SUMS)
        printf '%x  msgvault_1.0.0_%%s_%%s.tar.gz\n' "$FIXTURE_OS" "$FIXTURE_ARCH" > "$4" ;;
    *) exit 1 ;;
esac
`, sha256.Sum256(data))), 0o700))
	for _, tc := range []struct {
		name, shell, optOut, pathEntry string
	}{
		{name: "fresh with spaces", shell: "bash"},
		{name: "existing login", shell: "bash"},
		{name: "opt out", shell: "bash", optOut: "1"},
		{name: "exact PATH", shell: "bash", pathEntry: "exact"},
		{name: "sibling PATH", shell: "bash", pathEntry: "sibling"},
		{name: "zsh custom directory", shell: "zsh"},
		{name: "zsh redirects directory", shell: "zsh"},
		{name: "fish custom directory", shell: "fish"},
		{name: "unsupported", shell: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shell, _ := exec.LookPath(tc.shell)
			home := t.TempDir()
			destination := filepath.Join(home, ".local", "bin")
			installDir := destination
			if tc.name == "fresh with spaces" {
				installDir = "bin with spaces"
				destination = filepath.Join(home, installDir)
			}
			path := tools
			if tc.pathEntry == "exact" {
				require.NoError(t, os.MkdirAll(filepath.Dir(destination), 0o700))
				require.NoError(t, os.Mkdir(filepath.Join(home, "target"), 0o700))
				require.NoError(t, os.Symlink(filepath.Join(home, "target"), destination))
				path += ":" + destination
			} else if tc.pathEntry == "sibling" {
				path += ":" + destination + "ary"
			}
			profiles := map[string]string{".bashrc": "# preserved", ".profile": "# preserved"}
			switch tc.shell {
			case "zsh":
				profiles = map[string]string{".zshenv": "# preserved", "zsh/.zshenv": "# preserved"}
				if tc.name == "zsh redirects directory" {
					profiles[".zshenv"] = `export ZDOTDIR="$HOME/zsh"`
				}
			case "fish":
				profiles = map[string]string{"config/fish/config.fish": "# preserved"}
			}
			if tc.name == "existing login" {
				profiles[".bash_profile"] = "# preserved\n. \"$HOME/.bashrc\""
			}
			for profile, content := range profiles {
				if tc.name == "fresh with spaces" {
					profiles[profile] = ""
					continue
				}
				file := filepath.Join(home, profile)
				require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
				require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
			}
			env := []string{"HOME=" + home, "SHELL=/bin/" + tc.shell, "PATH=" + path, "TMPDIR=" + home,
				"MSGVAULT_INSTALL_DIR=" + installDir, "MSGVAULT_NO_MODIFY_PATH=" + tc.optOut,
				"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
				"FIXTURE=" + fixture, "FIXTURE_OS=" + runtime.GOOS, "FIXTURE_ARCH=" + runtime.GOARCH}
			if tc.name == "zsh custom directory" {
				env = append(env, "ZDOTDIR="+filepath.Join(home, "zsh"))
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			for run := range 2 {
				cmd := exec.CommandContext(ctx, bash, script)
				cmd.Env = append(append([]string{}, env...), "PATH="+transport+":"+path)
				if tc.name == "zsh redirects directory" {
					cmd.Env = append(cmd.Env, "ZDOTDIR="+filepath.Join(home, "zsh"))
				}
				cmd.Dir = home
				output, err := cmd.CombinedOutput()
				require.NoError(t, err, string(output))
				assert.Contains(t, string(output), "Checksum verified")
				if tc.optOut != "" || tc.shell == "other" {
					assert.NotContains(t, string(output), "Open a new terminal")
					if tc.shell == "other" {
						assert.Contains(t, string(output), "Configure PATH manually for your shell.")
						assert.Contains(t, string(output), destination+"'/msgvault\n")
					} else {
						assert.Contains(t, string(output), "Run in your current shell:")
					}
				}
				for profile, original := range profiles {
					data, err := os.ReadFile(filepath.Join(home, profile))
					require.NoError(t, err)
					if tc.optOut != "" || tc.pathEntry == "exact" || tc.shell == "other" || (tc.name == "existing login" && profile == ".profile") {
						assert.Equal(t, original, string(data))
						continue
					}
					if run == 0 {
						assert.Contains(t, string(output), filepath.Join(home, profile))
					}
					assert.Regexp(t, "^"+regexp.QuoteMeta(original)+"\n[^\n]+\n$", string(data))
				}
			}
			if tc.optOut != "" || tc.shell == "other" || tc.pathEntry == "exact" {
				return
			}
			probe := `command -v msgvault; msgvault; printf '\nTEST_PATH=%s\n' "$PATH"`
			if tc.shell == "fish" {
				probe = `command -s msgvault; msgvault; printf '\nTEST_PATH=%s\n' (string join : $PATH)`
			}
			starts := [][]string{}
			if tc.shell == "zsh" {
				for profile := range profiles {
					starts = append(starts, []string{bash, "-c", `. "$1"; ` + probe, "source-profile", filepath.Join(home, profile)})
				}
			}
			if shell != "" {
				starts = append(starts, []string{shell, "-ic", probe})
				if tc.shell == "bash" || tc.shell == "zsh" {
					starts = append(starts, []string{shell, "--login", "-ic", probe})
				}
			}
			for _, args := range starts {
				cmd := exec.CommandContext(ctx, args[0], args[1:]...)
				cmd.Env, cmd.Dir = env, fixture
				output, err := cmd.CombinedOutput()
				require.NoError(t, err, string(output))
				assert.Contains(t, string(output), destination+"/msgvault\n")
				assert.Contains(t, string(output), "installed sentinel\n")
				_, value, found := strings.Cut(string(output), "\nTEST_PATH=")
				require.True(t, found, string(output))
				count := 0
				for _, entry := range strings.Split(strings.TrimSpace(value), ":") {
					if entry == destination {
						count++
					}
				}
				assert.Equal(t, 1, count)
			}
			if shell == "" {
				t.Skipf("%s not installed; startup not checked", tc.shell)
			}
		})
	}
}
