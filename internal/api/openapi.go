package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/explorecatalog"
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
//
// 1.5.0 adds source IDs to message summaries, source filters and capability
// echoes to fast search, and the search_scope/source filters plus capability
// echoes to total statistics. The echoes let remote clients fail closed when
// a released older daemon ignores an additive request filter.
// Additive (minor bump): the major-version compatibility gate stays at 1.
//
// 1.6.0 adds the browser-session login, bootstrap, and logout routes. Existing
// API-key security remains the documented scheme for protected API routes;
// cookie authentication is an additive same-origin browser mechanism.
//
// 1.7.0 adds optimistic, secret-redacting browser settings reads and writes.
//
// 1.8.0 adds daemon-owned shared Saved View CRUD with schema-versioned
// canonical definitions and revision ETags.
//
// 1.9.0 adds finite analytical exploration, grouping, selection preflight,
// visible-row lexical match counts, and bounded attachment-fact operations.
//
// 1.10.0 adds filtered semantic-index coverage and explicit coverage states.
//
// 1.11.0 adds attachment-accurate analytical grouping for the Files workspace.
// 1.13.0 adds contextual People and Domain summaries with search authority.
// 1.14.0 adds path-scoped People and Domain file search routes. Their identity
// scope is server-owned narrowing applied after canonical search resolution.
// 1.15.0 adds deletion-manifest detail, exact reviewed-selection deletion
// staging, and server-owned source-sync and selection-action capabilities.
// 1.16.0 adds exact server-authorized browser action targets and truthful
// nullable source-run status fields.
// 1.18.0 makes task mutations retry-stable, adds configured-project task
// search and explicit outbound metadata disclosure, and expands cache states.
// 1.19.0 adds POST /api/v1/relationships: reciprocity-weighted, time-decayed
// ranking of counterparts over resolved identity clusters, with an
// identity_revision cursor authority alongside the existing cache revision.
// 1.20.0 adds POST /api/v1/relationships/{id}/timeline: one counterpart's
// modality-neutral interaction timeline, with chat messages grouped into
// local-day bursts. {id} accepts any member of the counterpart's identity
// cluster and the response echoes the resolved canonical_id.
// 1.21.0 adds POST /api/v1/identity/links and POST /api/v1/identity/unlinks:
// idempotent participant-link mutations that report the new identity
// revision and whether the synchronous Parquet identity-dataset refresh that
// follows succeeded (cache_state: ready|stale). Also adds the additive
// cache_state field to the existing CLI identity add/remove responses.
// 1.22.0 adds optional start/end (RFC3339, UTC, half-open [start, end)) query
// params to GET /api/v1/conversations/{id}. When present, the window and the
// before/after counts are scoped to the range; an anchor outside the range is
// a 400 (conversation_anchor_outside_range) rather than the default full-
// conversation window. Additive (minor bump): omitting the params preserves
// the existing full-conversation behavior.
// 1.23.0 makes GET /api/v1/people/{id} cluster-aware: PersonIdentifier adds
// participant_id, and PersonSummary adds an additive cluster field
// (canonical_id, member_ids, edges) populated only when the requested
// participant is linked to at least one other participant. Identifiers on a
// linked participant's detail span every cluster member instead of just the
// requested ID. Additive (minor bump): unlinked participants and existing
// callers that ignore the new fields see no behavior change.
// 1.24.0 adds the additive counterpart_participant_id field to EntryRow: the
// smallest participant ID on the entry that is not the archive owner, with
// owners resolved through the same cluster-aware canon Relationships ranking
// uses. It is omitted/null when the owner set is unknown (no
// owner_participants rows) or every participant on the entry is the owner.
// Additive (minor bump): existing callers that ignore the field see no
// behavior change.
// 1.25.0 adds the entry_key field to FileMetadataResponse: the canonical
// explore entry key of the attachment's containing item, built with the same
// chat/message classification the explore listings render, so file deep
// links can select a listed entry exactly. Additive (minor bump): existing
// callers that ignore the field see no behavior change.
// 1.26.0 adds the search_deletion_scope field to explore, groups, and
// preflight responses: semantic and hybrid searches declare that an
// unrestricted deletion context was narrowed to active messages (vector
// indexes cover only live rows). Additive (minor bump): existing callers
// that ignore the field see no behavior change.
// 1.27.0 bounds GET /api/v1/conversations/{id} responses: inline message
// bodies are capped by a cumulative uncompressed-body budget (the anchor's
// body is always inlined). Messages beyond the budget carry the additive
// body_omitted flag with empty body fields and an intact snippet; clients
// fetch those bodies individually via GET /api/v1/messages/{id}. The
// store-backed single-message path now also returns body_html. Additive
// (minor bump): typical threads still inline every body, and existing
// callers that ignore the flag see empty bodies only on threads that would
// previously have produced unbounded responses.
// 1.28.0 adds the additive read_only field to Setting: settings marked
// read_only (currently vector.embeddings.api_key_env) are visible over HTTP
// but can only be changed by editing config.toml on the daemon host, and
// PATCH /api/v1/settings continues to reject updates to them. Clients use
// the flag to render such settings as non-editable and exclude them from
// atomic updates. Additive (minor bump): existing callers that ignore the
// field see no behavior change.
// 1.29.0 adds GET /api/v1/content/remote-image: an SSRF-hardened proxy the
// browser uses to load consented remote mail images. The daemon validates
// the URL (http/https, no credentials, hostname gate), rejects private or
// reserved destinations, resolves DNS itself and validates every answer,
// dials only the validated address (re-validating each bounded redirect
// hop), and enforces an image/* content type and a 10 MiB body cap. The
// browser therefore never contacts sender-controlled hosts directly.
// Additive (minor bump): the major-version compatibility gate stays at 1.
// 1.30.0 changes /api/v1/content/remote-image from GET (url query parameter)
// to POST with a required JSON body {"url": "..."}. POST makes the proxy an
// unsafe method for browsers, so the session CSRF machinery (same-origin
// Origin check plus X-Csrf-Token) applies and a sibling-origin <img> embed
// can no longer trigger authenticated outbound fetches. The response
// (image bytes) is unchanged. The endpoint shipped in 1.29.0 and had no
// released non-browser consumers, so this replaces the GET form outright.
// 1.31.0 adds the optional group_key field to POST /api/v1/explore/groups:
// when set, the response contains only the group whose key equals it exactly
// (any rank), and total_count reports the matched-row count (0 or 1). Clients
// use it to hydrate a selected group without paging the ranked listing.
// Additive (minor bump): omitting the field preserves the ranked listing.
const APISchemaVersion = "1.31.0"

// OpenAPIDocument builds the API schema from the same Huma route registration
// used by the daemon. It binds no socket and needs no database.
func OpenAPIDocument() *huma.OpenAPI {
	doc := baseOpenAPIDocument()
	hardenSourceStatusPublicSchemas(doc)
	relaxResponseAdditionalProperties(doc)
	return doc
}

func openAPIClientDocument() *huma.OpenAPI {
	doc := baseOpenAPIDocument()
	hardenSourceStatusClientSchemas(doc)
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
	doc := api.OpenAPI()
	hardenSettingsSchemas(doc)
	hardenSavedViewSchemas(doc)
	hardenExploreSchemas(doc)
	hardenSearchCoverageSchemas(doc)
	hardenTaskLinkSchemas(doc)
	return doc
}

func hardenTaskLinkSchemas(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	if lookup := doc.Components.Schemas.Map()["TaskLinkLookupResponse"]; lookup != nil {
		if tasks := lookup.Properties["tasks"]; tasks != nil {
			tasks.Nullable = false
		}
	}
}

func hardenSourceStatusPublicSchemas(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	schema := doc.Components.Schemas.Map()["SourceStatus"]
	if schema == nil {
		return
	}
	for _, name := range []string{"active_sync", "latest_sync", "last_successful_sync"} {
		if property := schema.Properties[name]; property != nil {
			ref := property.Ref
			property.Ref = ""
			property.Type = ""
			property.Nullable = false
			property.OneOf = []*huma.Schema{{Ref: ref}, {Type: "null"}}
		}
	}
}

func hardenSourceStatusClientSchemas(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	schema := doc.Components.Schemas.Map()["SourceStatus"]
	if schema == nil {
		return
	}
	nullableRuns := map[string]struct{}{
		"active_sync": {}, "latest_sync": {}, "last_successful_sync": {},
	}
	required := schema.Required[:0]
	for _, name := range schema.Required {
		if _, nullable := nullableRuns[name]; !nullable {
			required = append(required, name)
		}
	}
	schema.Required = required
	for name := range nullableRuns {
		if property := schema.Properties[name]; property != nil {
			property.Nullable = true
		}
	}
}

func hardenSearchCoverageSchemas(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	schema := doc.Components.Schemas.Map()["SearchCoverageResponse"]
	if schema != nil && schema.Properties["actions"] != nil {
		schema.Properties["actions"].Nullable = false
		schema.Properties["actions"].Items.Enum = []any{"retry", "build_index"}
	}
}

func hardenExploreSchemas(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	schemas := doc.Components.Schemas.Map()
	dimension := &huma.Schema{
		Type: huma.TypeString,
		Enum: exploreGroupingEnum(),
	}
	schemas["ExploreGroupDimension"] = dimension
	for _, schemaName := range []string{"ExploreGroupsHTTPRequest", "FileGroupsHTTPRequest", "ExploreHTTPRequest"} {
		if schema := schemas[schemaName]; schema != nil && schema.Properties["grouping"] != nil {
			schema.Properties["grouping"].Items = &huma.Schema{Ref: "#/components/schemas/ExploreGroupDimension"}
		}
	}
	for _, schemaName := range []string{"ExploreGroupsHTTPRequest", "FileGroupsHTTPRequest"} {
		if groups := schemas[schemaName]; groups != nil && groups.Properties["grouping"] != nil {
			one := 1
			groups.Properties["grouping"].MinItems = &one
			groups.Properties["grouping"].MaxItems = &one
		}
	}
	for schemaName, properties := range map[string][]string{
		"ExploreFilter":              {"values"},
		"ExploreHTTPResponse":        {"rows"},
		"ExploreGroupsHTTPRequest":   {"grouping"},
		"ExploreGroupsHTTPResponse":  {"rows"},
		"FileGroupsHTTPRequest":      {"grouping", "predicate"},
		"FileGroupsHTTPResponse":     {"rows"},
		"ExploreMatchCountsRequest":  {"predicate", "row_keys"},
		"ExploreFilesHTTPResponse":   {"files"},
		"ExploreFilesHTTPRequest":    {"predicate"},
		"ExploreMatchCountsResponse": {"counts"},
		"ExplorePreflightRequest":    {"selection"},
		"ExplorePreflightResponse":   {"unavailable_actions", "action_targets"},
		"ExploreSelection":           {"predicate", "cache_revision"},
	} {
		schema := schemas[schemaName]
		if schema == nil {
			continue
		}
		for _, property := range properties {
			if schema.Properties[property] != nil {
				schema.Properties[property].Nullable = false
			}
		}
	}
}

func exploreGroupingEnum() []any {
	dimensions := explorecatalog.GroupingDimensions()
	values := make([]any, len(dimensions))
	for index, dimension := range dimensions {
		values[index] = dimension
	}
	return values
}

func hardenSavedViewSchemas(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	schemas := doc.Components.Schemas.Map()
	if filter := schemas["SavedViewFilter"]; filter != nil {
		filter.Properties["values"].Nullable = false
	}
	if state := schemas["SavedViewStateEnvelope"]; state != nil {
		for _, name := range []string{"filters", "grouping", "sort", "columns"} {
			state.Properties[name].Nullable = false
		}
		state.Properties["presentation"].Enum = []any{"table", "timeline", "files"}
	}
}

func hardenSettingsSchemas(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	schemas := doc.Components.Schemas.Map()
	value := schemas["SettingValue"]
	if value != nil {
		value.Type = ""
		value.Properties = nil
		value.Required = nil
		value.AdditionalProperties = nil
		value.OneOf = []*huma.Schema{
			settingsValueArm("string", &huma.Schema{Type: huma.TypeString}),
			settingsValueArm("integer", &huma.Schema{Type: huma.TypeInteger, Format: "int64"}),
			settingsValueArm("number", &huma.Schema{Type: huma.TypeNumber, Format: "double"}),
			settingsValueArm("boolean", &huma.Schema{Type: huma.TypeBoolean}),
			settingsValueArm("strings", &huma.Schema{
				Type:  huma.TypeArray,
				Items: &huma.Schema{Type: huma.TypeString},
			}),
		}
	}
	if setting := schemas["Setting"]; setting != nil {
		setting.Properties["group"].Enum = []any{"browser", "server", "archive", "search", "sources", "integrations"}
		setting.Properties["kind"].Enum = []any{"string", "integer", "number", "boolean", "string_array", "secret"}
	}
	if request := schemas["SettingsPatchRequest"]; request != nil {
		request.Properties["updates"].Nullable = false
	}
	if response := schemas["SettingsResponse"]; response != nil {
		response.Properties["settings"].Nullable = false
	}
}

func settingsValueArm(name string, property *huma.Schema) *huma.Schema {
	return &huma.Schema{
		Type:                 huma.TypeObject,
		AdditionalProperties: false,
		Properties:           map[string]*huma.Schema{name: property},
		Required:             []string{name},
	}
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
	schemas := doc.Components.Schemas.Map()
	for _, schemaName := range []string{"FileSearchRow", "FileMetadataResponse"} {
		if schema := schemas[schemaName]; schema != nil {
			for _, property := range []string{"filename", "mime_type"} {
				if schema.Properties[property] != nil {
					schema.Properties[property].Nullable = true
				}
			}
		}
	}
	for _, schemaName := range []string{"ExploreGroupsHTTPRequest", "FileGroupsHTTPRequest"} {
		if groups := schemas[schemaName]; groups != nil && groups.Properties["grouping"] != nil {
			grouping := groups.Properties["grouping"]
			if grouping.Extensions == nil {
				grouping.Extensions = map[string]any{}
			}
			grouping.Extensions["x-oapi-codegen-extra-tags"] = map[string]any{
				"validate": "required,min=1,max=1",
			}
		}
	}
	setEnumNames := func(schema *huma.Schema, enumNames []any) {
		if schema == nil {
			return
		}
		if schema.Extensions == nil {
			schema.Extensions = map[string]any{}
		}
		schema.Extensions["x-enum-names"] = enumNames
	}
	setEnumNames(schemas["ExploreGroupDimension"], []any{
		"ExploreGroupDimensionSource", "ExploreGroupDimensionParticipant", "ExploreGroupDimensionDomain",
		"ExploreGroupDimensionMessageType", "ExploreGroupDimensionKind", "ExploreGroupDimensionYear", "ExploreGroupDimensionMonth",
	})
	for schemaName, properties := range map[string]map[string][]any{
		"ExploreCacheUnavailableResponse": {
			"readiness": {"ExploreCacheUnavailableResponseReadinessAbsent", "ExploreCacheUnavailableResponseReadinessInterrupted", "ExploreCacheUnavailableResponseReadinessStaleSchema", "ExploreCacheUnavailableResponseReadinessDrifted"},
		},
		"ExploreFilter": {
			"dimension": {"ExploreFilterDimensionSource", "ExploreFilterDimensionParticipant", "ExploreFilterDimensionDomain", "ExploreFilterDimensionMessageType", "ExploreFilterDimensionAfter", "ExploreFilterDimensionBefore", "ExploreFilterDimensionDeletion"},
		},
		"ExploreGroupSort": {
			"direction": {"ExploreGroupSortDirectionAsc", "ExploreGroupSortDirectionDesc"},
			"field":     {"ExploreGroupSortFieldKey", "ExploreGroupSortFieldCount", "ExploreGroupSortFieldEstimatedBytes", "ExploreGroupSortFieldLatestAt"},
		},
		"ExploreGroupsHTTPRequest": {
			"presentation": {"ExploreGroupsHTTPRequestPresentationTable"},
			"search_mode":  {"ExploreGroupsHTTPRequestSearchModeFullText", "ExploreGroupsHTTPRequestSearchModeSemantic", "ExploreGroupsHTTPRequestSearchModeHybrid"},
		},
		"ExploreHTTPRequest": {
			"presentation": {"ExploreHTTPRequestPresentationTable", "ExploreHTTPRequestPresentationTimeline", "ExploreHTTPRequestPresentationFiles"},
			"search_mode":  {"ExploreHTTPRequestSearchModeFullText", "ExploreHTTPRequestSearchModeSemantic", "ExploreHTTPRequestSearchModeHybrid"},
		},
		"IdentitySearchSort": {
			"direction": {"IdentitySearchSortDirectionAsc", "IdentitySearchSortDirectionDesc"},
			"field":     {"IdentitySearchSortFieldActivityCount", "IdentitySearchSortFieldLatestAt", "IdentitySearchSortFieldDisplayLabel"},
		},
		"ExploreSelection": {
			"mode": {"ExploreSelectionModeExplicit", "ExploreSelectionModeAllMatching"},
		},
		"ExploreSort": {
			"direction": {"ExploreSortDirectionDesc"},
			"field":     {"ExploreSortFieldOccurredAt"},
		},
	} {
		schema := schemas[schemaName]
		if schema == nil {
			continue
		}
		for propertyName, enumNames := range properties {
			setEnumNames(schema.Properties[propertyName], enumNames)
		}
	}
	queryResult := schemas["QueryResult"]
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
