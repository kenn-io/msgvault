package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChangelogSkipsDocumentationOnlyChanges(t *testing.T) {
	requireT := require.New(t)

	tempDir := t.TempDir()
	repo := filepath.Join(tempDir, "repo")
	binDir := filepath.Join(tempDir, "bin")
	requireT.NoError(os.MkdirAll(repo, 0o755))
	requireT.NoError(os.MkdirAll(binDir, 0o755))

	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Test User")
	git(t, repo, "config", "user.email", "test@example.invalid")

	writeAssetFile(t, repo, "cmd/msgvault/main.go", "package main")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "seed")
	git(t, repo, "tag", "v0.1.0")

	writeAssetFile(t, repo, "docs/usage.md", "# Usage docs")
	writeAssetFile(t, repo, "README.md", "# README docs")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "docs: expand release documentation")

	writeAssetFile(t, repo, "internal/api/deletions.go", "package api")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "Add deletion staging endpoints")

	scriptPath := installScript(t, repo, filepath.Join("scripts", "changelog.sh"))
	writeExecutableFile(t, filepath.Join(binDir, "codex"), fakeCodexPromptEchoer())

	for _, dir := range []string{repo, filepath.Join(repo, "scripts")} {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			cmd := exec.Command("bash", scriptPath, "NEXT", "v0.1.0")
			cmd.Dir = dir
			cmd.Env = envWithPath(binDir + string(os.PathListSeparator) + os.Getenv("PATH"))
			output, err := cmd.CombinedOutput()
			require.NoError(err, string(output))

			text := string(output)
			assert.Contains(text, "Add deletion staging endpoints")
			assert.Contains(text, "internal/api/deletions.go")
			assert.NotContains(text, "docs: expand release documentation")
			assert.NotContains(text, "docs/usage.md")
			assert.NotContains(text, "README.md")
		})
	}
}

func fakeCodexPromptEchoer() string {
	return `#!/usr/bin/env bash
set -euo pipefail

output=""
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    -o)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

if [[ -z "$output" ]]; then
  printf 'missing -o output path\n' >&2
  exit 1
fi

cat > "$output"
`
}
