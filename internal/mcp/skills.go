package mcp

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.kenn.io/kit/mcpskills"
	"go.kenn.io/msgvault/docs"
	"go.kenn.io/msgvault/internal/skills"
	"gopkg.in/yaml.v3"
)

// ToolSkills lists and reads the guides bundled with this binary.
const ToolSkills = "msgvault.skills"

// cliSkillNote opens each CLI skill so MCP-only clients do not try to run it.
const cliSkillNote = "> This guide teaches the msgvault CLI. In an MCP session, use the matching " +
	"tools from `tools/list` instead; CLI flags, `--json` and shell pipelines do not apply.\n\n"

// Stateless HTTP builds a server per request, so render the shipped guides once.
var bundledSkills = sync.OnceValue(func() *mcpskills.Catalog {
	catalog, err := newSkillsCatalog()
	if err != nil {
		panic(fmt.Errorf("build bundled MCP guides: %w", err))
	}
	return catalog
})

// newSkillsCatalog serves the MCP quickstart as "workflow", followed by every
// bundled CLI skill under its own name, title and description.
func newSkillsCatalog() (*mcpskills.Catalog, error) {
	rendered, err := skills.Render("")
	if err != nil {
		return nil, err
	}
	guides := []mcpskills.Guide{{
		Name: "workflow", Title: "msgvault MCP quickstart",
		Summary:  "Connect to msgvault over MCP, discover tools, and work safely with the archive.",
		Markdown: docs.MCPQuickstart(docs.AgentQuickstart),
	}}
	for _, skill := range rendered {
		guide, err := skillGuide(skill)
		if err != nil {
			return nil, fmt.Errorf("skill %s: %w", skill.Name, err)
		}
		guides = append(guides, guide)
	}
	catalog, err := mcpskills.New(mcpskills.Config{ToolName: ToolSkills, Guides: guides})
	if err != nil {
		return nil, fmt.Errorf("validate guides: %w", err)
	}
	return catalog, nil
}

func skillGuide(skill skills.Skill) (mcpskills.Guide, error) {
	frontMatter, body, ok := strings.Cut(strings.TrimPrefix(skill.Content, "---\n"), "\n---\n")
	if !ok {
		return mcpskills.Guide{}, errors.New("missing front matter")
	}
	var meta struct {
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(frontMatter), &meta); err != nil {
		return mcpskills.Guide{}, fmt.Errorf("parse front matter: %w", err)
	}
	body = strings.TrimSpace(body)
	heading, rest, _ := strings.Cut(body, "\n")
	title, ok := strings.CutPrefix(heading, "# ")
	if !ok {
		return mcpskills.Guide{}, errors.New("body must start with a # heading")
	}
	// The install marker tells file readers to rerun `skills install`; it
	// does not apply to a guide served by the running binary.
	if i := strings.LastIndex(rest, "\n"); i >= 0 && strings.Contains(rest[i:], skills.Marker) {
		rest = rest[:i]
	}
	return mcpskills.Guide{
		Name: skill.Name, Title: title, Summary: meta.Description,
		Markdown: heading + "\n\n" + cliSkillNote + strings.TrimSpace(rest) + "\n",
	}, nil
}
