package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

const (
	localDaemonAuthProbeTimeout = 2 * time.Second
	localDaemonAuthProbeHeader  = "X-Msgvault-Local-Daemon-Probe"
	localDaemonAuthProbeValue   = "auth"
)

// runStartupMigrations pulls legacy identity addresses from the global config
// and runs the one-time migration. If migration was performed, the notice is
// logged and printed to stderr. If the migration is deferred because no source
// exists yet, it will be retried on a later command after a source has been
// created — and ingest commands that create the first source should call
// runPostSourceCreateMigrations after GetOrCreateSource so the deferred
// migration applies on the same invocation.
//
// Always returns nil unless the migration itself errors.
func runStartupMigrations(s *store.Store) error {
	addrs := cfg.Identity.Addresses
	res, err := s.RunStartupMigrations(addrs)
	if err != nil {
		logger.Warn("startup migration failed", "error", err)
		return err
	}
	// Success cases log at Info (the operation succeeded; res.Notice is
	// the user-facing surface on stderr). Reserved Warn for the actual
	// error path above.
	switch {
	case res.Deferred:
		logger.Info("legacy [identity] block in config detected (migration deferred until a source exists)",
			"address_count", res.AddressCount,
			"hint", "run 'msgvault add-account ...' to create a source; the migration will retry on the next command")
	case res.Applied:
		logger.Info("legacy identity migrated",
			"addresses", res.AddressCount,
			"sources", res.SourceCount)
	}
	if res.Notice != "" {
		fmt.Fprintln(os.Stderr, res.Notice)
	}
	return nil
}

// runStartupMigrationsForIngest is the pre-source-create hook for ingest
// commands. The only startup migration today is MigrateLegacyIdentityConfig,
// which writes to account_identities — and any pre-source-create write
// races confirmDefaultIdentity by populating identity rows before the
// source's own identifier is confirmed, causing confirmDefaultIdentity's
// `len(existing) > 0` guard to skip the source's own address (regression
// caught upstream at iter20).
//
// All ingest paths already invoke runPostSourceCreateMigrations after
// confirmDefaultIdentity, which handles the legacy migration correctly
// in the deferred (no-source) case and is a no-op once the migration
// sentinel is set. So this pre-source call is intentionally a no-op
// to avoid the race. Kept as a named hook so future startup work that
// genuinely belongs *before* source creation has an obvious place to
// land without re-introducing the legacy-identity race.
func runStartupMigrationsForIngest(s *store.Store) error {
	_ = s
	return nil
}

// runPostSourceCreateMigrations re-runs startup migrations after the caller
// has just created a source. The legacy identity migration defers when no
// source exists at startup, so on a fresh install the very first
// add-account / add-imap / add-o365 / import-* invocation needs a second
// pass to actually apply the migration on the same invocation that created
// the first source. Subsequent calls are O(1) — once the migration sentinel
// is set, MigrateLegacyIdentityConfig short-circuits.
func runPostSourceCreateMigrations(s *store.Store) error {
	return runStartupMigrations(s)
}

// HTTPStoreKind identifies which HTTP endpoint a CLI command is using.
type HTTPStoreKind string

const (
	HTTPStoreConfiguredRemote HTTPStoreKind = "configured_remote"
	HTTPStoreLocalDaemon      HTTPStoreKind = "local_daemon"
)

// HTTPStoreInfo carries the selected daemon endpoint alongside the client.
// Commands use it for user-facing endpoint labels and local-daemon cwd policy.
type HTTPStoreInfo struct {
	Kind HTTPStoreKind
	URL  string
}

// IsRemoteMode returns true when CLI requests should target the configured
// remote daemon instead of this machine's local daemon.
// Resolution order:
//  1. --local flag → local daemon
//  2. [remote].url set in config → configured remote daemon
//  3. Default → local daemon
func IsRemoteMode() bool {
	if useLocal {
		return false
	}
	return cfg != nil && cfg.Remote.URL != ""
}

// OpenHTTPStore returns the HTTP store that ordinary CLI commands should use.
// A configured [remote].url wins unless --local was passed. Otherwise the local
// daemon is discovered or started so SQLite remains owned by one long-lived
// process.
func OpenHTTPStore(ctx context.Context) (*daemonclient.Client, HTTPStoreInfo, error) {
	if cfg == nil {
		return nil, HTTPStoreInfo{}, errors.New("nil config")
	}
	if IsRemoteMode() {
		st, err := openRemoteStoreWithTimeout(api.DaemonLongRequestTimeout)
		if err != nil {
			return nil, HTTPStoreInfo{}, err
		}
		return st, HTTPStoreInfo{
			Kind: HTTPStoreConfiguredRemote,
			URL:  cfg.Remote.URL,
		}, nil
	}

	rt, err := ensureLocalDaemonRuntime(ctx, cfg)
	if err != nil {
		return nil, HTTPStoreInfo{}, err
	}
	url := urlFromDaemonRuntime(rt)
	st, err := daemonclient.New(daemonclient.Config{
		URL:           url,
		APIKey:        cfg.Server.APIKey,
		AllowInsecure: true,
		Timeout:       api.DaemonLongRequestTimeout,
	})
	if err != nil {
		return nil, HTTPStoreInfo{}, err
	}
	return st, HTTPStoreInfo{
		Kind: HTTPStoreLocalDaemon,
		URL:  url,
	}, nil
}

func openRemoteStoreWithTimeout(timeout time.Duration) (*daemonclient.Client, error) {
	return daemonclient.New(daemonclient.Config{
		URL:           cfg.Remote.URL,
		APIKey:        cfg.Remote.APIKey,
		AllowInsecure: cfg.Remote.AllowInsecure,
		Timeout:       timeout,
	})
}

func ensureLocalDaemonRuntime(ctx context.Context, c *config.Config) (*DaemonRuntime, error) {
	if c == nil {
		return nil, errors.New("nil config")
	}
	if err := os.MkdirAll(c.Data.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	if rt := findDaemonRuntime(c.Data.DataDir); rt != nil &&
		!shouldUpgradeDaemonRuntimeWithPolicy(rt, Version, c.Server.DaemonAutoRestart) {
		if err := probeLocalDaemonAuth(ctx, rt, c); err != nil {
			return nil, err
		}
		return rt, nil
	}

	// No usable daemon was found. If a direct CLI writer owns the archive,
	// fail fast with a clear message instead of spawning a daemon that cannot
	// claim the held write-owner lock.
	if err := daemonAutostartPreflight(c); err != nil {
		return nil, err
	}

	launchLock, ok := acquireBackgroundLaunchLock(c.Data.DataDir)
	if !ok {
		if rt := waitForUsableBackgroundRuntime(ctx, c.Data.DataDir, c.Server.DaemonAutoRestart, 30*time.Second); rt != nil {
			if err := probeLocalDaemonAuth(ctx, rt, c); err != nil {
				return nil, err
			}
			return rt, nil
		}
		return nil, errors.New("msgvault daemon start is already in progress")
	}
	defer func() { _ = launchLock.Unlock() }()

	prep, err := prepareBackgroundDaemonStart(c, "run `msgvault serve stop` or retry with --local")
	if err != nil {
		return nil, err
	}
	if rt := prep.Reusable; rt != nil {
		if err := probeLocalDaemonAuth(ctx, rt, c); err != nil {
			return nil, err
		}
		return rt, nil
	}

	proc, err := startServeBackgroundProcessForRun(c)
	if err != nil {
		return nil, fmt.Errorf("start background daemon: %w", err)
	}
	rt, ready, err := waitForBackgroundServeReadyForRun(
		ctx, c.Data.DataDir, proc.Wait, 30*time.Second,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"server exited before becoming ready: %w\nLogs: %s",
			err, proc.LogPath,
		)
	}
	if !ready {
		return nil, fmt.Errorf(
			"msgvault daemon did not become ready within 30s (pid %d)\nLogs: %s",
			proc.PID, proc.LogPath,
		)
	}
	return rt, nil
}

func probeLocalDaemonAuth(ctx context.Context, rt *DaemonRuntime, c *config.Config) error {
	if rt == nil {
		return errors.New("nil daemon runtime")
	}
	if c == nil {
		return errors.New("nil config")
	}
	url := urlFromDaemonRuntime(rt)
	if url == "" {
		return errors.New("daemon runtime has no usable endpoint")
	}
	if err := localDaemonAuthIdentityError(url, rt, c); err != nil {
		return err
	}
	if c.Server.APIKey == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, localDaemonAuthProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url+"/api/v1/stats", nil)
	if err != nil {
		return fmt.Errorf("create local daemon auth probe: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(localDaemonAuthProbeHeader, localDaemonAuthProbeValue)
	if c.Server.APIKey != "" {
		req.Header.Set("X-Api-Key", c.Server.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("probe local daemon authentication at %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf(
			"local msgvault daemon at %s rejected the configured [server] api_key; "+
				"the daemon may have been started before config.toml changed. "+
				"Run `msgvault serve restart` or `msgvault serve stop` and retry",
			url,
		)
	default:
		return fmt.Errorf(
			"local msgvault daemon at %s failed the authenticated readiness probe: %s",
			url,
			resp.Status,
		)
	}
}

func localDaemonAuthIdentityError(url string, rt *DaemonRuntime, c *config.Config) error {
	if rt == nil || c == nil {
		return nil
	}
	want := daemonAPIKeyFingerprint(c.Server.APIKey)
	got := ""
	if rt.Record.Metadata != nil {
		got = rt.Record.Metadata[runtimeAuthFingerprint]
	}
	if got == "" && c.Server.APIKey == "" {
		return nil
	}
	if got != want {
		return localDaemonAPIKeyMismatchError(url)
	}
	return nil
}

func localDaemonAPIKeyMismatchError(url string) error {
	return fmt.Errorf(
		"local msgvault daemon at %s was started with a different [server] api_key configuration. "+
			"Run `msgvault serve restart` or `msgvault serve stop` and retry",
		url,
	)
}

func waitForUsableBackgroundRuntime(ctx context.Context, dataDir string, policy string, timeout time.Duration) *DaemonRuntime {
	rt, ready, _ := waitForDaemonRuntime(
		ctx,
		dataDir,
		timeout,
		func(rt *DaemonRuntime) bool {
			return rt != nil && !shouldUpgradeDaemonRuntimeWithPolicy(rt, Version, policy)
		},
		nil,
	)
	if !ready {
		return nil
	}
	return rt
}
