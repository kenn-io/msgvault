package mcp

import (
	"context"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/spf13/cobra"
	"go.kenn.io/kit/clisearch"
)

const (
	ToolSearchTools = "search_tools"

	maxToolDiscoveryQueryBytes = 512
	maxToolDiscoveryResults    = 20
)

type toolDiscoveryInput struct {
	Name        string   `json:"name"`
	Types       []string `json:"types"`
	Description string   `json:"description"`
	Required    bool     `json:"required"`
}

type toolDiscoveryResult struct {
	Name        string               `json:"name"`
	Description string               `json:"description"`
	ReadOnly    bool                 `json:"read_only"`
	Inputs      []toolDiscoveryInput `json:"inputs"`
}

type toolDiscoveryResponse struct {
	Query     string                `json:"query"`
	Results   []toolDiscoveryResult `json:"results"`
	Truncated bool                  `json:"truncated"`
}

var stableToolDiscoveryDefinition = readDefinition(
	ToolSearchTools,
	"Find available MCP tools for a task using tool names, descriptions, and input metadata. Does not search archive content or run the matched tools. Use tools/list for complete input schemas.",
	closedObject(map[string]*jsonschema.Schema{
		"query": {Type: "string", MinLength: new(1), Description: "Task description or tool/input name; 1-512 UTF-8 bytes, with non-whitespace text"},
		"limit": boundedIntegerSchema("Maximum results (default 5, max 20)", 1, maxToolDiscoveryResults),
	}, "query"),
	outputSchemaFor[toolDiscoveryResponse](),
	(*handlers).searchTools,
)

// Capture only the catalog this server registers. Dynamic family discovery is
// applied again per search, just as HTTP tools/list does before pagination.
type toolDiscovery struct {
	definitions      []toolDefinition
	discoverFamilies func(context.Context) *ArchiveFamilies
}

func authorizedOperationCatalog(opts ServeOptions, allowWrites bool) []toolDefinition {
	return slices.DeleteFunc(operationCatalog(opts, nil), func(d toolDefinition) bool {
		return !toolClassAllowed(d.security, opts, allowWrites)
	})
}

func (h *handlers) searchTools(ctx context.Context, req toolRequest) (*toolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	args := req.GetArguments()
	query, _ := args["query"].(string)
	if !utf8.ValidString(query) || len(query) > maxToolDiscoveryQueryBytes || strings.TrimSpace(query) == "" {
		return toolErrorResult("query must contain 1-512 UTF-8 bytes of non-whitespace text"), nil
	}
	limit := limitArg(args, "limit", 5)
	var families *ArchiveFamilies
	if h.toolDiscovery.discoverFamilies != nil {
		families = h.toolDiscovery.discoverFamilies(ctx)
	}
	definitions := slices.DeleteFunc(slices.Clone(h.toolDiscovery.definitions), func(d toolDefinition) bool {
		return !families.allows(d.name)
	})
	return discoverTools(definitions, query, limit)
}

func discoverTools(definitions []toolDefinition, query string, limit int) (*toolResult, error) {
	root := &cobra.Command{Use: "msgvault"}
	byName := make(map[string]toolDefinition, len(definitions))
	for _, definition := range definitions {
		// Kit's shipped ranker accepts Cobra metadata. These private descriptors
		// are never executed and have no links to the CLI or its lifecycle hooks.
		command := &cobra.Command{Use: definition.name, Short: definition.description, Run: func(*cobra.Command, []string) {}}
		var keywords []string
		for name, input := range definition.inputSchema.Properties {
			keywords = append(keywords, name+" "+input.Description)
		}
		slices.Sort(keywords)
		clisearch.SetKeywords(command, keywords...)
		root.AddCommand(command)
		byName[definition.name] = definition
	}
	ranked := clisearch.Search(root, query, maxToolDiscoveryResults, clisearch.Options{})
	response := toolDiscoveryResponse{
		Query: query, Results: make([]toolDiscoveryResult, 0, min(limit, len(ranked))),
		Truncated: len(ranked) > limit || len(ranked) == maxToolDiscoveryResults,
	}
	for _, match := range ranked[:min(limit, len(ranked))] {
		definition := byName[strings.TrimPrefix(match.Command, root.Name()+" ")]
		response.Results = append(response.Results, discoveryResult(definition))
	}
	return jsonResult(response)
}

func discoveryResult(definition toolDefinition) toolDiscoveryResult {
	result := toolDiscoveryResult{
		Name: definition.name, Description: definition.description,
		ReadOnly: definition.annotations != nil && definition.annotations.ReadOnlyHint,
		Inputs:   make([]toolDiscoveryInput, 0, len(definition.inputSchema.Properties)),
	}
	names := make([]string, 0, len(definition.inputSchema.Properties))
	for name := range definition.inputSchema.Properties {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		input := definition.inputSchema.Properties[name]
		types := slices.Clone(input.Types)
		if types == nil {
			types = []string{}
		}
		if input.Type != "" {
			types = append(types, input.Type)
		}
		slices.Sort(types)
		result.Inputs = append(result.Inputs, toolDiscoveryInput{
			Name: name, Types: types, Required: slices.Contains(definition.inputSchema.Required, name), Description: input.Description,
		})
	}
	return result
}
