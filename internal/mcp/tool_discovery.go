package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"go.kenn.io/kit/clisearch"
)

const (
	ToolSearchTools = "search_tools"

	maxToolDiscoveryQueryBytes = 512
	maxToolDiscoveryResults    = 20
	maxToolDiscoveryBytes      = 32 << 10
	maxToolDiscoveryInputs     = 16
)

type toolDiscoveryInput struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

type toolDiscoveryResult struct {
	Name            string               `json:"name"`
	Description     string               `json:"description"`
	ReadOnly        bool                 `json:"read_only"`
	Inputs          []toolDiscoveryInput `json:"inputs"`
	InputsTruncated bool                 `json:"inputs_truncated"`
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
		"query": {Type: "string", MinLength: new(1), MaxLength: new(maxToolDiscoveryQueryBytes), Description: "Task description or tool/input name; 1-512 UTF-8 bytes, with non-whitespace text"},
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
	query, ok := args["query"].(string)
	if !ok || !utf8.ValidString(query) || len(query) > maxToolDiscoveryQueryBytes || strings.TrimSpace(query) == "" {
		return toolErrorResult("query must contain 1-512 UTF-8 bytes of non-whitespace text"), nil
	}
	limit := 5
	if _, supplied := args["limit"]; supplied {
		parsed, err := positiveInt64Arg(args, "limit")
		if err != nil || parsed > maxToolDiscoveryResults {
			//nolint:nilerr // Invalid arguments are MCP tool errors, not protocol failures.
			return toolErrorResult("limit must be an integer between 1 and 20"), nil
		}
		limit = int(parsed)
	}
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
	for {
		// The SDK validates and re-encodes object outputs with encoding/json
		// before building its text fallback. Match its escaping, rather than
		// jsonResult's encoding/json/v2 text representation.
		content, err := json.Marshal(response)
		if err != nil {
			return nil, newInternalError("marshal tool discovery result", err)
		}
		// Budget both the structured object and the SDK's text fallback after
		// JSON escaping, including HTML/control characters and UTF-8. The outer
		// JSON-RPC envelope is not part of this tool-result budget.
		encoded, err := json.Marshal(&sdkmcp.CallToolResult{
			StructuredContent: json.RawMessage(content),
			Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: string(content)}},
		})
		if err != nil {
			return nil, newInternalError("measure tool discovery result", err)
		}
		// Modern sessions append this discriminator after the handler returns.
		if len(encoded)+len(`,"resultType":"complete"`) <= maxToolDiscoveryBytes {
			return jsonResult(response)
		}
		response.Results = response.Results[:len(response.Results)-1]
		response.Truncated = true
	}
}

func discoveryResult(definition toolDefinition) toolDiscoveryResult {
	result := toolDiscoveryResult{
		Name: definition.name, Description: discoveryText(definition.description, 320),
		ReadOnly: definition.annotations != nil && definition.annotations.ReadOnlyHint,
		Inputs:   make([]toolDiscoveryInput, 0, min(maxToolDiscoveryInputs, len(definition.inputSchema.Properties))),
	}
	names := make([]string, 0, len(definition.inputSchema.Properties))
	for name := range definition.inputSchema.Properties {
		names = append(names, name)
	}
	// Keep required arguments first when a large schema needs a summary.
	slices.SortFunc(names, func(a, b string) int {
		aRequired := slices.Contains(definition.inputSchema.Required, a)
		bRequired := slices.Contains(definition.inputSchema.Required, b)
		if aRequired != bRequired {
			if aRequired {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	result.InputsTruncated = len(names) > maxToolDiscoveryInputs
	for _, name := range names[:min(len(names), maxToolDiscoveryInputs)] {
		input := definition.inputSchema.Properties[name]
		result.Inputs = append(result.Inputs, toolDiscoveryInput{
			Name: name, Type: input.Type, Required: slices.Contains(definition.inputSchema.Required, name),
			Description: discoveryText(input.Description, 160),
		})
	}
	return result
}

func discoveryText(text string, maxBytes int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes - len("…")
	for !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}
