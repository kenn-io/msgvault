package cmd

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/daemonclient"
)

// openMCPHTTPStore gives a long-lived local proxy the same runtime discovery
// policy as a fresh CLI invocation. Configured remote and delegated endpoints
// retain their fixed URL and authentication behavior.
func openMCPHTTPStore(ctx context.Context) (*daemonclient.Client, HTTPStoreInfo, error) {
	client, info, err := OpenHTTPStore(ctx)
	if err != nil || info.Kind != HTTPStoreLocalDaemon {
		return client, info, err
	}
	invocation := invocationFromContext(ctx)
	// Discovery refreshes credentials. Keep those mutations private to the
	// serialized resolver rather than changing the shared invocation config.
	cfg := *invocation.cfg
	client.EnableLocalDaemonRecovery(func(ctx context.Context) (daemonclient.LocalDaemonEndpoint, error) {
		// MCP tool contexts carry request cancellation but not command metadata.
		// Reattach the original invocation so a replacement daemon honors flags
		// such as --config while retaining the request's cancellation chain.
		resolveCtx := withInvocation(ctx, invocation)
		runtime, _, err := ensureLocalDaemonRuntimeWithStartupCacheIntent(resolveCtx, &cfg, startupCacheBuildIntentNone)
		if err != nil {
			return daemonclient.LocalDaemonEndpoint{}, err
		}
		if err := cfg.ResolveServerKey(); err != nil {
			return daemonclient.LocalDaemonEndpoint{}, fmt.Errorf("refresh daemon API key: %w", err)
		}
		return daemonclient.LocalDaemonEndpoint{
			URL: urlFromDaemonRuntime(runtime), APIKey: cfg.Server.AuthenticationKey(),
		}, nil
	})
	return client, info, nil
}
