package mcp

import (
	"context"
	"encoding/json/v2"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/googlereviews"
)

type reviewBackend func(context.Context, googlereviews.Request) (*googlereviews.Result, error)

func (f reviewBackend) ControlGoogleReviews(ctx context.Context, r googlereviews.Request) (*googlereviews.Result, error) {
	return f(ctx, r)
}

func TestGoogleReviewsToolsReuseCachedSchemasAcrossPermittedSubsets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	full := googleReviewTools(true)
	require.Len(full, 4)
	repeated := googleReviewTools(true)
	restricted := googleReviewTools(false)
	require.Len(restricted, 1)
	assert.Equal("google_review_capabilities", restricted[0].name)
	assert.Same(full[0].inputSchema, repeated[0].inputSchema)
	assert.Same(full[0].outputSchema, repeated[0].outputSchema)
	assert.Same(full[0].inputSchema, restricted[0].inputSchema)
	assert.Same(full[0].outputSchema, restricted[0].outputSchema)
}

func TestGoogleReviewsMCPCapabilityAndContentBoundary(t *testing.T) {
	for _, content := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit content"}[content], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var requests []googlereviews.Request
			backend := reviewBackend(func(_ context.Context, r googlereviews.Request) (*googlereviews.Result, error) {
				requests = append(requests, r)
				return &googlereviews.Result{Preview: &googlereviews.Preview{Review: googlereviews.Review{ReviewID: "r"}, Comment: "Thanks", ConfirmationToken: "CANARY_TOKEN", ReceiptID: "CANARY_RECEIPT"}}, nil
			})
			clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
			server, err := newMCPServer(ServeOptions{GoogleReviews: backend, AllowGoogleReviewContent: content}, true).Connect(t.Context(), serverTransport, nil)
			require.NoError(err)
			t.Cleanup(func() { assert.NoError(server.Close()) })
			client, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "review-test", Version: "1"}, nil).Connect(t.Context(), clientTransport, nil)
			require.NoError(err)
			t.Cleanup(func() { assert.NoError(client.Close()) })
			list, err := client.ListTools(t.Context(), &sdkmcp.ListToolsParams{})
			require.NoError(err)
			names := make([]string, 0, len(list.Tools))
			for _, tool := range list.Tools {
				names = append(names, tool.Name)
			}
			assert.Contains(names, "google_review_capabilities")
			if content {
				assert.Contains(names, "list_google_reviews")
				assert.Contains(names, "get_google_review")
				assert.Contains(names, "preview_google_review_reply")
				result, err := client.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "preview_google_review_reply", Arguments: map[string]any{"target": "shop", "review_id": "r", "comment": "Thanks"}})
				require.NoError(err)
				assert.False(result.IsError)
				data, err := json.Marshal(result)
				require.NoError(err)
				assert.NotContains(string(data), "CANARY")
				require.Len(requests, 1)
				assert.Equal("assist_reply", requests[0].Action)
				result, err = client.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "preview_google_review_reply", Arguments: map[string]any{"target": "shop", "review_id": "r", "comment": "Thanks", "action": "confirm_reply", "confirmed": true}})
				require.NoError(err)
				assert.True(result.IsError)
				assert.Len(requests, 1)
			} else {
				assert.NotContains(names, "list_google_reviews")
				assert.NotContains(names, "get_google_review")
				assert.NotContains(names, "preview_google_review_reply")
			}
			assert.NotContains(names, "confirm_google_review_reply")
			assert.NotContains(names, "google_review_receipt")
		})
	}
}
