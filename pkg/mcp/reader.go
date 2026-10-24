package mcp

import (
	"context"
	"errors"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	internalmcp "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/archive"
)

// ReaderOptions selects external attachment storage. An empty AttachmentsDir
// leaves attachment bytes unavailable; no local configuration is discovered.
type ReaderOptions struct{ AttachmentsDir string }

// Reader runs native read tools on a caller-owned archive without a daemon or
// listener. The caller owns authentication, admission and transport.
type Reader struct{ reader *internalmcp.EmbeddedReader }

// NewReader supports get_message, list_messages, list_thread, search_metadata,
// search_message_bodies, get_stats and get_attachment. Other catalog definitions
// are not executable through Reader. No archive or attachment storage is opened
// implicitly. Stop and drain calls before closing Reader or its archive.
func NewReader(a *archive.Archive, opts ReaderOptions) (*Reader, error) {
	if a == nil {
		return nil, errors.New("reader requires an archive")
	}
	return &Reader{reader: internalmcp.NewEmbeddedReader(a.Store(), opts.AttachmentsDir)}, nil
}

// Call runs one tool with the supplied source IDs as an upper bound. Nil and
// empty scopes permit no sources. Tool arguments can narrow this scope but
// cannot enlarge it. Counts, references, account discovery and attachment
// continuations use the current call's scope inside the native read snapshot.
//
// Register ReadTool's definition with the SDK's generic AddTool and forward its
// validated arguments here. That SDK path validates both input and output.
// The host must resolve current authority before every call and hold its
// admission fence until Call returns, including each download continuation.
// This API enforces whole-source scopes, not selected messages within a source.
func (r *Reader) Call(ctx context.Context, sourceIDs []int64, req *sdkmcp.CallToolRequest, args map[string]any) (*sdkmcp.CallToolResult, any, error) {
	return r.reader.Call(ctx, sourceIDs, req, args)
}

// Close releases cached download bytes. It does not close the caller's archive.
func (r *Reader) Close() { r.reader.Close() }
