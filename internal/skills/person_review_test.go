package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonReviewLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	rendered, err := Render("person-review-test")
	require.NoError(err)
	var selected []Skill
	for _, skill := range rendered {
		if skill.Name == "msgvault-person-review" {
			selected = append(selected, skill)
		}
	}
	require.Len(selected, 1, "person review must ship in the embedded skill pack")

	root := filepath.Join(t.TempDir(), "skills [local] café")
	path := filepath.Join(root, selected[0].Name, skillFileName)
	results, err := Install(root, selected, false)
	require.NoError(err)
	require.Len(results, 1)
	assert.Equal(StatusInstalled, results[0].Status)
	content, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(selected[0].Content, string(content))

	// A local companion must survive both managed updates and uninstall.
	companion := filepath.Join(filepath.Dir(path), "local-notes.md")
	require.NoError(os.WriteFile(companion, []byte("synthetic local notes"), 0o600))
	results, err = Install(root, selected, false)
	require.NoError(err)
	require.Len(results, 1)
	assert.Equal(StatusUpdated, results[0].Status)

	// Removing the generation marker opts a hand-edited skill out of updates.
	manual := "---\nname: msgvault-person-review\ndescription: Local review instructions\n---\nMy local skill.\n"
	require.NoError(os.WriteFile(path, []byte(manual), 0o600))
	results, err = Install(root, selected, false)
	require.NoError(err)
	require.Len(results, 1)
	assert.Equal(StatusSkipped, results[0].Status)
	removed, err := Uninstall(root)
	require.NoError(err)
	assert.Empty(removed)
	content, err = os.ReadFile(path)
	require.NoError(err)
	assert.Equal(manual, string(content))

	results, err = Install(root, selected, true)
	require.NoError(err)
	require.Len(results, 1)
	assert.Equal(StatusUpdated, results[0].Status)
	content, err = os.ReadFile(path)
	require.NoError(err)
	assert.Equal(selected[0].Content, string(content))
	removed, err = Uninstall(root)
	require.NoError(err)
	assert.Equal([]string{path}, removed)
	assert.NoFileExists(path)
	content, err = os.ReadFile(companion)
	require.NoError(err)
	assert.Equal("synthetic local notes", string(content))
	removed, err = Uninstall(root)
	require.NoError(err)
	assert.Empty(removed)
}
