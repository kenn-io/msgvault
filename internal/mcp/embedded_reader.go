package mcp

import (
	"context"
	"errors"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

// EmbeddedReader executes the supported native tools against one caller-owned
// store. Scope is supplied per call, never retained as session authority.
type EmbeddedReader struct {
	store     *store.Store
	handlers  map[string]sdkmcp.ToolHandlerFor[map[string]any, any]
	downloads *downloadCache
}

func NewEmbeddedReader(st *store.Store, attachmentsDir string) *EmbeddedReader {
	downloads := &downloadCache{}
	h := &handlers{engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), attachmentsDir: attachmentsDir, downloads: downloads, delegatedOnly: true}
	r := &EmbeddedReader{store: st, downloads: downloads, handlers: make(map[string]sdkmcp.ToolHandlerFor[map[string]any, any])}
	for _, d := range stableOperationCatalogs.get(catalogCapabilities{}) {
		switch d.name {
		case ToolGetMessage, ToolListMessages, ToolListThread, ToolSearchMetadata, ToolSearchMessageBodies, ToolGetStats, ToolGetAttachment:
			r.handlers[d.name] = officialToolHandler(d.bind(h))
		}
	}
	return r
}

func (r *EmbeddedReader) Call(ctx context.Context, sourceIDs []int64, req *sdkmcp.CallToolRequest, args map[string]any) (*sdkmcp.CallToolResult, any, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if req == nil || req.Params == nil {
		return nil, nil, errors.New("read tool request is required")
	}
	handler, ok := r.handlers[req.Params.Name]
	if !ok {
		return nil, nil, fmt.Errorf("embedded read tool %q is unavailable", req.Params.Name)
	}
	ctx = sourcescope.WithSourceIDs(ctx, sourceIDs)
	ctx, release, err := r.store.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	return handler(ctx, req, args)
}

func (r *EmbeddedReader) Close() { r.downloads.close() }
