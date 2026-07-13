package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/config"
)

// APISchemaVersion is the version stamped into the OpenAPI document
// (info.version). It tracks the HTTP wire contract, not the binary build
// version, so clients can reason about compatibility independently of releases.
//
// 1.1.0: GET /api/v1/cli/search no longer blocks on the FTS completeness
// probe/backfill; it returns immediately and reports background index work in
// the additive index_state field ("checking"/"building"). Clients older than
// this field ignore it and see results without a completeness caveat during
// that window — the same exposure GET /api/v1/search has always had. Additive
// (minor bump): the major-version compatibility gate stays at 1.
//
// 1.2.0: adds the deletion staging endpoints — POST /api/v1/deletions
// (server-side Gmail-ID resolution, dry-run preview), GET /api/v1/deletions
// (list staged manifests by status), and DELETE /api/v1/deletions/{id}
// (cancel a pending/in-progress manifest). Additive (minor bump): the
// major-version compatibility gate stays at 1.
//
// 1.3.0: GET /api/v1/search/deep accepts the additive scope=body parameter
// and echoes that scope in successful body-only responses. Those responses
// carry ID-keyed body_contexts selected by the active FTS backend while the
// existing messages element schema remains stable. Omitted scope keeps the
// existing composite search contract.
// Additive (minor bump): the major-version compatibility gate stays at 1.
//
// 1.4.0: vector/hybrid search adds offset pagination, has_more, and opt-in
// scored chunk matches. Additive (minor bump): existing summary-only callers
// retain their request and response behavior.
const APISchemaVersion = "1.4.0"

// OpenAPIDocument builds the API schema from the same Huma route registration
// used by the daemon. It binds no socket and needs no database.
func OpenAPIDocument() *huma.OpenAPI {
	doc := baseOpenAPIDocument()
	relaxResponseAdditionalProperties(doc)
	return doc
}

func openAPIClientDocument() *huma.OpenAPI {
	doc := baseOpenAPIDocument()
	clearResponseAdditionalProperties(doc)
	applyClientCodegenExtensions(doc)
	return doc
}

func baseOpenAPIDocument() *huma.OpenAPI {
	mux := http.NewServeMux()
	s := &Server{cfg: config.NewDefaultConfig()}
	api := s.setupHumaAPI(mux)
	apiV1 := s.setupAPIV1Group(api)
	s.registerHumaRoutes(api, apiV1)
	return api.OpenAPI()
}

// OpenAPIYAML renders the OpenAPI 3.1 schema as YAML.
func OpenAPIYAML() ([]byte, error) {
	return OpenAPIYAMLVersion("3.1")
}

// OpenAPIYAMLVersion renders the schema as YAML for a supported OpenAPI
// version. Version 3.0 exists for generators that do not yet consume OpenAPI
// 3.1's JSON Schema dialect.
func OpenAPIYAMLVersion(version string) ([]byte, error) {
	switch version {
	case "3.1":
		out, err := OpenAPIDocument().YAML()
		if err != nil {
			return nil, fmt.Errorf("render OpenAPI 3.1 YAML: %w", err)
		}
		return out, nil
	case "3.0":
		out, err := openAPIClientDocument().DowngradeYAML()
		if err != nil {
			return nil, fmt.Errorf("render OpenAPI 3.0 YAML: %w", err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported openapi version %q", version)
	}
}

// OpenAPIJSONVersion renders the schema as pretty JSON.
func OpenAPIJSONVersion(version string) ([]byte, error) {
	var (
		raw []byte
		err error
	)
	switch version {
	case "3.1":
		raw, err = OpenAPIDocument().MarshalJSON()
	case "3.0":
		raw, err = openAPIClientDocument().Downgrade()
	default:
		return nil, fmt.Errorf("unsupported openapi version %q", version)
	}
	if err != nil {
		return nil, fmt.Errorf("render OpenAPI %s JSON: %w", version, err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return nil, err
	}
	pretty.WriteByte('\n')
	return pretty.Bytes(), nil
}

func relaxResponseAdditionalProperties(doc *huma.OpenAPI) {
	replaceStrictResponseAdditionalProperties(doc, true)
}

func clearResponseAdditionalProperties(doc *huma.OpenAPI) {
	replaceStrictResponseAdditionalProperties(doc, nil)
}

func applyClientCodegenExtensions(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	queryResult := doc.Components.Schemas.Map()["QueryResult"]
	if queryResult == nil || queryResult.Properties == nil {
		return
	}
	rows := queryResult.Properties["rows"]
	if rows == nil || rows.Items == nil || rows.Items.Items == nil {
		return
	}
	cell := rows.Items.Items
	if cell.Extensions == nil {
		cell.Extensions = map[string]any{}
	}
	cell.Extensions["x-go-type"] = "any"
}

func replaceStrictResponseAdditionalProperties(doc *huma.OpenAPI, replacement any) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	reg := doc.Components.Schemas
	requestStrict := requestReachableSchemas(documentOperations(doc), reg)
	seen := map[*huma.Schema]struct{}{}
	for _, op := range documentOperations(doc) {
		for _, resp := range op.Responses {
			for _, media := range resp.Content {
				walkSchemaTree(media.Schema, reg, seen, func(schema *huma.Schema) {
					if _, ok := requestStrict[schema]; ok {
						return
					}
					if additionalProperties, ok := schema.AdditionalProperties.(bool); ok && !additionalProperties {
						schema.AdditionalProperties = replacement
					}
				})
			}
		}
	}
}

func requestReachableSchemas(ops []*huma.Operation, reg huma.Registry) map[*huma.Schema]struct{} {
	strict := map[*huma.Schema]struct{}{}
	seen := map[*huma.Schema]struct{}{}
	for _, op := range ops {
		if op.RequestBody == nil {
			continue
		}
		for _, media := range op.RequestBody.Content {
			walkSchemaTree(media.Schema, reg, seen, func(schema *huma.Schema) {
				strict[schema] = struct{}{}
			})
		}
	}
	return strict
}

func documentOperations(doc *huma.OpenAPI) []*huma.Operation {
	if doc == nil {
		return nil
	}
	ops := []*huma.Operation{}
	for _, path := range doc.Paths {
		if path == nil {
			continue
		}
		for _, op := range []*huma.Operation{
			path.Get, path.Put, path.Post, path.Delete,
			path.Options, path.Head, path.Patch, path.Trace,
		} {
			if op != nil {
				ops = append(ops, op)
			}
		}
	}
	return ops
}

func walkSchemaTree(
	schema *huma.Schema,
	reg huma.Registry,
	seen map[*huma.Schema]struct{},
	visit func(*huma.Schema),
) {
	if schema == nil {
		return
	}
	if schema.Ref != "" {
		walkSchemaTree(reg.SchemaFromRef(schema.Ref), reg, seen, visit)
		return
	}
	if _, ok := seen[schema]; ok {
		return
	}
	seen[schema] = struct{}{}
	visit(schema)
	for _, child := range schemaChildren(schema) {
		walkSchemaTree(child, reg, seen, visit)
	}
}

func schemaChildren(schema *huma.Schema) []*huma.Schema {
	children := make([]*huma.Schema, 0, len(schema.Properties)+len(schema.OneOf)+len(schema.AnyOf)+len(schema.AllOf)+3)
	for _, prop := range schema.Properties {
		children = append(children, prop)
	}
	children = append(children, schema.Items, schema.Not)
	if additionalProperties, ok := schema.AdditionalProperties.(*huma.Schema); ok {
		children = append(children, additionalProperties)
	}
	children = append(children, schema.OneOf...)
	children = append(children, schema.AnyOf...)
	children = append(children, schema.AllOf...)
	return children
}
