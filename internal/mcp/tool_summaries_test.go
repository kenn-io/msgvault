package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func TestToolSummariesCoverRegistries(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	summaries := ToolSummaries()
	byName := make(map[string]ToolSummary, len(summaries))
	for _, summary := range summaries {
		assertions.NotContains(byName, summary.Name, "duplicate definition")
		assertions.NotEmpty(summary.Description)
		assertions.NotEmpty(summary.GrantPermission)
		assertions.NotEqual("unknown", summary.Security)
		byName[summary.Name] = summary
	}
	assertions.Equal(summaries, ToolSummaries(), "map iteration must not change ordering")
	for _, capabilities := range []catalogCapabilities{{}, {sqlQuery: true, people: true, savedViews: true, directoryPeople: true, contactRoutes: true, personCardDAV: true, identityReview: true, kata: true, kataLookup: true}} {
		for _, definition := range buildOperationCatalog(capabilities) {
			assertions.Contains(byName, definition.name)
		}
	}
	for _, definition := range stableCalendarTools() {
		assertions.Equal("backend-dependent", byName[definition.name].GrantPermission)
	}
	for _, definition := range stableDraftDefinitions {
		assertions.Equal("backend-dependent", byName[definition.name].GrantPermission)
	}
	for _, definition := range fixedOperationalDefinitions() {
		assertions.Contains(byName, definition.definition.name)
		if definition.writes {
			assertions.NotEqual("read", byName[definition.definition.name].Security, definition.definition.name)
			assertions.NotEmpty(byName[definition.definition.name].Flag, definition.definition.name)
		}
	}
	for _, summary := range summaries {
		if permission, ok := agentToolPermission(summary.Name); ok {
			assertions.Equal(string(permission), summary.GrantPermission)
		}
	}
	assertions.Equal("owner-only", byName[ToolQuerySQL].GrantPermission)
	assertions.Equal("message.read", byName[ToolGetMessage].GrantPermission)
	assertions.Equal("--allow-profile-writes", byName[ToolPromotePerson].Flag)
	tools := rawListTools(t, ServeOptions{Engine: &querytest.MockEngine{}}, false)
	requirements.NotEmpty(tools)
	for _, tool := range tools {
		name, ok := tool["name"].(string)
		requirements.True(ok)
		assertions.Contains(byName, name)
		assertions.Equal("read", byName[name].Security)
	}
}

func TestToolClassWriteGates(t *testing.T) {
	assertions := assert.New(t)
	opts := ServeOptions{
		AllowProfileWrites: true, AllowIdentityDecisions: true, AllowIdentityScoring: true,
		AllowPersonMerges: true, AllowCardDAVWrites: true, AllowCalendarWrites: true, AllowKataWrites: true,
	}
	for class := toolSecurityRead; class <= toolSecurityKataWrite; class++ {
		assertions.True(toolClassAllowed(class, opts, true))
		assertions.Equal(class == toolSecurityRead, toolClassAllowed(class, opts, false))
		assertions.Equal(class == toolSecurityRead || class == toolSecurityWrite, toolClassAllowed(class, ServeOptions{}, true))
	}
	assertions.False(toolClassAllowed(toolSecurityClass(255), opts, true))
}
