package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/daemonclient"
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
	Data      []byte
	MediaType string
	Output    any
	IsError   bool
	// ETag is adapter metadata. It is exposed only by an owning typed projection.
	ETag string
}

// OperationRefusalError exposes a fixed safe code selected by the owning adapter;
// provider diagnostics and response messages must never populate Code.
type OperationRefusalError struct{ Code string }

func (e *OperationRefusalError) Error() string { return e.Code }

type OperationFamily string

const (
	OperationFamilySources OperationFamily = "sources"
	OperationFamilyRecords OperationFamily = "records"
	OperationFamilyLoops   OperationFamily = "loops"
	OperationFamilyCurated OperationFamily = "curated"
)

type operationalDefinition struct {
	definition toolDefinition
	family     OperationFamily
	writes     bool
	chunked    *chunkedOperation
}
type chunkedOperation struct {
	inline func(*downloadSnapshot) (any, error)
	chunk  func(*downloadSnapshot, byteChunk) any
}

// Roots are built once, independent of capability combinations. Selecting any
// subset retains pointer identity for the official SDK's shared schema cache.
var fixedOperationalDefinitions = sync.OnceValue(func() []operationalDefinition {
	return append(append(sourceOperationalDefinitions(), loopOperationalDefinitions()...), personRecordDefinitions()...)
})

func operationalCatalog(opts ServeOptions, allowWrites bool) []operationalDefinition {
	if opts.Operations == nil || opts.DelegatedOnly {
		return nil
	}
	definitions := make([]operationalDefinition, 0)
	for _, definition := range fixedOperationalDefinitions() {
		if !opts.Families.allows(definition.definition.name) || !slices.Contains(opts.OperationCapabilities, definition.definition.name) {
			continue
		}
		if definition.writes && (!allowWrites || !slices.Contains(opts.OperationWriteFamilies, definition.family)) {
			continue
		}
		definitions = append(definitions, definition)
	}
	return definitions
}

func newOperationalDefinition(name, description string, family OperationFamily, input, output *jsonschema.Schema, writes bool) operationalDefinition {
	definition := toolDefinition{name: name, description: description, inputSchema: input, outputSchema: operationalOutputSchema(output), annotations: toolAnnotations(!writes)}
	openWorld := writes && family == OperationFamilySources
	definition.annotations.OpenWorldHint = &openWorld
	return operationalDefinition{definition: definition, family: family, writes: writes}
}

// Operational tools can return a declared refusal instead of their success
// receipt. The SDK validates structured error results against this same root.
func operationalOutputSchema(output *jsonschema.Schema) *jsonschema.Schema {
	failure := closedObject(map[string]*jsonschema.Schema{
		"error":                        stringSchema("Fixed public refusal code"),
		"message":                      stringSchema("Recovery instruction"),
		"operation_may_have_completed": {Type: "boolean"},
	}, "error")
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

func (d operationalDefinition) bind(backend OperationBackend, downloads *downloadCache) func(context.Context, toolRequest) (*toolResult, error) {
	return func(ctx context.Context, request toolRequest) (*toolResult, error) {
		if d.chunked != nil {
			req, present, err := chunkArgs(request.arguments)
			if err != nil {
				return operationalToolResponse(d.definition.name, &OperationResult{Output: map[string]any{"error": "invalid_arguments"}, IsError: true}, nil)
			}
			args := maps.Clone(request.arguments)
			delete(args, "offset")
			delete(args, "length")
			delete(args, "sha256")
			identity, err := json.Marshal(args, json.Deterministic(true))
			if err != nil {
				return nil, newInternalError("encode operation arguments", err)
			}
			key := downloadKey{operation: d.definition.name, identity: string(identity)}
			var failed *OperationResult
			var backendErr error
			snapshot, err := downloads.get(ctx, key, req, func() (*downloadSnapshot, error) {
				result, err := backend.ExecuteOperation(ctx, d.definition.name, args)
				if err != nil || result == nil || result.IsError {
					failed, backendErr = result, err
					if err == nil {
						err = errors.New("operation refused")
					}
					return nil, err
				}
				return &downloadSnapshot{data: result.Data, etag: result.ETag, mediaType: result.MediaType}, nil
			})
			if failed != nil {
				return operationalToolResponse(d.definition.name, failed, backendErr)
			}
			if err != nil {
				if errors.Is(err, errDownloadExpired) {
					return operationalToolResponse(d.definition.name, &OperationResult{Output: map[string]any{"error": "invalid_arguments", "message": errDownloadExpired.Error()}, IsError: true}, nil)
				}
				if errors.Is(err, errDownloadTooLarge) {
					return operationalToolResponse(d.definition.name, &OperationResult{Output: map[string]any{"error": "operation_response_limit_exceeded"}, IsError: true}, nil)
				}
				return nil, newInternalError(d.definition.name, err)
			}
			if !present && d.chunked.inline != nil && len(snapshot.data) <= defaultChunkBytes {
				output, err := d.chunked.inline(snapshot)
				if err != nil {
					return nil, newInternalError("decode operation response", err)
				}
				return operationalToolResponse(d.definition.name, &OperationResult{Output: output}, nil)
			}
			chunk, err := sliceChunk(snapshot.data, snapshot.digest, req)
			if err != nil {
				return operationalToolResponse(d.definition.name, &OperationResult{Output: map[string]any{"error": "invalid_arguments"}, IsError: true}, nil)
			}
			return operationalToolResponse(d.definition.name, &OperationResult{Output: d.chunked.chunk(snapshot, chunk)}, nil)
		}

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
	response, jsonErr := jsonResult(result.Output)
	if jsonErr != nil {
		return nil, jsonErr
	}
	response.isError = result.IsError
	if local, ok := errors.AsType[*daemonclient.LocalDaemonUnavailableError](err); ok {
		response.text = local.Error()
	}
	return response, nil
}
