package mcp

import (
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/mcpskills"
	"go.kenn.io/msgvault/docs"
	"go.kenn.io/msgvault/internal/skills"
)

func TestSkillsToolServesEveryBundledGuide(t *testing.T) {
	rendered, err := skills.Render("")
	require.NoError(t, err)
	want := []string{"workflow"}
	for _, skill := range rendered {
		want = append(want, skill.Name)
	}

	for _, delegated := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner", true: "delegated"}[delegated], func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			session := task5ConnectClient(t, ServeOptions{DelegatedOnly: delegated}, false)
			assertions.Contains(session.InitializeResult().Instructions, ToolSkills+" {}")

			listed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSkills, Arguments: map[string]any{}})
			requirements.NoError(err)
			requirements.False(listed.IsError, task5SDKResultText(listed))
			catalog := task5StructuredAs[mcpskills.Output](t, listed)
			var names []string
			for _, entry := range catalog.Skills {
				names = append(names, entry.Name)
				assertions.NotEmpty(entry.Title, entry.Name)
				assertions.NotEmpty(entry.Summary, entry.Name)
			}
			assertions.Equal(want, names)

			for _, name := range want {
				guide, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSkills, Arguments: map[string]any{"name": name}})
				requirements.NoError(err)
				requirements.False(guide.IsError, task5SDKResultText(guide))
				text := task5SDKResultText(guide)
				if name == "workflow" {
					assertions.Equal(docs.MCPQuickstart(docs.AgentQuickstart), text)
					continue
				}
				assertions.Contains(text, cliSkillNote, name)
				assertions.NotContains(text, "\nname: "+name, "front matter belongs in metadata")
				assertions.NotContains(text, skills.Marker, "install marker does not apply to served guides")
			}

			unknown, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSkills, Arguments: map[string]any{"name": "missing"}})
			requirements.NoError(err)
			assertions.True(unknown.IsError)
		})
	}
}

func TestSkillGuideUsesSkillMetadata(t *testing.T) {
	guide, err := skillGuide(skills.Skill{
		Name: "msgvault-example",
		Content: "---\nname: msgvault-example\ndescription: Find example messages.\n---\n\n" +
			"# Example guide\n\nRun `msgvault search`.\n\n<!-- " + skills.Marker + " v1 -->\n",
	})
	require.NoError(t, err)
	assert.Equal(t, mcpskills.Entry{Name: "msgvault-example", Title: "Example guide", Summary: "Find example messages."}, guide.Entry)
	assert.Equal(t, "# Example guide\n\n"+cliSkillNote+"Run `msgvault search`.\n", guide.Markdown)
}
