package skills

import (
	"os"
	"path/filepath"
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
	root := t.TempDir()

	results, err := Install(root, testSkills(), false)
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, r := range results {
		assert.Equal(t, StatusInstalled, r.Status)
		content, err := os.ReadFile(r.Path)
		require.NoError(t, err)
		assert.Contains(t, string(content), Marker)
	}

	// Re-install over marker-bearing files: updated, not skipped.
	results, err = Install(root, testSkills(), false)
	require.NoError(t, err)
	for _, r := range results {
		assert.Equal(t, StatusUpdated, r.Status)
	}
}

func TestInstall_SkipsHandEditedUnlessForced(t *testing.T) {
	root := t.TempDir()
	_, err := Install(root, testSkills(), false)
	require.NoError(t, err)

	edited := filepath.Join(root, "msgvault-search", "SKILL.md")
	require.NoError(t, os.WriteFile(edited, []byte("my custom skill\n"), 0o644))

	results, err := Install(root, testSkills(), false)
	require.NoError(t, err)
	byName := map[string]InstallResult{}
	for _, r := range results {
		byName[r.Skill] = r
	}
	assert.Equal(t, StatusSkipped, byName["msgvault-search"].Status)
	assert.Equal(t, StatusUpdated, byName["msgvault-analytics"].Status)
	content, err := os.ReadFile(edited)
	require.NoError(t, err)
	assert.Equal(t, "my custom skill\n", string(content),
		"hand-edited file must not be touched")

	results, err = Install(root, testSkills(), true)
	require.NoError(t, err)
	for _, r := range results {
		assert.Equal(t, StatusUpdated, r.Status, "--force overwrites")
	}
}

func TestUninstall_RemovesOnlyMarkerDirs(t *testing.T) {
	root := t.TempDir()
	_, err := Install(root, testSkills(), false)
	require.NoError(t, err)

	// A hand-authored skill that happens to match the name prefix.
	custom := filepath.Join(root, "msgvault-custom")
	require.NoError(t, os.MkdirAll(custom, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(custom, "SKILL.md"), []byte("mine\n"), 0o644))
	// An unrelated skill directory.
	other := filepath.Join(root, "other-skill")
	require.NoError(t, os.MkdirAll(other, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(other, "SKILL.md"), []byte("other\n"), 0o644))

	removed, err := Uninstall(root)
	require.NoError(t, err)
	assert.Len(t, removed, 2)
	assert.NoDirExists(t, filepath.Join(root, "msgvault-search"))
	assert.NoDirExists(t, filepath.Join(root, "msgvault-analytics"))
	assert.DirExists(t, custom, "non-marker msgvault-* dir kept")
	assert.DirExists(t, other, "unrelated skill kept")
}

func TestUninstall_EmptyRoot(t *testing.T) {
	removed, err := Uninstall(filepath.Join(t.TempDir(), "nope"))
	require.NoError(t, err)
	assert.Empty(t, removed)
}
