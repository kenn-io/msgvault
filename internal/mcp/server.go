package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// Tool name constants.
const (
	ToolSearchMessages      = "search_messages"
	ToolSearchMessageBodies = "search_message_bodies"
	ToolGetMessage          = "get_message"
	ToolGetAttachment       = "get_attachment"
	ToolExportAttachment    = "export_attachment"
	ToolListMessages        = "list_messages"
	ToolGetStats            = "get_stats"
	ToolAggregate           = "aggregate"
	ToolStageDeletion       = "stage_deletion"
	ToolSearchByDomains     = "search_by_domains"
	ToolFindSimilarMessages = "find_similar_messages"
	ToolSearchInMessage     = "search_in_message"
)

// search_messages mode values (wire format).
const (
	searchModeFTS    = "fts"
	searchModeVector = "vector"
	searchModeHybrid = "hybrid"
)

// Common argument helpers for recurring tool option definitions.

func withLimit(defaultDesc string) mcp.ToolOption {
	return mcp.WithNumber("limit",
		mcp.Description("Maximum results to return (default "+defaultDesc+")"),
	)
}

func withOffset() mcp.ToolOption {
	return mcp.WithNumber("offset",
		mcp.Description("Number of results to skip for pagination (default 0)"),
	)
}

func withAfter() mcp.ToolOption {
	return mcp.WithString("after",
		mcp.Description("Only messages after this date (YYYY-MM-DD)"),
	)
}

func withBefore() mcp.ToolOption {
	return mcp.WithString("before",
		mcp.Description("Only messages before this date (YYYY-MM-DD)"),
	)
}

func withAccount() mcp.ToolOption {
	return mcp.WithString("account",
		mcp.Description("Filter by account email address (use get_stats to list available accounts)"),
	)
}

// ServeOptions configures an MCP server. Only Engine is required; the
// HybridEngine and VectorCfg fields enable the vector/hybrid modes on
// the search_messages tool, and Backend additionally enables the
// find_similar_messages tool.
type ServeOptions struct {
	Engine           query.Engine
	AttachmentsDir   string
	AttachmentReader AttachmentReader
	ManifestSaver    DeletionManifestSaver
	HybridSearcher   HybridSearcher
	SimilarSearcher  SimilarSearcher
	DataDir          string

	// HybridEngine is optional. When nil, search_messages rejects
	// mode=vector and mode=hybrid with a vector_not_enabled error.
	HybridEngine *hybrid.Engine
	// VectorCfg should already have ApplyDefaults() called on it.
	// The handler reads Search.MaxPageSizeHybridClamp() at request
	// time; a positive value clamps the per-request limit, and zero
	// disables clamping (the user can set
	// `max_page_size_hybrid = 0` in TOML to disable; ApplyDefaults
	// only fills in 50 when the field was omitted).
	VectorCfg vector.Config
	// Backend is optional. When nil, find_similar_messages rejects all
	// calls with a vector_not_enabled error.
	Backend vector.Backend
}

// newMCPServer builds an MCP server with all tools registered from opts.
// Shared by ServeWithOptions (stdio) and ServeHTTPWithOptions (HTTP).
func newMCPServer(opts ServeOptions) *server.MCPServer {
	s := server.NewMCPServer(
		"msgvault",
		"1.0.0",
		server.WithToolCapabilities(false),
	)

	h := &handlers{
		engine:           opts.Engine,
		attachmentsDir:   opts.AttachmentsDir,
		attachmentReader: opts.AttachmentReader,
		manifestSaver:    opts.ManifestSaver,
		hybridSearcher:   opts.HybridSearcher,
		similarSearcher:  opts.SimilarSearcher,
		dataDir:          opts.DataDir,
		hybridEngine:     opts.HybridEngine,
		vectorCfg:        opts.VectorCfg,
		backend:          opts.Backend,
	}

	vectorAvailable := opts.HybridEngine != nil || opts.HybridSearcher != nil
	s.AddTool(searchMessagesTool(vectorAvailable), h.searchMessages)
	s.AddTool(searchMessageBodiesTool(), h.searchMessageBodies)
	s.AddTool(getMessageTool(), h.getMessage)
	s.AddTool(getAttachmentTool(), h.getAttachment)
	s.AddTool(searchInMessageTool(), h.searchInMessage)
	s.AddTool(exportAttachmentTool(), h.exportAttachment)
	s.AddTool(listMessagesTool(), h.listMessages)
	s.AddTool(getStatsTool(), h.getStats)
	s.AddTool(aggregateTool(), h.aggregate)
	s.AddTool(stageDeletionTool(), h.stageDeletion)
	s.AddTool(searchByDomainsTool(), h.searchByDomains)
	if opts.Backend != nil || opts.SimilarSearcher != nil {
		s.AddTool(findSimilarMessagesTool(), h.findSimilarMessages)
	}

	return s
}

// Serve creates an MCP server with email archive tools and serves over stdio.
// It blocks until stdin is closed or the context is cancelled.
// dataDir is the base data directory (e.g., ~/.msgvault) used for deletions.
//
// Serve is a thin wrapper around ServeWithOptions that leaves the vector
// fields empty; callers that want vector/hybrid search should use
// ServeWithOptions directly.
func Serve(ctx context.Context, engine query.Engine, attachmentsDir, dataDir string) error {
	return ServeWithOptions(ctx, ServeOptions{
		Engine:         engine,
		AttachmentsDir: attachmentsDir,
		DataDir:        dataDir,
	})
}

// ServeWithOptions creates an MCP server from opts and serves over stdio.
// It blocks until stdin is closed or the context is cancelled.
func ServeWithOptions(ctx context.Context, opts ServeOptions) error {
	s := newMCPServer(opts)
	stdio := server.NewStdioServer(s)
	if err := stdio.Listen(ctx, os.Stdin, os.Stdout); err != nil {
		return fmt.Errorf("serve MCP over stdio: %w", err)
	}
	return nil
}

// ServeHTTPWithOptions creates an MCP server from opts and serves over
// StreamableHTTP on the given address. Useful for daemonized deployments
// where remote MCP clients (Claude Desktop, IDE plugins, custom
// integrations) connect over a network rather than a local stdin/stdout
// pipe.
//
// When ctx is canceled (e.g. on SIGINT in the daemon), the HTTP server
// is shut down gracefully via httpServer.Shutdown so in-flight requests
// can complete. Mirrors how ServeWithOptions threads the context through
// the stdio Listen call.
func ServeHTTPWithOptions(ctx context.Context, opts ServeOptions, addr string) error {
	s := newMCPServer(opts)
	httpServer := server.NewStreamableHTTPServer(s)
	fmt.Fprintf(os.Stderr, "Starting MCP server on %s\n", addr)

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Graceful shutdown with a short bound; in-flight tool calls
		// usually finish in milliseconds, so 10s is plenty.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		return ctx.Err()
	}
}

func searchMessagesTool(vectorAvailable bool) mcp.Tool {
	if !vectorAvailable {
		return mcp.NewTool(ToolSearchMessages,
			mcp.WithDescription("Search email metadata (subject, sender, recipients, labels, dates) using Gmail-like query syntax. "+
				"Supports from:, to:, subject:, label:, has:attachment, before:, after:, and free text (matched against subject/snippet only, not body). "+
				"Gmail-only operators such as list: are rejected because msgvault does not index List-ID locally. "+
				"For full message body keyword search, use search_message_bodies instead. "+
				"Paginate with offset/limit (default limit 20, max 50). Response: data, total, returned, offset, has_more."),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Gmail-style search query (e.g. 'from:alice subject:meeting after:2024-01-01')"),
			),
			withAccount(),
			withLimit("20"),
			withOffset(),
		)
	}
	return mcp.NewTool(ToolSearchMessages,
		mcp.WithDescription("Search email metadata (subject, sender, recipients, labels, dates) using Gmail-like query syntax. "+
			"Supports from:, to:, subject:, label:, has:attachment, before:, after:, and free text (matched against subject/snippet only, not body). "+
			"Gmail-only operators such as list: are rejected because msgvault does not index List-ID locally. "+
			"For full message body keyword search, use search_message_bodies instead. "+
			"Paginate with offset/limit (default limit 20, max 50). Response: data, total, returned, offset, has_more. "+
			"Vector search is configured: set mode=vector for pure semantic search or mode=hybrid to fuse BM25 and vector ranking via RRF. "+
			"Vector/hybrid require free-text terms; filter-only queries must omit mode. "+
			"total=-1 means the full match count is unknown — use has_more. "+
			"Vector/hybrid ranking depth is capped by max_page_size_hybrid in config."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Gmail-style search query (e.g. 'from:alice subject:meeting after:2024-01-01'); mode=vector|hybrid require at least one free-text term"),
		),
		withAccount(),
		withLimit("20"),
		withOffset(),
		mcp.WithString("mode",
			mcp.Description("Search mode: vector (semantic only) or hybrid (BM25 + vector fused via RRF). Omit for metadata search."),
			mcp.Enum(searchModeVector, searchModeHybrid),
		),
		mcp.WithBoolean("explain",
			mcp.Description("Include per-signal scores in the response (for debugging or ranking inspection)"),
		),
	)
}

func searchMessageBodiesTool() mcp.Tool {
	return mcp.NewTool(ToolSearchMessageBodies,
		mcp.WithDescription("Search message bodies by keyword using full-text search (FTS). Returns messages whose body text contains the search terms, "+
			"plus context_snippets — short excerpts (up to 5 per message, 300 bytes each) centered on each matched term. "+
			"Requires at least one free-text term; use search_messages for filter-only queries (from:, label:, etc.). "+
			"Paginate with offset/limit (default limit 20, max 50). Response: data, returned, offset, has_more. "+
			"(total is not available for body search; use has_more to detect more pages.)"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Search query with at least one free-text term (e.g. 'quarterly report' or 'from:alice budget')"),
		),
		withAccount(),
		withLimit("20"),
		withOffset(),
	)
}

func getMessageTool() mcp.Tool {
	return mcp.NewTool(ToolGetMessage,
		mcp.WithDescription("Get message details including recipients, labels, attachments, and a slice of the message body. "+
			"Returns plain text when available; HTML-only messages return a body_html slice with body_format=html. "+
			"Body paging mirrors search pagination: body_length=total bytes, offset=where this chunk starts, body_returned=bytes in this chunk, has_more=more body follows. "+
			"To read sequentially: call again with offset += body_returned. "+
			"To jump to a known match location: use center_at=<byte offset> to center the window on that location. "+
			"Note: snippet is pre-stored source metadata (may be empty for non-Gmail sources)."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithNumber("id",
			mcp.Required(),
			mcp.Description("Message ID"),
		),
		mcp.WithNumber("offset",
			mcp.Description("Byte offset from the start of the selected body to begin reading (default 0). Ignored when center_at is provided."),
		),
		mcp.WithNumber("center_at",
			mcp.Description("Byte offset from the start of the selected body to center the window on. Takes precedence over offset."),
		),
		mcp.WithNumber("max_chars",
			mcp.Description("Maximum selected-body bytes to return (default 2000, max 4000). Values above 4000 are clamped to 4000; zero or negative values use the default."),
		),
		mcp.WithString("body_format",
			mcp.Description("Which body representation to page: auto (default, plain text when available, HTML fallback), text, or html."),
			mcp.Enum(bodyFormatAuto, bodyFormatText, bodyFormatHTML),
		),
		mcp.WithBoolean("full_body",
			mcp.Description("Return the complete selected body in one response, ignoring offset, center_at, and max_chars. Use only when the full content is explicitly needed."),
		),
	)
}

func getAttachmentTool() mcp.Tool {
	return mcp.NewTool(ToolGetAttachment,
		mcp.WithDescription("Get attachment content by attachment ID. Returns metadata as text and the file content as an embedded resource blob. Use get_message first to find attachment IDs."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithNumber("attachment_id",
			mcp.Required(),
			mcp.Description("Attachment ID (from get_message response)"),
		),
	)
}

func exportAttachmentTool() mcp.Tool {
	return mcp.NewTool(ToolExportAttachment,
		mcp.WithDescription("Save an attachment to the local filesystem. Use this for file types that cannot be displayed inline (e.g. PDFs, documents). Returns the saved file path."),
		mcp.WithNumber("attachment_id",
			mcp.Required(),
			mcp.Description("Attachment ID (from get_message response)"),
		),
		mcp.WithString("destination",
			mcp.Description("Directory to save the file to (default: ~/Downloads)"),
		),
	)
}

func searchInMessageTool() mcp.Tool {
	return mcp.NewTool(ToolSearchInMessage,
		mcp.WithDescription("Find all occurrences of a term within one message body. Returns each match with a character-centered snippet, line number, and char_offset (byte offset into body_text). "+
			"Use char_offset with get_message center_at to read a larger window around any match."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithNumber("id",
			mcp.Required(),
			mcp.Description("Message ID"),
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Term to find in the message body"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum matches to return (default 10)"),
		),
		withOffset(),
	)
}

func listMessagesTool() mcp.Tool {
	return mcp.NewTool(ToolListMessages,
		mcp.WithDescription("List messages with optional filters. Returns message summaries sorted by date. "+
			"Paginate with offset/limit (default limit 20, max 50). Response: data, total, returned, offset, has_more. "+
			"total=-1 because the full count is not computed; use has_more for paging."),
		mcp.WithReadOnlyHintAnnotation(true),
		withAccount(),
		mcp.WithString("from",
			mcp.Description("Filter by sender email address"),
		),
		mcp.WithString("to",
			mcp.Description("Filter by recipient email address"),
		),
		mcp.WithString("label",
			mcp.Description("Filter by Gmail label"),
		),
		withAfter(),
		withBefore(),
		mcp.WithBoolean("has_attachment",
			mcp.Description("Only messages with attachments"),
		),
		mcp.WithNumber("conversation_id",
			mcp.Description("Filter by conversation/thread ID"),
		),
		withLimit("20"),
		withOffset(),
	)
}

func getStatsTool() mcp.Tool {
	return mcp.NewTool(ToolGetStats,
		mcp.WithDescription("Get archive overview: total messages, size, attachment count, and accounts."),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func aggregateTool() mcp.Tool {
	return mcp.NewTool(ToolAggregate,
		mcp.WithDescription("Get grouped statistics (e.g. top senders, domains, labels, or message volume over time)."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("group_by",
			mcp.Required(),
			mcp.Description("Dimension to group by"),
			mcp.Enum("sender", "recipient", "domain", "label", "time"),
		),
		withAccount(),
		withLimit("50"),
		withAfter(),
		withBefore(),
	)
}

func searchByDomainsTool() mcp.Tool {
	return mcp.NewTool(ToolSearchByDomains,
		mcp.WithDescription("Find emails where any participant (from, to, or cc) belongs to one of the given domains. Useful for finding all communication with a company regardless of direction."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("domains",
			mcp.Required(),
			mcp.Description("Comma-separated domain names (e.g. 'gobright.com,ascentae.com')"),
		),
		withLimit("100"),
		withOffset(),
		withAfter(),
		withBefore(),
	)
}

func stageDeletionTool() mcp.Tool {
	return mcp.NewTool(ToolStageDeletion,
		mcp.WithDescription("Stage messages for deletion. Use EITHER 'query' (Gmail-style search) OR structured filters (from, domain, label, etc.), not both. Does NOT delete immediately - run 'msgvault delete-staged' CLI command to execute staged deletions."),
		withAccount(),
		mcp.WithString("query",
			mcp.Description("Gmail-style search query (e.g. 'from:linkedin subject:job alert'). Cannot be combined with structured filters."),
		),
		mcp.WithString("from",
			mcp.Description("Filter by sender email address"),
		),
		mcp.WithString("domain",
			mcp.Description("Filter by sender domain (e.g. 'linkedin.com')"),
		),
		mcp.WithString("label",
			mcp.Description("Filter by Gmail label (e.g. 'CATEGORY_PROMOTIONS')"),
		),
		withAfter(),
		withBefore(),
		mcp.WithBoolean("has_attachment",
			mcp.Description("Only messages with attachments"),
		),
	)
}

func findSimilarMessagesTool() mcp.Tool {
	return mcp.NewTool(ToolFindSimilarMessages,
		mcp.WithDescription("Find messages whose embeddings are closest to the given message. Requires vector search to be configured and an active index generation."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithNumber("message_id",
			mcp.Required(),
			mcp.Description("Seed message ID; its embedding is used as the query vector"),
		),
		withLimit("20"),
		withAccount(),
		mcp.WithString("message_type",
			mcp.Description("Restrict results to one message type, such as email, sms, mms, fbmessenger, or calendar_event"),
		),
		withAfter(),
		withBefore(),
		mcp.WithBoolean("has_attachment",
			mcp.Description("Only messages with attachments"),
		),
	)
}
