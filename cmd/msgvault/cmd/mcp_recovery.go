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
	// Discovery refreshes credentials. Keep those mutations private to the
	// serialized resolver rather than changing the shared invocation config.
	cfg := *invocationFromContext(ctx).cfg
	err = client.EnableLocalDaemonRecovery(func(ctx context.Context) (daemonclient.LocalDaemonEndpoint, error) {
		runtime, _, err := ensureLocalDaemonRuntimeWithStartupCacheIntent(ctx, &cfg, startupCacheBuildIntentNone)
		if err != nil {
			return daemonclient.LocalDaemonEndpoint{}, err
		}
		if err := cfg.ResolveServerKey(); err != nil {
			return daemonclient.LocalDaemonEndpoint{}, fmt.Errorf("refresh daemon API key: %w", err)
		}
		return daemonclient.LocalDaemonEndpoint{
			URL: urlFromDaemonRuntime(runtime), APIKey: cfg.Server.AuthenticationKey(),
			LocalDaemonToken: runtime.Record.Metadata[runtimeShutdownToken],
		}, nil
	})
	if err != nil {
		_ = client.Close()
		return nil, HTTPStoreInfo{}, err
	}
	return client, info, nil
}
