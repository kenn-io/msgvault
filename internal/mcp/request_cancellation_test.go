package mcp

import (
	"context"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPMethodCancellationPreservesSDKContext(t *testing.T) {
	type sdkValueKey struct{}
	for _, cancelSource := range []string{"request", "sdk"} {
		t.Run(cancelSource, func(t *testing.T) {
			requestContext, cancelRequest := context.WithCancel(t.Context())
			defer cancelRequest()
			sdkContext, cancelSDK := context.WithCancel(context.WithValue(t.Context(), sdkValueKey{}, "sdk-value"))
			defer cancelSDK()
			if cancelSource == "request" {
				cancelRequest()
			} else {
				cancelSDK()
			}
			method := requestCancellationMiddleware(requestContext)(func(ctx context.Context, _ string, _ sdkmcp.Request) (sdkmcp.Result, error) {
				assert.Equal(t, "sdk-value", ctx.Value(sdkValueKey{}))
				return nil, ctx.Err()
			})
			_, err := method(sdkContext, "tools/call", nil)
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}
