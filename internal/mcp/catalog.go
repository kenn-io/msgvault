package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"sort"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/visual"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const (
	schema202012       = "https://json-schema.org/draft/2020-12/schema"
	maxJSONSafeInteger = float64(9007199254740991)
)

type toolSecurityClass uint8

const (
	toolSecurityRead toolSecurityClass = iota
	toolSecurityWrite
	toolSecurityProfileWrite
)

type catalogCapabilities struct {
	semanticSearch  bool
	vectorInMessage bool
	similarMessages bool
	documentSearch  bool
	people          bool
	visualSearch    bool
}

func visualSearchAvailable(capabilities catalogCapabilities) bool {
	return capabilities.visualSearch
}

func searchVisualAttachmentsDefinition() toolDefinition {
	direction := stringSchema("How the owning message relates to the person",
		"from_person", "to_person", "group")
	definition := readDefinition(
		ToolSearchVisualAttachments,
		"Search the visual content of authoritative standalone attachments by text or a bounded base64 query image. Results preserve exact attachment and owning-message provenance.",
		closedObject(map[string]*jsonschema.Schema{
			bodyFormatText:       stringSchema("Natural-language visual query"),
			"image_base64":       stringSchema("Base64 JPEG, PNG, or WebP query image; not persisted"),
			toolArgLimit:         nonNegativeIntegerSchema("Maximum results (1-100, default 20)", 20),
			"sender_person_id":   safeIDSchema("Legacy alias for person_id with from_person direction"),
			toolArgPersonID:      safeIDSchema("Only attachments related to this durable person ID"),
			toolArgParticipantID: safeIDSchema("Only attachments related to this observed participant, translated through its durable person when bound"),
			"directions": {
				Type: "array", Description: "Optional union of from_person, to_person, and group; requires a person reference",
				Items: direction,
			},
			"source_id":      safeIDSchema("Only attachments from this source ID"),
			toolArgMessageID: safeIDSchema("Only attachments owned by this message ID"),
			"filename":       stringSchema("Case-insensitive filename substring filter"),
			"mime_prefix":    stringSchema("Case-insensitive MIME prefix filter, such as image/"),
			toolArgCursor:    stringSchema("Opaque next_cursor from the previous response"),
			toolArgAfter:     stringSchema("Only messages on or after YYYY-MM-DD"),
			toolArgBefore:    stringSchema("Only messages before YYYY-MM-DD"),
		}),
		outputSchemaFor[visual.SearchResponse](),
		(*handlers).searchVisualAttachments,
	)
	definition.availability = visualSearchAvailable
	return definition
}

type catalogToolHandler func(*handlers, context.Context, toolRequest) (*toolResult, error)

type toolDefinition struct {
	name         string
	description  string
	annotations  *sdkmcp.ToolAnnotations
	availability func(catalogCapabilities) bool
	security     toolSecurityClass
	inputSchema  *jsonschema.Schema
	outputSchema *jsonschema.Schema
	handler      catalogToolHandler
}

func (d toolDefinition) tool() *sdkmcp.Tool {
	return &sdkmcp.Tool{
		Name:         d.name,
		Description:  d.description,
		Annotations:  d.annotations,
		InputSchema:  d.inputSchema,
		OutputSchema: d.outputSchema,
	}
}

func (d toolDefinition) bind(h *handlers) func(context.Context, toolRequest) (*toolResult, error) {
	return func(ctx context.Context, req toolRequest) (*toolResult, error) {
		return d.handler(h, ctx, req)
	}
}

func capabilitiesFor(opts ServeOptions) catalogCapabilities {
	return catalogCapabilities{
		semanticSearch:  opts.HybridEngine != nil || opts.HybridSearcher != nil,
		vectorInMessage: opts.HybridEngine != nil && opts.Backend != nil,
		similarMessages: opts.Backend != nil || opts.SimilarSearcher != nil,
		documentSearch:  opts.DocumentSearcher != nil,
		people:          opts.PeopleBackend != nil,
		visualSearch:    opts.VisualSearcher != nil,
	}
}

// stableOperationCatalogs owns the immutable schemas registered with the SDK.
// The SDK v1.7 schema cache keys explicit schemas by pointer identity, so a
// stateless server must reuse these roots instead of rebuilding them per HTTP
// request. There are only sixty-four possible capability keys, which also keeps
// the shared SDK cache boundary fixed.
var stableOperationCatalogs = buildOperationCatalogs()

func buildOperationCatalogs() map[catalogCapabilities][]toolDefinition {
	catalogs := make(map[catalogCapabilities][]toolDefinition, 64)
	for mask := range 64 {
		capabilities := catalogCapabilities{
			semanticSearch:  mask&0b100000 != 0,
			vectorInMessage: mask&0b010000 != 0,
			similarMessages: mask&0b001000 != 0,
			documentSearch:  mask&0b000100 != 0,
			people:          mask&0b000010 != 0,
			visualSearch:    mask&0b000001 != 0,
		}
		catalogs[capabilities] = buildOperationCatalog(capabilities)
	}
	return catalogs
}

func operationCatalog(opts ServeOptions, _ *handlers) []toolDefinition {
	return slices.Clone(stableOperationCatalogs[capabilitiesFor(opts)])
}

func buildOperationCatalog(capabilities catalogCapabilities) []toolDefinition {
	definitions := []toolDefinition{
		aggregateDefinition(nil),
		exportAttachmentDefinition(nil),
		findSimilarMessagesDefinition(nil),
		getAttachmentDefinition(nil),
		getMessageDefinition(nil),
		getPersonNotesDefinition(nil),
		getPersonProfileDefinition(nil),
		getPersonRelationshipDefinition(nil),
		getStatsDefinition(nil),
		listMessagesDefinition(nil),
		searchByDomainsDefinition(nil),
		searchDocumentsDefinition(nil),
		searchInMessageDefinition(nil, capabilities.vectorInMessage),
		searchMessageBodiesDefinition(nil),
		searchMessagesDefinition(nil, capabilities.semanticSearch),
		searchMetadataDefinition(nil),
		searchPeopleDefinition(nil),
		searchPersonFilesDefinition(nil),
		searchVisualAttachmentsDefinition(),
		semanticSearchMessagesDefinition(nil, capabilities.semanticSearch),
		stageDeletionDefinition(nil),
		promotePersonDefinition(nil),
		updatePersonNotesDefinition(nil),
	}

	available := definitions[:0]
	for _, definition := range definitions {
		if definition.availability(capabilities) {
			available = append(available, definition)
		}
	}
	sort.Slice(available, func(i, j int) bool {
		return available[i].name < available[j].name
	})
	return available
}

func readDefinition(
	name, description string,
	inputSchema, outputSchema *jsonschema.Schema,
	handler catalogToolHandler,
) toolDefinition {
	return toolDefinition{
		name:         name,
		description:  description,
		annotations:  toolAnnotations(true),
		availability: alwaysAvailable,
		security:     toolSecurityRead,
		inputSchema:  inputSchema,
		outputSchema: outputSchema,
		handler:      handler,
	}
}

func writeDefinition(
	name, description string,
	inputSchema, outputSchema *jsonschema.Schema,
	handler catalogToolHandler,
) toolDefinition {
	return toolDefinition{
		name:         name,
		description:  description,
		annotations:  toolAnnotations(false),
		availability: alwaysAvailable,
		security:     toolSecurityWrite,
		inputSchema:  inputSchema,
		outputSchema: outputSchema,
		handler:      handler,
	}
}

func profileWriteDefinition(
	name, description string,
	inputSchema, outputSchema *jsonschema.Schema,
	handler catalogToolHandler,
) toolDefinition {
	definition := writeDefinition(name, description, inputSchema, outputSchema, handler)
	definition.security = toolSecurityProfileWrite
	return definition
}

func alwaysAvailable(catalogCapabilities) bool { return true }

func similarMessagesAvailable(c catalogCapabilities) bool { return c.similarMessages }

func documentSearchAvailable(c catalogCapabilities) bool { return c.documentSearch }

func peopleAvailable(c catalogCapabilities) bool { return c.people }

func toolAnnotations(readOnly bool) *sdkmcp.ToolAnnotations {
	falseValue := false
	return &sdkmcp.ToolAnnotations{
		DestructiveHint: &falseValue,
		OpenWorldHint:   &falseValue,
		ReadOnlyHint:    readOnly,
	}
}

func closedObject(properties map[string]*jsonschema.Schema, required ...string) *jsonschema.Schema {
	return &jsonschema.Schema{
		Schema:               schema202012,
		Type:                 "object",
		Properties:           properties,
		Required:             required,
		AdditionalProperties: rejectAllSchema(),
	}
}

func rejectAllSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
}

func stringSchema(description string, values ...string) *jsonschema.Schema {
	schema := &jsonschema.Schema{Type: "string", Description: description}
	if len(values) > 0 {
		schema.Enum = make([]any, len(values))
		for i, value := range values {
			schema.Enum[i] = value
		}
	}
	return schema
}

func booleanSchema(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Description: description}
}

func safeIDSchema(description string) *jsonschema.Schema {
	return boundedIntegerSchema(description, 1, maxJSONSafeInteger)
}

func nonNegativeIntegerSchema(description string, defaultValue int) *jsonschema.Schema {
	schema := boundedIntegerSchema(description, 0, maxJSONSafeInteger)
	schema.Default = json.RawMessage([]byte(json.Number(defaultValueString(defaultValue))))
	return schema
}

func signedSafeIntegerSchema(description string, defaultValue int) *jsonschema.Schema {
	schema := boundedIntegerSchema(description, -maxJSONSafeInteger, maxJSONSafeInteger)
	schema.Default = json.RawMessage([]byte(json.Number(defaultValueString(defaultValue))))
	return schema
}

func boundedIntegerSchema(description string, minimum, maximum float64) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "integer",
		Description: description,
		Minimum:     &minimum,
		Maximum:     &maximum,
	}
}

func scoreSchema(description string) *jsonschema.Schema {
	minimum, maximum := float64(0), float64(1)
	return &jsonschema.Schema{
		Type:        "number",
		Description: description,
		Minimum:     &minimum,
		Maximum:     &maximum,
		Default:     json.RawMessage("0"),
	}
}

func defaultValueString(value int) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func outputSchemaFor[T any]() *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	schema.Schema = schema202012
	return schema
}

func searchMessagesOutputSchema() *jsonschema.Schema {
	metadata := outputSchemaFor[searchMetadataResponse]()
	metadata.Schema = ""
	semantic := outputSchemaFor[searchMessageBodiesResponse]()
	semantic.Schema = ""
	return &jsonschema.Schema{
		Schema: schema202012,
		Type:   "object",
		OneOf:  []*jsonschema.Schema{metadata, semantic},
	}
}

func accountProperty() *jsonschema.Schema {
	return stringSchema("Filter by account email address (use get_stats to list available accounts)")
}

func afterProperty() *jsonschema.Schema {
	return stringSchema("Only messages after this date (YYYY-MM-DD)")
}

func beforeProperty() *jsonschema.Schema {
	return stringSchema("Only messages before this date (YYYY-MM-DD)")
}

func searchLimitProperty() *jsonschema.Schema {
	return nonNegativeIntegerSchema("Maximum results to return (default 20)", 20)
}

func offsetProperty() *jsonschema.Schema {
	return nonNegativeIntegerSchema("Number of results to skip for pagination (default 0)", 0)
}

const (
	searchMetadataOperatorDoc = "Supported operators: from:, to:, cc:, bcc:, subject:, label: (or l:), has:attachment, " +
		"before:/after: (YYYY-MM-DD), older_than:/newer_than: (e.g. 7d, 2w, 1m, 1y), larger:/smaller: (e.g. 5M). " +
		"Bare domains on from:/to: match any address at that domain. Multiple terms are ANDed. " +
		"Not supported: negation (-), OR, or parentheses grouping."
	searchMetadataFreeTextDoc = "Free text matches subject, snippet, and sender/recipient metadata only (not bodies). " +
		"Use search_message_bodies for body keywords or semantic_search_messages for vector/hybrid search."
	searchMetadataPaginationDoc = "Results are ordered newest-first (by sent date); there is no sort parameter — " +
		"use before:/after: to scope a date range. " +
		"Paginate with offset/limit (default limit 20, max 50). " +
		"Response: data, total, returned, offset, has_more."
)

func searchMetadataDefinition(_ *handlers) toolDefinition {
	searchIntro := "Search message metadata using a subset of Gmail query syntax (not full Gmail compatibility). " +
		searchMetadataOperatorDoc + " " + searchMetadataFreeTextDoc + " "
	queryDesc := "Search query (e.g. 'from:alice subject:meeting after:2024-01-01'). " +
		"See tool description for supported operators and limitations."
	return readDefinition(
		ToolSearchMetadata,
		searchIntro+searchMetadataPaginationDoc+
			"For body keywords use search_message_bodies; for vector/hybrid search use semantic_search_messages.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgQuery:   stringSchema(queryDesc),
			toolArgAccount: accountProperty(),
			toolArgLimit:   searchLimitProperty(),
			toolArgOffset:  offsetProperty(),
		}, toolArgQuery),
		outputSchemaFor[searchMetadataResponse](),
		(*handlers).searchMetadata,
	)
}

func searchMessagesDefinition(_ *handlers, vectorAvailable bool) toolDefinition {
	description := "Deprecated compatibility tool; use search_metadata when mode is omitted and semantic_search_messages for mode=vector or mode=hybrid. " +
		searchMetadataOperatorDoc + " " + searchMetadataFreeTextDoc + " " + searchMetadataPaginationDoc
	properties := map[string]*jsonschema.Schema{
		toolArgQuery: stringSchema(
			"Search query; omit mode for metadata search or set mode=vector|hybrid for semantic search",
		),
		toolArgAccount: accountProperty(),
		toolArgLimit:   searchLimitProperty(),
		toolArgOffset:  offsetProperty(),
	}
	if vectorAvailable {
		properties[toolArgMode] = stringSchema("Search mode: vector or hybrid. Omit for metadata search.", searchModeVector, searchModeHybrid)
		properties["explain"] = booleanSchema("Include per-signal scores for vector/hybrid results")
		properties[toolArgMinScore] = scoreSchema("Minimum semantic score for returned chunk excerpts; does not filter ranked messages")
	}
	return readDefinition(
		ToolSearchMessages,
		description,
		closedObject(properties, toolArgQuery),
		searchMessagesOutputSchema(),
		(*handlers).searchMessages,
	)
}

func searchMessageBodiesDefinition(_ *handlers) toolDefinition {
	searchIntro := "Keyword full-text search over message bodies. " +
		"Returns messages whose body text contains the query terms, newest-first, " +
		"each with matches — up to 5 excerpt snippets centered on matched terms. " +
		"Backend excerpts may omit char_offset and line when efficient source locations are unavailable; use search_in_message when exact locations are needed. " +
		"When matches_truncated is true on a hit, more than 5 excerpts matched — use search_in_message or get_message to read the full body. " +
		"Known Gmail operators (from:, subject:, label:, etc.) apply as metadata filters only and do not satisfy the free-text requirement. " +
		"Filter-only queries such as from:alice are rejected — use search_metadata for filter-only queries. " +
		"Unrecognized word:value tokens (e.g. RXD2:V2) are treated as literal body text, not filters. " +
		"Query syntax: space-separated words are ANDed (each must appear somewhere in the body); " +
		"a double-quoted phrase is one exact phrase (e.g. \"RXD2 V2\"); OR and NOT are not supported. " +
		searchMetadataOperatorDoc + " "
	queryDesc := "Body search query with at least one free-text term (bare word or quoted phrase). " +
		"Gmail operators (from:, subject:, etc.) are metadata filters, not body search — " +
		"subject:test alone is rejected; combine with body terms (from:alice budget) or use search_metadata for filter-only queries. " +
		"Unrecognized word:value tokens (RXD2:V2) are literal text. " +
		"Space-separated words are ANDed; double quotes match an exact phrase; OR/NOT unsupported."
	return readDefinition(
		ToolSearchMessageBodies,
		searchIntro+
			"Results are ordered newest-first (by sent date). "+
			"Paginate with offset/limit (default limit 20, max 50). Response: data, returned, offset, has_more. "+
			"Body search does not return a total; use has_more to detect more pages.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgQuery:   stringSchema(queryDesc),
			toolArgAccount: accountProperty(),
			toolArgLimit:   searchLimitProperty(),
			toolArgOffset:  offsetProperty(),
		}, toolArgQuery),
		outputSchemaFor[searchMessageBodiesResponse](),
		(*handlers).searchMessageBodies,
	)
}

func semanticSearchMessagesDefinition(_ *handlers, vectorAvailable bool) toolDefinition {
	if !vectorAvailable {
		return readDefinition(
			ToolSemanticSearchMessages,
			"Semantic (embedding) search over message bodies is unavailable: vector search is not configured on this server.",
			closedObject(map[string]*jsonschema.Schema{
				toolArgQuery: stringSchema("Free-text query to embed (requires at least one free-text term)"),
			}, toolArgQuery),
			outputSchemaFor[searchMessageBodiesResponse](),
			(*handlers).semanticSearchMessages,
		)
	}
	searchIntro := "Semantic (embedding) search over each preprocessed message subject and body. " +
		"Returns messages ranked by similarity to the query — there is no exact total, so page on has_more. " +
		"Each hit includes matches — embedded subject/body chunks ranked by semantic similarity (up to 5 per message), each with a score. " +
		"Vector char_offset and line locations may be omitted because preprocessing usually prevents exact raw-body mapping; use snippet terms with search_in_message keyword mode when navigation is needed. " +
		"min_score filters chunk excerpts only; it does not remove or reorder ranked messages. " +
		"Requires at least one free-text term (used to embed); filter-only queries must use search_metadata. " +
		"Known Gmail operators (from:, subject:, label:, etc.) apply as metadata filters only. " +
		searchMetadataOperatorDoc + " "
	queryDesc := "Free-text query to embed (requires at least one free-text term). " +
		"Gmail operators are metadata filters, not body search; combine with body terms or use search_metadata for filter-only queries."
	mode := stringSchema("Search mode: vector (semantic only) or hybrid (BM25 + vector fused via RRF). Defaults to hybrid when omitted.", searchModeVector, searchModeHybrid)
	mode.Default = json.RawMessage(`"hybrid"`)
	return readDefinition(
		ToolSemanticSearchMessages,
		searchIntro+
			"mode=vector for pure semantic search or mode=hybrid to fuse BM25 and vector ranking via RRF. "+
			"Paginate with offset/limit (default limit 20, max 50). Response: data, returned, offset, has_more, mode, pool_saturated, generation. "+
			"total is not available; use has_more to page.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgQuery:    stringSchema(queryDesc),
			toolArgAccount:  accountProperty(),
			toolArgLimit:    searchLimitProperty(),
			toolArgOffset:   offsetProperty(),
			toolArgMode:     mode,
			"explain":       booleanSchema("Include per-signal scores in the response (for debugging or ranking inspection)"),
			toolArgMinScore: scoreSchema("Minimum chunk similarity score for included match excerpts (default 0); does not filter ranked messages"),
		}, toolArgQuery),
		outputSchemaFor[searchMessageBodiesResponse](),
		(*handlers).semanticSearchMessages,
	)
}

func getMessageDefinition(_ *handlers) toolDefinition {
	bodyFormat := stringSchema("Which body representation to page: auto (default, plain text when available, HTML fallback), text, or html.", bodyFormatAuto, bodyFormatText, bodyFormatHTML)
	bodyFormat.Default = json.RawMessage(`"auto"`)
	return readDefinition(
		ToolGetMessage,
		"Get message details including recipients, labels, attachments, and a slice of the message body. "+
			"Returns plain text when available; HTML-only messages return a body_html slice with body_format=html. "+
			"Body paging mirrors search pagination: body_length=total bytes, offset=where this chunk starts, body_returned=bytes in this chunk, has_more=more body follows. "+
			"To read sequentially: call again with offset += body_returned. "+
			"To jump to a known match location: use center_at=<byte offset> to center the window on that location. "+
			"Note: snippet is pre-stored source metadata (may be empty for non-Gmail sources).",
		closedObject(map[string]*jsonschema.Schema{
			"id":            safeIDSchema("Message ID"),
			toolArgOffset:   nonNegativeIntegerSchema("Byte offset from the start of the selected body to begin reading (default 0). Ignored when center_at is provided.", 0),
			"center_at":     signedSafeIntegerSchema("Byte offset from the start of the selected body to center the window on. Takes precedence over offset.", -1),
			toolArgMaxChars: signedSafeIntegerSchema("Maximum selected-body bytes to return (default 2000, max 4000). Values above 4000 are clamped to 4000; zero or negative values use the default.", 2000),
			"body_format":   bodyFormat,
			"full_body":     booleanSchema("Return the complete selected body in one response, ignoring offset, center_at, and max_chars. Use only when the full content is explicitly needed."),
		}, "id"),
		outputSchemaFor[getMessageResponse](),
		(*handlers).getMessage,
	)
}

func getAttachmentDefinition(_ *handlers) toolDefinition {
	return readDefinition(
		ToolGetAttachment,
		"Get attachment content by attachment ID. Returns metadata as text and the file content as an embedded resource blob. Use get_message first to find attachment IDs.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgAttachmentID: safeIDSchema("Attachment ID (from get_message response)"),
		}, toolArgAttachmentID),
		outputSchemaFor[getAttachmentResponse](),
		(*handlers).getAttachment,
	)
}

func exportAttachmentDefinition(_ *handlers) toolDefinition {
	return writeDefinition(
		ToolExportAttachment,
		"Save an attachment to the local filesystem. Use this for file types that cannot be displayed inline (e.g. PDFs, documents). Returns the saved file path.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgAttachmentID: safeIDSchema("Attachment ID (from get_message response)"),
			toolArgDestination:  stringSchema("Directory to save the file to (default: ~/Downloads)"),
		}, toolArgAttachmentID),
		outputSchemaFor[exportAttachmentResponse](),
		(*handlers).exportAttachment,
	)
}

func searchInMessageDefinition(_ *handlers, vectorAvailable bool) toolDefinition {
	description := "Find matches within one message body. Default mode=keyword finds literal term occurrences. " +
		"Each match includes char_offset (byte offset into body_text), snippet, and line. " +
		"Use char_offset with get_message center_at to read a larger window around any match."
	properties := map[string]*jsonschema.Schema{
		"id":          safeIDSchema("Message ID"),
		toolArgQuery:  stringSchema("Search query (keyword term, or semantic query when mode=vector)"),
		toolArgLimit:  nonNegativeIntegerSchema("Maximum matches to return (default 10)", 10),
		toolArgOffset: offsetProperty(),
	}
	if vectorAvailable {
		description = "Find matches within one message body. Default mode=keyword finds literal term occurrences. " +
			"mode=vector scores each embedded chunk by semantic similarity to the query (best first, with score on each match). " +
			"Keyword matches include raw-body char_offset and line. Vector matches always include snippet and score; char_offset and line may be omitted after preprocessing. " +
			"Use a present char_offset with get_message center_at to read a larger window around the match."
		mode := stringSchema("Search mode: keyword (default, literal term) or vector (semantic chunk scoring)", searchModeKeyword, searchModeVector)
		mode.Default = json.RawMessage(`"keyword"`)
		properties[toolArgMode] = mode
		properties[toolArgMinScore] = scoreSchema("Minimum chunk similarity score (0–1) when mode=vector (default 0)")
	}
	return readDefinition(
		ToolSearchInMessage,
		description,
		closedObject(properties, "id", toolArgQuery),
		outputSchemaFor[searchInMessageResponse](),
		(*handlers).searchInMessage,
	)
}

func listMessagesDefinition(_ *handlers) toolDefinition {
	return readDefinition(
		ToolListMessages,
		"List messages with optional filters, newest-first. "+
			"Pass conversation_id to enumerate a thread's messages, then call get_message(id) per message to read bodies — "+
			"there is deliberately no bulk body fetch, to avoid loading huge threads into the context window. "+
			"Paginate with offset/limit (default limit 20, max 50). Response: data, total, returned, offset, has_more. "+
			"total=-1 because the full count is not computed; use has_more for paging.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgAccount:    accountProperty(),
			toolArgFrom:       stringSchema("Filter by sender email address"),
			"to":              stringSchema("Filter by recipient email address"),
			"label":           stringSchema("Filter by Gmail label"),
			toolArgAfter:      afterProperty(),
			toolArgBefore:     beforeProperty(),
			"has_attachment":  booleanSchema("Only messages with attachments"),
			"conversation_id": safeIDSchema("Filter by conversation/thread ID"),
			toolArgLimit:      searchLimitProperty(),
			toolArgOffset:     offsetProperty(),
		}),
		outputSchemaFor[listMessagesResponse](),
		(*handlers).listMessages,
	)
}

func getStatsDefinition(_ *handlers) toolDefinition {
	return readDefinition(
		ToolGetStats,
		"Get archive overview: total messages, size, attachment count, and accounts.",
		closedObject(map[string]*jsonschema.Schema{}),
		outputSchemaFor[getStatsResponse](),
		(*handlers).getStats,
	)
}

func aggregateDefinition(_ *handlers) toolDefinition {
	return readDefinition(
		ToolAggregate,
		"Get grouped statistics (top senders, recipients, domains, labels, mailing lists, or message volume by calendar year). "+
			"Returns an object with a data array containing objects with fields Key, Count, TotalSize, AttachmentSize, AttachmentCount, and TotalUnique.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgGroupBy: stringSchema("Dimension to group by. When 'time', buckets are by calendar year only (Key is a year string like \"2024\").", toolArgSender, "recipient", "domain", "label", toolArgList, "time"),
			toolArgAccount: accountProperty(),
			toolArgLimit:   nonNegativeIntegerSchema("Maximum results to return (default 50)", 50),
			toolArgAfter:   afterProperty(),
			toolArgBefore:  beforeProperty(),
		}, toolArgGroupBy),
		outputSchemaFor[aggregateResponse](),
		(*handlers).aggregate,
	)
}

func searchByDomainsDefinition(_ *handlers) toolDefinition {
	return readDefinition(
		ToolSearchByDomains,
		"Find messages where any participant (from, to, or cc) belongs to one of the given domains. "+
			"Useful for finding all communication with a company regardless of direction. Returns an object with a data array of matching message summaries.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgDomains: stringSchema("Comma-separated domain names (e.g. 'gobright.com,ascentae.com')"),
			toolArgLimit:   nonNegativeIntegerSchema("Maximum results to return (default 100)", 100),
			toolArgOffset:  offsetProperty(),
			toolArgAfter:   afterProperty(),
			toolArgBefore:  beforeProperty(),
		}, toolArgDomains),
		outputSchemaFor[searchByDomainsResponse](),
		(*handlers).searchByDomains,
	)
}

func stageDeletionDefinition(_ *handlers) toolDefinition {
	return writeDefinition(
		ToolStageDeletion,
		"Stage messages for deletion. Use EITHER 'query' (Gmail-style search) OR structured filters (from, domain, label, etc.), not both. Does NOT delete immediately. To execute, set '[deletion] remote_enabled = true' in the invoking CLI's config.toml for durable consent, then run 'msgvault delete-staged'. One-command alternative: MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgAccount:   accountProperty(),
			toolArgQuery:     stringSchema("Gmail-style search query (e.g. 'from:linkedin subject:job alert'). Cannot be combined with structured filters."),
			toolArgFrom:      stringSchema("Filter by sender email address"),
			"domain":         stringSchema("Filter by sender domain (e.g. 'linkedin.com')"),
			"label":          stringSchema("Filter by Gmail label (e.g. 'CATEGORY_PROMOTIONS')"),
			toolArgAfter:     afterProperty(),
			toolArgBefore:    beforeProperty(),
			"has_attachment": booleanSchema("Only messages with attachments"),
		}),
		outputSchemaFor[stageDeletionResponse](),
		(*handlers).stageDeletion,
	)
}

func findSimilarMessagesDefinition(_ *handlers) toolDefinition {
	definition := readDefinition(
		ToolFindSimilarMessages,
		"Find messages whose embeddings are closest to the given message. Requires vector search to be configured and an active index generation.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgMessageID: safeIDSchema("Seed message ID; its embedding is used as the query vector"),
			toolArgLimit:     nonNegativeIntegerSchema("Maximum results to return (default 20)", 20),
			toolArgAccount:   accountProperty(),
			"message_type":   stringSchema("Restrict results to one message type, such as email, sms, mms, fbmessenger, or calendar_event"),
			toolArgAfter:     afterProperty(),
			toolArgBefore:    beforeProperty(),
			"has_attachment": booleanSchema("Only messages with attachments"),
		}, toolArgMessageID),
		outputSchemaFor[similarMessagesResponse](),
		(*handlers).findSimilarMessages,
	)
	definition.availability = similarMessagesAvailable
	return definition
}

func searchDocumentsDefinition(_ *handlers) toolDefinition {
	limit := boundedIntegerSchema("Maximum results to return (default 20, max 100)", 1, 100)
	limit.Default = json.RawMessage("20")
	mode := stringSchema("Search mode: lexical (default and auto); semantic/hybrid send the query to the embedding provider",
		"auto", "lexical", "semantic", "hybrid")
	mode.Default = json.RawMessage(`"lexical"`)
	direction := stringSchema("How the owning message relates to the person",
		"from_person", "to_person", "group")
	definition := readDefinition(
		ToolSearchDocuments,
		"Search locally indexed content and filenames from standalone document attachments extracted by the configured document provider. Results preserve the exact attachment occurrence, containing message, unit range, excerpt, and provider/model provenance. Paginate with the opaque cursor; restart when the index revision changes.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgQuery: stringSchema("Document content or filename query; terms are ANDed"),
			"source_ids": {
				Type: "array", Description: "Optional source ID scope",
				Items: safeIDSchema("Source ID"),
			},
			"message_types": {
				Type: "array", Description: "Optional containing message type scope",
				Items: stringSchema("Containing message type"),
			},
			toolArgAttachmentID:  safeIDSchema("Optional exact attachment occurrence ID"),
			toolArgMessageID:     safeIDSchema("Optional exact containing message ID"),
			toolArgPersonID:      safeIDSchema("Optional durable person ID"),
			toolArgParticipantID: safeIDSchema("Optional observed participant ID; translated through its durable person when bound"),
			"directions": {
				Type: "array", Description: "Optional union of from_person, to_person, and group; requires a person reference",
				Items: direction,
			},
			toolArgAfter:      stringSchema("Only messages on or after YYYY-MM-DD"),
			toolArgBefore:     stringSchema("Only messages before YYYY-MM-DD"),
			toolArgLimit:      limit,
			toolArgCursor:     stringSchema("Opaque cursor from the previous page"),
			toolArgMode:       mode,
			"candidate_limit": boundedIntegerSchema("Maximum candidates (default/max: lexical 10000; semantic/hybrid 100/1000)", 1, store.MaxLexicalDocumentSearchCandidateLimit),
		}, toolArgQuery),
		outputSchemaFor[store.DocumentSearchResponse](),
		(*handlers).searchDocuments,
	)
	definition.availability = documentSearchAvailable
	return definition
}

func searchPersonFilesDefinition(_ *handlers) toolDefinition {
	limit := boundedIntegerSchema("Maximum metadata results to return (default 100, max 100)", 1, 100)
	limit.Default = json.RawMessage("100")
	direction := stringSchema("How the owning message relates to the person",
		"from_person", "to_person", "group")
	mimeFamily := stringSchema("Stable MIME family",
		"image", "pdf", "audio", "video", bodyFormatText, "document", "archive", "other")
	return readDefinition(
		ToolSearchPersonFiles,
		"Search authoritative attachment metadata for one durable person. Results preserve exact attachment occurrence, owning message, conversation, source, matched participant, role, and direction provenance.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgPersonID: safeIDSchema("Durable person ID"),
			"directions": {
				Type: "array", Description: "Optional union of from_person, to_person, and group",
				Items: direction,
			},
			toolArgAfter:  stringSchema("Only messages on or after YYYY-MM-DD"),
			toolArgBefore: stringSchema("Only messages before YYYY-MM-DD"),
			"filename":    stringSchema("Case-insensitive filename substring filter"),
			"mime_families": {
				Type: "array", Description: "Optional stable MIME-family filter",
				Items: mimeFamily,
			},
			toolArgLimit:  limit,
			toolArgCursor: stringSchema("Opaque cursor from the previous metadata page"),
		}, toolArgPersonID),
		outputSchemaFor[generated.PersonFileSearchHTTPResponse](),
		(*handlers).searchPersonFiles,
	)
}

// The following named response types make every catalog output schema visible
// without changing existing JSON field names.
type searchMetadataResponse paginatedResponse[query.MessageSummary]
type searchInMessageResponse paginatedResponse[messageMatch]
type listMessagesResponse paginatedResponse[query.MessageSummary]
type aggregateResponse struct {
	Data []query.AggregateRow `json:"data"`
}
type searchByDomainsResponse struct {
	Data []query.MessageSummary `json:"data"`
}

type getAttachmentResponse struct {
	Filename string `json:"filename"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

type exportAttachmentResponse struct {
	Path     string `json:"path"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

type stageDeletionResponse struct {
	BatchID      string `json:"batch_id"`
	MessageCount int    `json:"message_count"`
	Status       string `json:"status"`
	NextStep     string `json:"next_step"`
}
