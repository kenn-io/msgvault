package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

type publisherScriptCase struct {
	name        string
	scriptRel   string
	branch      string
	branchEnv   string
	write       func(*testing.T, string, string)
	symlinkPath string
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
	writeAssetFile(t, filepath.Join(docsAssetsDir, "static"), "stale-static.svg", "stale static extra")
	writeAssetFile(t, filepath.Join(docsAssetsDir, "generated"), "stale-generated.svg", "stale generated extra")

	scriptPath := installScript(t, localRepo, filepath.Join("docs", "assets", "hydrate-assets.sh"))
	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = localRepo
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	assert.Equal(t, newStaticCommit, gitOutput(t, localRepo, "rev-parse", "refs/remotes/origin/docs-assets"))
	assert.Equal(t, generatedCommit, gitOutput(t, localRepo, "rev-parse", "refs/remotes/origin/docs-generated-assets"))
	assertRecursiveFileList(t, filepath.Join(docsAssetsDir, "static"), msgvaultStaticAssets)
	assertRecursiveFileList(t, filepath.Join(docsAssetsDir, "generated"), msgvaultGeneratedAssets)
	assertAssetFilesHaveContent(t, filepath.Join(docsAssetsDir, "static"), msgvaultStaticAssets, "new static")
	assertAssetFilesHaveContent(t, filepath.Join(docsAssetsDir, "generated"), msgvaultGeneratedAssets, "generated")
}

func TestAssetPublishersRejectUnexpectedFiles(t *testing.T) {
	for _, tc := range publisherScriptCases() {
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
			beforeRef, beforeExists := gitRef(t, repo, tc.branch)

			scriptPath := installScript(t, repo, tc.scriptRel)
			cmd := exec.Command("bash", scriptPath, "--source", sourceDir)
			cmd.Dir = repo
			output, err := cmd.CombinedOutput()

			require.Error(t, err, string(output))
			assert.Contains(t, strings.ToLower(string(output)), "unexpected")
			assert.Contains(t, string(output), ".env.local")
			assertBranchRefUnchanged(t, repo, tc.branch, beforeRef, beforeExists)
		})
	}
}

func TestAssetPublishersRejectSymlinks(t *testing.T) {
	for _, tc := range publisherScriptCases() {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			repo := filepath.Join(tempDir, "repo")
			sourceDir := filepath.Join(tempDir, "source")
			require.NoError(t, os.MkdirAll(repo, 0o755))
			git(t, repo, "init")
			git(t, repo, "config", "user.name", "Test User")
			git(t, repo, "config", "user.email", "test@example.invalid")
			tc.write(t, sourceDir, "asset")
			beforeRef := seedBranchRef(t, repo, tc.branch)

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
			assertBranchRefUnchanged(t, repo, tc.branch, beforeRef, true)
		})
	}
}

func TestAssetPublishersRejectProtectedBranchNames(t *testing.T) {
	for _, tc := range publisherScriptCases() {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			repo := filepath.Join(tempDir, "repo")
			sourceDir := filepath.Join(tempDir, "source")
			require.NoError(t, os.MkdirAll(repo, 0o755))
			git(t, repo, "init")
			git(t, repo, "config", "user.name", "Test User")
			git(t, repo, "config", "user.email", "test@example.invalid")
			tc.write(t, sourceDir, "asset")

			require.NoError(t, os.WriteFile(filepath.Join(repo, "main-seed.txt"), []byte("main\n"), 0o644))
			git(t, repo, "add", ".")
			git(t, repo, "commit", "-m", "seed main")
			mainRef := gitOutput(t, repo, "rev-parse", "HEAD")
			git(t, repo, "update-ref", "refs/heads/main", mainRef)
			git(t, repo, "checkout", "-b", "work")

			scriptPath := installScript(t, repo, tc.scriptRel)
			cmd := exec.Command("bash", scriptPath, "--source", sourceDir)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), tc.branchEnv+"=main")
			output, err := cmd.CombinedOutput()

			require.Error(t, err, string(output))
			assert.Contains(t, strings.ToLower(string(output)), "protected")
			assert.Contains(t, string(output), "main")
			assertBranchRefUnchanged(t, repo, "main", mainRef, true)
		})
	}
}

func TestCheckDocsRejectsForbiddenMediaReferenceOnLineWithAllowedAsset(t *testing.T) {
	runCheckDocsMediaReferenceTest(
		t,
		"![bad](/favicon.svg) ![ok](/assets/static/favicon.svg)",
		"docs media references must use /assets/static or /assets/generated",
	)
}

func TestCheckDocsRejectsForbiddenSourceMediaReferenceOnLineWithAllowedAsset(t *testing.T) {
	runCheckDocsMediaReferenceTest(
		t,
		`logo = "/favicon.svg" favicon = "assets/static/favicon.svg"`,
		"docs source media references must use /assets/static or /assets/generated",
	)
}

func TestDocsScreenshotDemoDataUsesIntegratedRepoSchemaPath(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "docs", "screenshots", "generate_demo_data.py"))
	require.NoError(t, err)

	text := string(content)
	assert.Contains(t, text, `SCRIPT_DIR / "../../internal/store/schema.sql"`)
	assert.NotContains(t, text, `SCRIPT_DIR / "../../msgvault/internal/store/schema.sql"`)
}

func runCheckDocsMediaReferenceTest(t *testing.T, docsLine, wantMessage string) {
	t.Helper()
	tempDir := t.TempDir()
	repo := filepath.Join(tempDir, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Test User")
	git(t, repo, "config", "user.email", "test@example.invalid")

	scriptPath := installScript(t, repo, filepath.Join("scripts", "check-docs.sh"))
	writeAssetFile(
		t,
		filepath.Join(repo, "docs"),
		"index.md",
		docsLine,
	)
	writeAssetFile(t, repo, "README.md", "# test")
	writeExecutableFile(
		t,
		filepath.Join(repo, "docs", "scripts", "check_markdown_sources.py"),
		"#!/usr/bin/env python3\nprint('docs markdown source checks passed')\n",
	)
	writeExecutableFile(
		t,
		filepath.Join(repo, "docs", "assets", "hydrate-assets.sh"),
		"#!/usr/bin/env bash\necho 'hydrate should not run' >&2\nexit 1\n",
	)

	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()

	require.Error(t, err, string(output))
	assert.Contains(t, string(output), wantMessage)
	assert.Contains(t, string(output), "/favicon.svg")
	assert.NotContains(t, string(output), "hydrate should not run")
}

func installScript(t *testing.T, repo, scriptRel string) string {
	t.Helper()
	sourceScriptPath := filepath.Join("..", scriptRel)
	info, err := os.Stat(sourceScriptPath)
	require.NoError(t, err)
	require.False(t, info.IsDir(), sourceScriptPath)

	for _, relDir := range supportDirsForScript(scriptRel) {
		copySupportFiles(t, repo, relDir)
	}

	scriptPath := filepath.Join(repo, scriptRel)
	require.FileExists(t, scriptPath)
	return scriptPath
}

func publisherScriptCases() []publisherScriptCase {
	return []publisherScriptCase{
		{
			name:        "static",
			scriptRel:   filepath.Join("docs", "assets", "update-static-assets-branch.sh"),
			branch:      "docs-assets",
			branchEnv:   "MSGVAULT_DOCS_ASSETS_BRANCH",
			write:       writeStaticAssets,
			symlinkPath: "favicon.svg",
		},
		{
			name:        "generated",
			scriptRel:   filepath.Join("docs", "screenshots", "update-generated-assets-branch.sh"),
			branch:      "docs-generated-assets",
			branchEnv:   "MSGVAULT_DOCS_GENERATED_ASSETS_BRANCH",
			write:       writeGeneratedAssets,
			symlinkPath: "tui-filter-modal.svg",
		},
	}
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
		writeAssetFile(t, dir, file, content)
	}
}

func writeAssetFile(t *testing.T, dir, file, content string) {
	t.Helper()
	path := filepath.Join(dir, file)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content+"\n"), 0o644))
}

func writeExecutableFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
}

func assertRecursiveFileList(t *testing.T, dir string, want []string) {
	t.Helper()
	got := recursiveFileList(t, dir)
	expected := append([]string(nil), want...)
	sort.Strings(expected)
	assert.Equal(t, expected, got)
}

func assertAssetFilesHaveContent(t *testing.T, dir string, files []string, want string) {
	t.Helper()
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join(dir, file))
		require.NoError(t, err, file)
		assert.Equal(t, want, strings.TrimRight(string(content), "\r\n"), file)
	}
}

func recursiveFileList(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		require.NoError(t, err)
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		require.NoError(t, err)
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	sort.Strings(files)
	return files
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

func supportDirsForScript(scriptRel string) []string {
	dirs := []string{filepath.Dir(scriptRel)}
	if filepath.Dir(scriptRel) == filepath.Join("docs", "screenshots") {
		dirs = append(dirs, filepath.Join("docs", "assets"))
	}
	return dirs
}

func copySupportFiles(t *testing.T, repo, relDir string) {
	t.Helper()
	sourceDir := filepath.Join("..", relDir)
	if _, err := os.Stat(sourceDir); os.IsNotExist(err) {
		return
	}

	err := filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, err error) error {
		require.NoError(t, err)
		if entry.IsDir() {
			switch entry.Name() {
			case ".venv", ".vercel", "demo-data", "generated", "site", "static":
				return filepath.SkipDir
			default:
				return nil
			}
		}
		if !isSupportFile(path) {
			return nil
		}

		rel, err := filepath.Rel("..", path)
		require.NoError(t, err)
		info, err := entry.Info()
		require.NoError(t, err)
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		dest := filepath.Join(repo, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
		require.NoError(t, os.WriteFile(dest, content, info.Mode().Perm()))
		return nil
	})
	require.NoError(t, err)
}

func isSupportFile(path string) bool {
	switch filepath.Ext(path) {
	case ".bash", ".py", ".sh":
		return true
	default:
		return false
	}
}

func seedBranchRef(t *testing.T, repo, branch string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "branch-seed.txt"), []byte(branch+"\n"), 0o644))
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "seed branch ref")
	commit := gitOutput(t, repo, "rev-parse", "HEAD")
	git(t, repo, "branch", branch, commit)
	return commit
}

func assertBranchRefUnchanged(t *testing.T, repo, branch, beforeRef string, beforeExists bool) {
	t.Helper()
	afterRef, afterExists := gitRef(t, repo, branch)
	if !beforeExists {
		assert.False(t, afterExists, branch)
		return
	}
	require.True(t, afterExists, branch)
	assert.Equal(t, beforeRef, afterRef, branch)
}

func gitRef(t *testing.T, dir, ref string) (string, bool) {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", ref)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(output)), true
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
