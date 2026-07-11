package skills

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectAgents(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(home, ".claude"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(home, ".codex"), 0o755))

	t.Run("detects both", func(t *testing.T) {
		got, err := DetectAgents(home, nil)
		require.NoError(t, err)
		want := []AgentDir{
			{Agent: "claude", Dir: filepath.Join(home, ".claude", "skills")},
			{Agent: "codex", Dir: filepath.Join(home, ".codex", "skills")},
		}
		assert.Equal(t, want, got)
	})

	t.Run("filter restricts", func(t *testing.T) {
		got, err := DetectAgents(home, []string{"codex"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "codex", got[0].Agent)
	})

	t.Run("unknown agent errors", func(t *testing.T) {
		_, err := DetectAgents(home, []string{"cursor"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cursor")
	})

	t.Run("missing dirs yield nothing", func(t *testing.T) {
		got, err := DetectAgents(t.TempDir(), nil)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func testSkills() []Skill {
	return []Skill{
		{Name: "msgvault-search", Content: "search body\n<!-- " + Marker + " v1 -->\n"},
		{Name: "msgvault-analytics", Content: "analytics body\n<!-- " + Marker + " v1 -->\n"},
	}
}

func TestInstall_FreshAndUpdate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()

	results, err := Install(root, testSkills(), false)
	require.NoError(err)
	require.Len(results, 2)
	for _, r := range results {
		assert.Equal(StatusInstalled, r.Status)
		content, err := os.ReadFile(r.Path)
		require.NoError(err)
		assert.Contains(string(content), Marker)
	}

	// Re-install over marker-bearing files: updated, not skipped.
	results, err = Install(root, testSkills(), false)
	require.NoError(err)
	for _, r := range results {
		assert.Equal(StatusUpdated, r.Status)
	}
}

func TestInstall_SkipsHandEditedUnlessForced(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	_, err := Install(root, testSkills(), false)
	require.NoError(err)

	edited := filepath.Join(root, "msgvault-search", "SKILL.md")
	require.NoError(os.WriteFile(edited, []byte("my custom skill\n"), 0o644))

	results, err := Install(root, testSkills(), false)
	require.NoError(err)
	byName := map[string]InstallResult{}
	for _, r := range results {
		byName[r.Skill] = r
	}
	assert.Equal(StatusSkipped, byName["msgvault-search"].Status)
	assert.Equal(StatusUpdated, byName["msgvault-analytics"].Status)
	content, err := os.ReadFile(edited)
	require.NoError(err)
	assert.Equal("my custom skill\n", string(content),
		"hand-edited file must not be touched")

	results, err = Install(root, testSkills(), true)
	require.NoError(err)
	for _, r := range results {
		assert.Equal(StatusUpdated, r.Status, "--force overwrites")
	}
}

func TestUninstall_RemovesOnlyMarkerDirs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	_, err := Install(root, testSkills(), false)
	require.NoError(err)

	// A hand-authored skill that happens to match the name prefix.
	custom := filepath.Join(root, "msgvault-custom")
	require.NoError(os.MkdirAll(custom, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(custom, "SKILL.md"), []byte("mine\n"), 0o644))
	// An unrelated skill directory.
	other := filepath.Join(root, "other-skill")
	require.NoError(os.MkdirAll(other, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(other, "SKILL.md"), []byte("other\n"), 0o644))

	removed, err := Uninstall(root)
	require.NoError(err)
	assert.Len(removed, 2)
	assert.NoDirExists(filepath.Join(root, "msgvault-search"))
	assert.NoDirExists(filepath.Join(root, "msgvault-analytics"))
	assert.DirExists(custom, "non-marker msgvault-* dir kept")
	assert.DirExists(other, "unrelated skill kept")
}

func TestUninstall_PreservesUserFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	_, err := Install(root, testSkills(), false)
	require.NoError(err)

	// User adds a supporting file next to the generated SKILL.md.
	searchDir := filepath.Join(root, "msgvault-search")
	userFile := filepath.Join(searchDir, "notes.md")
	require.NoError(os.WriteFile(userFile, []byte("my notes\n"), 0o644))

	removed, err := Uninstall(root)
	require.NoError(err)
	assert.NoFileExists(filepath.Join(searchDir, "SKILL.md"),
		"generated SKILL.md removed")
	assert.FileExists(userFile, "user-added file preserved")
	assert.DirExists(searchDir, "dir with user files kept")
	assert.NoDirExists(filepath.Join(root, "msgvault-analytics"),
		"empty skill dir fully removed")
	assert.Contains(removed, filepath.Join(searchDir, "SKILL.md"))
	assert.Contains(removed, filepath.Join(root, "msgvault-analytics"))
}

func TestUninstall_EmptyRoot(t *testing.T) {
	removed, err := Uninstall(filepath.Join(t.TempDir(), "nope"))
	require.NoError(t, err)
	assert.Empty(t, removed)
}

func TestUninstall_RootWithGlobMetacharacters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "sk[1]lls")
	_, err := Install(root, testSkills(), false)
	require.NoError(err)

	removed, err := Uninstall(root)
	require.NoError(err, "a [ in the root path must not break uninstall")
	assert.Len(removed, 2)
	assert.NoDirExists(filepath.Join(root, "msgvault-search"))
}

func TestUninstall_GlobRootDoesNotEscape(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	parent := t.TempDir()
	neighbor := filepath.Join(parent, "real-skills")
	_, err := Install(neighbor, testSkills(), false)
	require.NoError(err)

	// A root literally named "*" must not expand to neighboring dirs.
	removed, err := Uninstall(filepath.Join(parent, "*"))
	if runtime.GOOS == "windows" {
		// '*' is an invalid path character on Windows; Uninstall
		// fails fast instead of expanding it.
		require.Error(err)
	} else {
		require.NoError(err)
	}
	assert.Empty(removed)
	assert.FileExists(
		filepath.Join(neighbor, "msgvault-search", "SKILL.md"),
		"skills under a sibling directory must be untouched")
}
