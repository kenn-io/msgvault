package mcp

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// OperationBackend executes only the registered operation name. Each owning
// adapter validates typed arguments and results; paths, commands and disclosure
// policy are never selected by tool arguments.
type OperationBackend interface {
	ExecuteOperation(ctx context.Context, name string, args map[string]any) (*OperationResult, error)
	OperationDisclosure(ctx context.Context, name string, args map[string]any) (string, error)
}

// OperationResult contains the owning adapter's validated public output. A
// failed result may retain a structured partial or uncertain operation receipt.
type OperationResult struct {
	Output  any
	IsError bool
	// ETag is adapter metadata. It is exposed only by an owning typed projection.
	ETag string
}

// OperationRefusalError exposes a fixed safe code selected by the owning adapter;
// provider diagnostics and response messages must never populate Code.
type OperationRefusalError struct{ Code string }

func (e *OperationRefusalError) Error() string { return e.Code }

const mcpSchemaArray = "array"

type OperationFamily string

const (
	OperationFamilySources   OperationFamily = "sources"
	OperationFamilyDrafts    OperationFamily = "drafts"
	OperationFamilyProviders OperationFamily = "providers"
	OperationFamilyDocuments OperationFamily = "documents"
	OperationFamilyCardDAV   OperationFamily = "carddav"
	OperationFamilyRecords   OperationFamily = "records"
	OperationFamilyInference OperationFamily = "inference"
	OperationFamilyVisual    OperationFamily = "visual"
	OperationFamilySettings  OperationFamily = "settings"
)

type operationalDefinition struct {
	definition toolDefinition
	family     OperationFamily
	writes     bool
	delegated  bool
}

// Roots are built once, independent of capability combinations. Selecting any
// subset retains pointer identity for the official SDK's shared schema cache.
var fixedOperationalDefinitions = sync.OnceValue(func() []operationalDefinition {
	return append(sourceOperationalDefinitions(), draftOperationalDefinitions()...)
})

func operationalCatalog(opts ServeOptions, allowWrites bool) []operationalDefinition {
	if opts.Operations == nil {
		return nil
	}
	definitions := make([]operationalDefinition, 0)
	for _, definition := range fixedOperationalDefinitions() {
		if !slices.Contains(opts.OperationCapabilities, definition.definition.name) || (opts.DelegatedOnly && !definition.delegated) {
			continue
		}
		if definition.writes && (!allowWrites || !slices.Contains(opts.OperationWriteFamilies, definition.family)) {
			continue
		}
		if capability, ok := opts.Operations.(ConversationDraftCapabilities); ok && capability.SupportsConversationDrafts() && definition.definition.name == "draft_compose" {
			definition = conversationDraftComposeDefinition()
		}
		definitions = append(definitions, definition)
	}
	return definitions
}

func newOperationalDefinition(name, description string, family OperationFamily, input, output *jsonschema.Schema, writes, delegated bool) operationalDefinition {
	definition := toolDefinition{name: name, description: description, inputSchema: input, outputSchema: operationalOutputSchema(output, family), annotations: toolAnnotations(!writes)}
	openWorld := writes && family != OperationFamilyRecords && family != OperationFamilySettings
	definition.annotations.OpenWorldHint = &openWorld
	return operationalDefinition{definition: definition, family: family, writes: writes, delegated: delegated}
}

// Operational tools can return a declared refusal instead of their success
// receipt. The SDK validates structured error results against this same root.
func operationalOutputSchema(output *jsonschema.Schema, family OperationFamily) *jsonschema.Schema {
	properties := map[string]*jsonschema.Schema{
		"error":                        stringSchema("Fixed public refusal code"),
		"operation_may_have_completed": {Type: "boolean"},
	}
	if family == OperationFamilyDrafts {
		properties["draft"] = outputSchemaFor[DraftOutput]()
	}
	failure := closedObject(properties, "error")
	failure.Schema = ""
	success := *output
	success.Schema = ""
	success.Defs = nil
	success.Definitions = nil
	return &jsonschema.Schema{
		Schema:      schema202012,
		Type:        "object",
		Defs:        output.Defs,
		Definitions: output.Definitions,
		AnyOf:       []*jsonschema.Schema{&success, failure},
	}
}

func (d operationalDefinition) bind(backend OperationBackend) func(context.Context, toolRequest) (*toolResult, error) {
	return func(ctx context.Context, request toolRequest) (*toolResult, error) {
		if d.writes {
			disclosure, err := backend.OperationDisclosure(ctx, d.definition.name, request.arguments)
			if err != nil {
				if refusal, ok := errors.AsType[*OperationRefusalError](err); ok {
					result, jsonErr := jsonResult(map[string]any{"error": refusal.Code})
					if jsonErr != nil {
						return nil, jsonErr
					}
					result.isError = true
					return result, nil
				}
				return nil, newInternalError("read operation disclosure", err)
			}
			if disclosure == "" {
				return nil, newInternalError("read operation disclosure", errors.New("empty disclosure"))
			}
			if err := request.confirmUserAction(ctx, disclosure); err != nil {
				return confirmationToolError(err)
			}
		}
		result, err := backend.ExecuteOperation(ctx, d.definition.name, request.arguments)
		return operationalToolResponse(d.definition.name, result, err)
	}
}

func operationalToolResponse(name string, result *OperationResult, err error) (*toolResult, error) {
	if err != nil {
		if result == nil || !result.IsError {
			return nil, newInternalError(name, err)
		}
		slog.Error("MCP operation returned a partial failure", "operation", name, "error", err)
	}
	if result == nil || result.Output == nil {
		return nil, newInternalError(name, errors.New("missing operation result"))
	}
	response, err := jsonResult(result.Output)
	if err != nil {
		return nil, err
	}
	response.isError = result.IsError
	return response, nil
}
