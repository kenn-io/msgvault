package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var msgvaultStaticAssets = []string{
	"favicon-192.png",
	"favicon-512.png",
	"favicon.svg",
	"how-it-works.svg",
	"oauth-multi-account.svg",
	"og-image.png",
	"og-image.svg",
}

var msgvaultGeneratedAssets = []string{
	"concepts/account-collection-concept.png",
	"concepts/deduplication-concept.png",
	"concepts/oauth-multi-account-concept.png",
	"concepts/safety-ladder-concept.png",
	"concepts/survivor-selection-concept.png",
	"list-senders.svg",
	"stats.svg",
	"tui-all-messages.svg",
	"tui-deletion.svg",
	"tui-domains.svg",
	"tui-drilldown.svg",
	"tui-filter-modal.svg",
	"tui-labels.svg",
	"tui-message-detail.svg",
	"tui-search-drilldown.svg",
	"tui-search-sender.svg",
	"tui-search-subject.svg",
	"tui-selection.svg",
	"tui-senders.svg",
	"tui-subgroup-recipients.svg",
	"tui-subgroup-time.svg",
	"tui-thread.svg",
	"tui-time-daily.svg",
	"tui-time-monthly.svg",
	"tui-time-yearly.svg",
	"tui-time.svg",
}

func TestHydrateAssetsForceFetchesRemoteAssetBranches(t *testing.T) {
	tempDir := t.TempDir()
	remoteRepo := filepath.Join(tempDir, "remote")
	localRepo := filepath.Join(tempDir, "local")
	require.NoError(t, os.MkdirAll(remoteRepo, 0o755))
	require.NoError(t, os.MkdirAll(localRepo, 0o755))

	git(t, remoteRepo, "init")
	git(t, remoteRepo, "config", "user.name", "Test User")
	git(t, remoteRepo, "config", "user.email", "test@example.invalid")
	writeStaticAssets(t, remoteRepo, "old static")
	git(t, remoteRepo, "add", ".")
	git(t, remoteRepo, "commit", "-m", "old static assets")
	oldStaticCommit := gitOutput(t, remoteRepo, "rev-parse", "HEAD")
	git(t, remoteRepo, "branch", "docs-assets")

	git(t, localRepo, "init")
	git(t, localRepo, "remote", "add", "origin", remoteRepo)
	git(t, localRepo, "fetch", "origin", "docs-assets:refs/remotes/origin/docs-assets")

	git(t, remoteRepo, "checkout", "--orphan", "new-static-assets")
	clearWorkingTree(t, remoteRepo)
	writeStaticAssets(t, remoteRepo, "new static")
	git(t, remoteRepo, "add", "-A")
	git(t, remoteRepo, "commit", "-m", "new static assets")
	newStaticCommit := gitOutput(t, remoteRepo, "rev-parse", "HEAD")
	git(t, remoteRepo, "update-ref", "refs/heads/docs-assets", newStaticCommit)
	require.NotEqual(t, oldStaticCommit, newStaticCommit)

	git(t, remoteRepo, "checkout", "--orphan", "generated-assets")
	clearWorkingTree(t, remoteRepo)
	writeGeneratedAssets(t, remoteRepo, "generated")
	git(t, remoteRepo, "add", "-A")
	git(t, remoteRepo, "commit", "-m", "generated assets")
	generatedCommit := gitOutput(t, remoteRepo, "rev-parse", "HEAD")
	git(t, remoteRepo, "update-ref", "refs/heads/docs-generated-assets", generatedCommit)

	docsAssetsDir := filepath.Join(localRepo, "docs", "assets")
	require.NoError(t, os.MkdirAll(docsAssetsDir, 0o755))
	writeStaticAssets(t, filepath.Join(docsAssetsDir, "static"), "stale local static")
	writeGeneratedAssets(t, filepath.Join(docsAssetsDir, "generated"), "stale local generated")

	scriptPath := installScript(t, localRepo, filepath.Join("docs", "assets", "hydrate-assets.sh"))
	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = localRepo
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	assert.Equal(t, newStaticCommit, gitOutput(t, localRepo, "rev-parse", "refs/remotes/origin/docs-assets"))
	assert.Equal(t, generatedCommit, gitOutput(t, localRepo, "rev-parse", "refs/remotes/origin/docs-generated-assets"))
	assertAssetFilesHaveContent(t, filepath.Join(docsAssetsDir, "static"), msgvaultStaticAssets, "new static")
	assertAssetFilesHaveContent(t, filepath.Join(docsAssetsDir, "generated"), msgvaultGeneratedAssets, "generated")
}

func TestAssetPublishersRejectUnexpectedFiles(t *testing.T) {
	cases := []struct {
		name      string
		scriptRel string
		write     func(*testing.T, string, string)
	}{
		{
			name:      "static",
			scriptRel: filepath.Join("docs", "assets", "update-static-assets-branch.sh"),
			write:     writeStaticAssets,
		},
		{
			name:      "generated",
			scriptRel: filepath.Join("docs", "screenshots", "update-generated-assets-branch.sh"),
			write:     writeGeneratedAssets,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			repo := filepath.Join(tempDir, "repo")
			sourceDir := filepath.Join(tempDir, "source")
			require.NoError(t, os.MkdirAll(repo, 0o755))
			git(t, repo, "init")
			git(t, repo, "config", "user.name", "Test User")
			git(t, repo, "config", "user.email", "test@example.invalid")
			tc.write(t, sourceDir, "asset")
			require.NoError(t, os.WriteFile(filepath.Join(sourceDir, ".env.local"), []byte("TOKEN=secret\n"), 0o600))

			scriptPath := installScript(t, repo, tc.scriptRel)
			cmd := exec.Command("bash", scriptPath, "--source", sourceDir)
			cmd.Dir = repo
			output, err := cmd.CombinedOutput()

			require.Error(t, err, string(output))
			assert.Contains(t, strings.ToLower(string(output)), "unexpected")
			assert.Contains(t, string(output), ".env.local")
		})
	}
}

func TestAssetPublishersRejectSymlinks(t *testing.T) {
	cases := []struct {
		name        string
		scriptRel   string
		write       func(*testing.T, string, string)
		symlinkPath string
	}{
		{
			name:        "static",
			scriptRel:   filepath.Join("docs", "assets", "update-static-assets-branch.sh"),
			write:       writeStaticAssets,
			symlinkPath: "favicon.svg",
		},
		{
			name:        "generated",
			scriptRel:   filepath.Join("docs", "screenshots", "update-generated-assets-branch.sh"),
			write:       writeGeneratedAssets,
			symlinkPath: "tui-filter-modal.svg",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			repo := filepath.Join(tempDir, "repo")
			sourceDir := filepath.Join(tempDir, "source")
			require.NoError(t, os.MkdirAll(repo, 0o755))
			git(t, repo, "init")
			git(t, repo, "config", "user.name", "Test User")
			git(t, repo, "config", "user.email", "test@example.invalid")
			tc.write(t, sourceDir, "asset")

			target := filepath.Join(tempDir, "symlink-target")
			require.NoError(t, os.WriteFile(target, []byte("not an asset\n"), 0o644))
			link := filepath.Join(sourceDir, tc.symlinkPath)
			require.NoError(t, os.Remove(link))
			require.NoError(t, os.Symlink(target, link))

			scriptPath := installScript(t, repo, tc.scriptRel)
			cmd := exec.Command("bash", scriptPath, "--source", sourceDir)
			cmd.Dir = repo
			output, err := cmd.CombinedOutput()

			require.Error(t, err, string(output))
			assert.Contains(t, strings.ToLower(string(output)), "symlink")
		})
	}
}

func installScript(t *testing.T, repo, scriptRel string) string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", scriptRel))
	require.NoError(t, err)
	scriptPath := filepath.Join(repo, scriptRel)
	require.NoError(t, os.MkdirAll(filepath.Dir(scriptPath), 0o755))
	require.NoError(t, os.WriteFile(scriptPath, script, 0o755))
	return scriptPath
}

func writeStaticAssets(t *testing.T, dir, content string) {
	t.Helper()
	writeAssetFiles(t, dir, msgvaultStaticAssets, content)
}

func writeGeneratedAssets(t *testing.T, dir, content string) {
	t.Helper()
	writeAssetFiles(t, dir, msgvaultGeneratedAssets, content)
}

func writeAssetFiles(t *testing.T, dir string, files []string, content string) {
	t.Helper()
	for _, file := range files {
		path := filepath.Join(dir, file)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content+"\n"), 0o644))
	}
}

func assertAssetFilesHaveContent(t *testing.T, dir string, files []string, want string) {
	t.Helper()
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join(dir, file))
		require.NoError(t, err, file)
		assert.Equal(t, want, strings.TrimRight(string(content), "\r\n"), file)
	}
}

func clearWorkingTree(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		require.NoError(t, os.RemoveAll(filepath.Join(dir, entry.Name())))
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(output))
}
