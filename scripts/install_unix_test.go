//go:build !windows

package scripts

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	require.NoError(t, os.WriteFile(filepath.Join(fixture, "msgvault"), []byte("#!/bin/sh\nprintf 'installed sentinel\\n'\n"), 0o700))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	archive := filepath.Join(fixture, "release.tar.gz")
	output, err := exec.CommandContext(ctx, "tar", "-czf", archive, "-C", fixture, "msgvault").CombinedOutput()
	require.NoError(t, err, string(output))
	data, err := os.ReadFile(archive)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(fixture, "curl"), []byte(fmt.Sprintf(`#!/bin/sh
if [ "$1" = -fsSLI ] && [ "$6" = https://github.com/kenn-io/msgvault/releases/latest ]; then
    printf https://github.com/kenn-io/msgvault/releases/tag/v1.0.0
elif [ "$1" = -fsSL ] && [ "$3" = -o ]; then
    case "$2" in
        https://github.com/kenn-io/msgvault/releases/download/v1.0.0/msgvault_1.0.0_*.tar.gz) cp "$FIXTURE/release.tar.gz" "$4" ;;
        https://github.com/kenn-io/msgvault/releases/download/v1.0.0/SHA256SUMS)
            printf '%x  msgvault_1.0.0_%%s_%%s.tar.gz\n' "$FIXTURE_OS" "$FIXTURE_ARCH" > "$4" ;;
        *) exit 1 ;;
    esac
else
    exit 1
fi
`, sha256.Sum256(data))), 0o700))
	for _, tc := range []struct {
		name, shell, optOut, pathEntry, loginFile string
	}{
		{name: "bash fresh", shell: "bash"},
		{name: "bash profile", shell: "bash", loginFile: ".bash_profile"},
		{name: "bash login", shell: "bash", loginFile: ".bash_login"},
		{name: "opt out", shell: "bash", optOut: "1"},
		{name: "opt out true", shell: "bash", optOut: "TrUe"},
		{name: "opt out yes", shell: "bash", optOut: "YeS"},
		{name: "exact PATH", shell: "bash", pathEntry: "exact"},
		{name: "sibling PATH", shell: "bash", pathEntry: "sibling"},
		{name: "spaces and quotes", shell: "bash"},
		{name: "POSIX", shell: "sh"},
		{name: "zsh custom directory", shell: "zsh"},
		{name: "fish custom directory", shell: "fish"},
		{name: "unknown shell", shell: "tcsh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shell, err := exec.LookPath(tc.shell)
			if (tc.shell == "zsh" || tc.shell == "fish") && err != nil {
				t.Skipf("%s is unavailable", tc.shell)
			}
			home := t.TempDir()
			destination := filepath.Join(home, ".local", "bin")
			if tc.name == "spaces and quotes" {
				destination = filepath.Join(home, "bin with 'quotes' $dollars [glob]")
			}
			path := fixture + ":" + os.Getenv("PATH")
			if tc.pathEntry == "exact" {
				path += ":" + destination
			} else if tc.pathEntry == "sibling" {
				path += ":" + destination + "ary"
			}
			profiles := []string{".bashrc", ".profile"}
			if tc.loginFile != "" {
				profiles[1] = tc.loginFile
			}
			switch tc.shell {
			case "zsh":
				profiles = []string{"zsh/.zshrc"}
			case "fish":
				profiles = []string{"config/fish/config.fish"}
			case "sh":
				profiles = []string{".profile"}
			}
			original := "# existing profile content"
			if tc.name == "bash fresh" {
				original = ""
			}
			for _, profile := range profiles {
				if tc.name == "bash fresh" {
					continue
				}
				file := filepath.Join(home, profile)
				require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
				contents := original
				if tc.loginFile != "" && profile == tc.loginFile {
					contents += "\n. \"$HOME/.bashrc\""
				}
				require.NoError(t, os.WriteFile(file, []byte(contents), 0o600))
			}
			env := []string{"HOME=" + home, "SHELL=/bin/" + tc.shell, "PATH=" + path, "TMPDIR=" + home,
				"MSGVAULT_INSTALL_DIR=" + destination, "MSGVAULT_NO_MODIFY_PATH=" + tc.optOut,
				"ZDOTDIR=" + filepath.Join(home, "zsh"), "XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
				"FIXTURE=" + fixture, "FIXTURE_OS=" + runtime.GOOS, "FIXTURE_ARCH=" + runtime.GOARCH}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			var firstProfiles []string
			for range 2 {
				cmd := exec.CommandContext(ctx, bash, script)
				cmd.Env = env
				output, err := cmd.CombinedOutput()
				require.NoError(t, err, string(output))
				assert.Contains(t, string(output), "Checksum verified")
				var contents []string
				for _, profile := range profiles {
					data, err := os.ReadFile(filepath.Join(home, profile))
					require.NoError(t, err)
					assert.True(t, strings.HasPrefix(string(data), original))
					contents = append(contents, string(data))
					if tc.optOut != "" || tc.pathEntry == "exact" || tc.shell == "tcsh" {
						assert.Equal(t, original, string(data))
					} else {
						if firstProfiles == nil {
							assert.Contains(t, string(output), filepath.Join(home, profile))
						}
						wantLines := 2
						if original == "" {
							wantLines = 1
						}
						if profile == tc.loginFile {
							wantLines++
						}
						assert.Len(t, strings.Split(strings.TrimSpace(string(data)), "\n"), wantLines)
					}
				}
				if firstProfiles != nil {
					assert.Equal(t, firstProfiles, contents, "repeat install preserves startup files")
				}
				firstProfiles = contents
			}
			if tc.optOut != "" || tc.shell == "tcsh" {
				return
			}
			starts := [][]string{{"-ic", "msgvault"}}
			if tc.shell == "bash" {
				starts = append(starts, []string{"--login", "-c", "msgvault"})
			} else if tc.shell == "sh" {
				starts = [][]string{{"-c", ". \"$HOME/.profile\"; msgvault"}}
			}
			for _, args := range starts {
				args[len(args)-1] += `; printf '\n%s\n' "$PATH"`
				cmd := exec.CommandContext(ctx, shell, args...)
				cmd.Env = env
				output, err := cmd.CombinedOutput()
				require.NoError(t, err, string(output))
				assert.Contains(t, string(output), "installed sentinel\n")
				count := 1
				if tc.pathEntry == "sibling" {
					count++
				}
				assert.Equal(t, count, strings.Count(string(output), destination), "startup adds the PATH entry once")
			}
		})
	}
}
