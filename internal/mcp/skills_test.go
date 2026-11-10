package mcp

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/docs"
	"go.kenn.io/msgvault/internal/skills"
	"hegel.dev/go/hegel"
)

func TestSkillsCatalogWithoutArchiveAccess(t *testing.T) {
	for _, delegated := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner", true: "delegated"}[delegated], func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			session := task5ConnectClient(t, ServeOptions{DelegatedOnly: delegated}, false)
			assertions.Contains(session.InitializeResult().Instructions, "msgvault.skills")
			assertions.Contains(session.InitializeResult().Instructions, archiveSafetyInstructions)
			tools, err := session.ListTools(t.Context(), nil)
			requirements.NoError(err)
			var names []string
			for _, tool := range tools.Tools {
				names = append(names, tool.Name)
			}
			requirements.Contains(names, "msgvault.skills")
			if delegated {
				assertions.Equal([]string{"msgvault.skills"}, names)
				_, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessage, Arguments: map[string]any{"id": 1}})
				requirements.Error(err)
			}
			params := &sdkmcp.CallToolParams{Name: "msgvault.skills"}
			if !delegated {
				params.Arguments = map[string]any{}
			}
			result, err := session.CallTool(t.Context(), params)
			requirements.NoError(err)
			requirements.False(result.IsError)
			catalog := task5StructuredAs[struct {
				Skills []struct{ Name, Title, Summary string } `json:"skills"`
			}](t, result)
			requirements.NotEmpty(catalog.Skills)
			for _, guide := range catalog.Skills {
				assertions.NotEmpty(guide.Name)
				assertions.NotEmpty(guide.Title)
				assertions.NotEmpty(guide.Summary)
			}
		})
	}
}

func TestGuideCatalogIncludesEveryRenderedSkill(t *testing.T) {
	session := task5ConnectClient(t, ServeOptions{DelegatedOnly: true}, false)
	rendered, err := skills.Render("dev")
	require.NoError(t, err)
	want := []string{"workflow"}
	for _, skill := range rendered {
		want = append(want, skill.Name)
	}
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSkills, Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	catalog := task5StructuredAs[struct {
		Skills []struct{ Name string } `json:"skills"`
	}](t, result)
	var got []string
	for _, guide := range catalog.Skills {
		got = append(got, guide.Name)
	}
	assert.Equal(t, want, got)

	t.Run("additional_skill", func(t *testing.T) {
		const name = "msgvault-z-probe"
		const content = "# Synthetic skill\n\nComplete synthetic skill content.\n"
		catalog, err := buildSkillsCatalog(append(rendered, skills.Skill{Name: name, Content: content}))
		require.NoError(t, err)
		var names []string
		for _, entry := range catalog.Entries() {
			names = append(names, entry.Name)
		}
		assert.Equal(t, append(want, name), names)
		read, err := catalog.Handler(t.Context(), &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{
			Arguments: json.RawMessage(`{"name":"msgvault-z-probe"}`),
		}})
		require.NoError(t, err)
		require.False(t, read.IsError)
		require.Len(t, read.Content, 1)
		text, ok := read.Content[0].(*sdkmcp.TextContent)
		require.True(t, ok)
		assert.True(t, strings.HasSuffix(text.Text, content), "retain the entire bundled CLI skill")
		assert.Contains(t, strings.TrimSuffix(text.Text, content), skillMCPBasics)
	})
}

func TestSkillsRetrievesEveryShippedGuide(t *testing.T) {
	requirements := require.New(t)
	session := task5ConnectClient(t, ServeOptions{DelegatedOnly: true}, false)
	sources, err := skills.Render("dev")
	requirements.NoError(err)
	want := map[string]string{"workflow": docs.MCPQuickstart(docs.AgentQuickstart)}
	for _, source := range sources {
		want[source.Name] = source.Content
	}
	legacy := map[string]string{
		"msgvault-search":      searchMCPGuide,
		"msgvault-attachments": attachmentsMCPGuide,
		"msgvault-analytics":   analyticsMCPGuide,
	}
	for name := range legacy {
		requirements.Contains(want, name, "retain every legacy skill")
	}
	for name, source := range want {
		t.Run(name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "msgvault.skills", Arguments: map[string]any{"name": name}})
			requirements.NoError(err)
			requirements.False(result.IsError)
			requirements.Len(result.Content, 1)
			text, ok := result.Content[0].(*sdkmcp.TextContent)
			requirements.True(ok)
			if name == "workflow" {
				assertions.Equal(source, text.Text)
			} else {
				assertions.True(strings.HasSuffix(text.Text, source), "retain the entire bundled CLI skill")
				preamble := strings.TrimSuffix(text.Text, source)
				assertions.Contains(preamble, skillMCPBasics)
				if legacyGuide, ok := legacy[name]; ok {
					assertions.Equal(legacyGuide+"\n\n## Bundled CLI guide\n\n", preamble)
				}
			}
			metadata := task5StructuredAs[struct {
				Skills []struct{ Name, Title, Summary string } `json:"skills"`
			}](t, result)
			requirements.Len(metadata.Skills, 1)
			assertions.Equal(name, metadata.Skills[0].Name)
			assertions.NotEmpty(metadata.Skills[0].Title)
			assertions.NotEmpty(metadata.Skills[0].Summary)
		})
	}
}

func TestSkillsRejectsRawInvalidArguments(t *testing.T) {
	peer := newTask5RawStdioPeer(t, ServeOptions{DelegatedOnly: true})
	response := peer.call(t, task3ToolCallBody(1, "msgvault.skills", `{"name":17}`))
	require.NotNil(t, response.Error)
	assert.Equal(t, int64(-32602), response.Error.Code)
	assert.Empty(t, response.Result)
}

func TestSkillsUnknownNamesCannotSelectGuidance(t *testing.T) {
	// Disable throttling for input exploration; its real enforcement has a
	// separate integration test below.
	handler, closeHandler := newHTTPHandlerWithPolicy(ServeOptions{DelegatedOnly: true}, HTTPOptions{}, nil, true)
	t.Cleanup(closeHandler)
	rendered, err := skills.Render("dev")
	require.NoError(t, err)
	known := []string{"workflow"}
	for _, skill := range rendered {
		known = append(known, skill.Name)
	}
	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text())
		if slices.Contains(known, name) {
			return // The finite accepted domain is exhaustively tested above.
		}
		args, err := json.Marshal(map[string]any{"name": name})
		require.NoError(ht, err)
		recorder, response := task3Serve(handler, task3ModernRequest("tools/call", "msgvault.skills", task3ToolCallBody(1, "msgvault.skills", string(args))))
		require.Equal(ht, http.StatusBadRequest, recorder.Code, "response: %s", recorder.Body.String())
		require.NotNil(ht, response.Error)
		assert.Equal(ht, int64(-32602), response.Error.Code)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestSkillsEmptyNameHTTPInvalidParams(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	handler, closeHandler := NewHTTPHandler(ServeOptions{DelegatedOnly: true}, HTTPOptions{})
	t.Cleanup(closeHandler)
	recorder, response := task3Serve(handler, task3ModernRequest("tools/call", ToolSkills, task3ToolCallBody(1, ToolSkills, `{"name":""}`)))
	assertions.Equal(http.StatusBadRequest, recorder.Code)
	requirements.NotNil(response.Error)
	assertions.Equal(int64(-32602), response.Error.Code)
}

func TestSkillsHTTPAuthenticationAndInvocationLimit(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	policy := newInvocationPolicy(0, 1, 1)
	handler, closeHandler := newHTTPHandlerWithPolicy(ServeOptions{DelegatedOnly: true}, HTTPOptions{APIKey: "synthetic-guide-key"}, policy, false)
	t.Cleanup(closeHandler)
	request := func(key string) (int, task3RPCResponse) {
		req := task3ModernRequest("tools/call", "msgvault.skills", task3ToolCallBody(1, "msgvault.skills", `{}`))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		recorder, response := task3Serve(handler, req)
		return recorder.Code, response
	}
	for _, key := range []string{"", "wrong-synthetic-key"} {
		code, _ := request(key)
		assertions.Equal(http.StatusUnauthorized, code)
	}
	code, response := request("synthetic-guide-key")
	assertions.Equal(http.StatusOK, code)
	requirements.Nil(response.Error)
	assertions.NotEmpty(response.Result["structuredContent"])
	code, response = request("synthetic-guide-key")
	assertions.Equal(http.StatusOK, code)
	assertions.Equal(true, response.Result["isError"])
	assertions.Contains(task3ToolErrorText(t, response), "server is busy")
}
