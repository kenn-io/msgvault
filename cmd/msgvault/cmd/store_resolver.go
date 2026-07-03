package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

const (
	localDaemonAuthProbeTimeout        = 2 * time.Second
	localDaemonAuthProbeHeader         = "X-Msgvault-Local-Daemon-Probe"
	localDaemonAuthProbeValue          = "auth"
	localDaemonAutoStartReadyTimeout   = 30 * time.Minute
	localDaemonStartupProgressDelay    = 2 * time.Second
	localDaemonStartupProgressInterval = 10 * time.Second
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
	st.SetBusyNotifier(reportDaemonBusyWait)
	return st, HTTPStoreInfo{
		Kind: HTTPStoreLocalDaemon,
		URL:  url,
	}, nil
}

// reportDaemonBusyWait surfaces gate contention while the client retries:
// without it a command queued behind a long operation looks hung.
func reportDaemonBusyWait(message string) {
	_, _ = fmt.Fprintf(os.Stderr, "Waiting: %s (Ctrl+C to cancel).\n", message)
}

func openRemoteStoreWithTimeout(timeout time.Duration) (*daemonclient.Client, error) {
	st, err := daemonclient.New(daemonclient.Config{
		URL:           cfg.Remote.URL,
		APIKey:        cfg.Remote.APIKey,
		AllowInsecure: cfg.Remote.AllowInsecure,
		Timeout:       timeout,
	})
	if err != nil {
		return nil, err
	}
	st.SetBusyNotifier(reportDaemonBusyWait)
	return st, nil
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
	if ok {
		// Acquiring the launch lock is not proof no daemon start is underway: a
		// prior `serve start` releases the lock after its short readiness wait
		// while its background child may still be initializing (a live runtime
		// record not yet answering the ping). Spawning now would race a
		// duplicate daemon onto the port/ownership lock. Release and fall into
		// the wait path, mirroring the waited-acquisition guard below.
		inProgress, err := daemonStartInProgress(ctx, c.Data.DataDir)
		if err != nil {
			_ = launchLock.Unlock()
			return nil, err
		}
		if inProgress {
			_ = launchLock.Unlock()
			ok = false
		}
	}
	if !ok {
		_, _ = fmt.Fprintf(os.Stderr,
			"Another msgvault daemon start is in progress; waiting up to %s for readiness.\n",
			compactDuration(localDaemonAutoStartReadyTimeout))
		rt, acquiredLock, err := waitForUsableBackgroundRuntimeOrLaunchLock(
			ctx, c.Data.DataDir, c.Server.DaemonAutoRestart, localDaemonAutoStartReadyTimeout,
		)
		if err != nil {
			return nil, err
		}
		if rt != nil {
			if err := probeLocalDaemonAuth(ctx, rt, c); err != nil {
				return nil, err
			}
			return rt, nil
		}
		if acquiredLock != nil {
			launchLock = acquiredLock
		} else {
			return nil, errors.New("msgvault daemon start is already in progress")
		}
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

	proc, err := startServeBackgroundProcessForRun(c, backgroundServeStartOptions{})
	if err != nil {
		return nil, fmt.Errorf("start background daemon: %w", err)
	}
	stopProgress := reportLocalDaemonStartup(ctx, proc)
	defer stopProgress()
	rt, ready, err := waitForBackgroundServeReadyForRun(
		ctx, c.Data.DataDir, proc.Wait, localDaemonAutoStartReadyTimeout,
	)
	if err != nil {
		return nil, backgroundServeStartupError(err, proc)
	}
	if !ready {
		return nil, fmt.Errorf(
			"msgvault daemon did not become ready within %s (pid %d)\nLogs: %s",
			localDaemonAutoStartReadyTimeout, proc.PID, proc.LogPath,
		)
	}
	return rt, nil
}

func backgroundServeStartupError(err error, proc *backgroundServeProcess) error {
	if proc == nil {
		return fmt.Errorf("server exited before becoming ready: %w", err)
	}
	lastLog := humanizeDaemonLogLine(latestDaemonLogLine(proc.LogPath))
	if lastLog != "" {
		return fmt.Errorf(
			"server exited before becoming ready: %w\nLast log: %s\nLogs: %s",
			err, lastLog, proc.LogPath,
		)
	}
	return fmt.Errorf(
		"server exited before becoming ready: %w\nLogs: %s",
		err, proc.LogPath,
	)
}

func reportLocalDaemonStartup(ctx context.Context, proc *backgroundServeProcess) func() {
	if proc == nil {
		return func() {}
	}
	if proc.LogPath != "" {
		_, _ = fmt.Fprintf(os.Stderr, "Starting local msgvault daemon (pid %d). Logs: %s\n", proc.PID, proc.LogPath)
	} else {
		_, _ = fmt.Fprintf(os.Stderr, "Starting local msgvault daemon (pid %d).\n", proc.PID)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		timer := time.NewTimer(localDaemonStartupProgressDelay)
		defer timer.Stop()
		started := time.Now()
		lastLine := ""
		announced := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-timer.C:
			}

			// Only startups that are actually slow get the readiness
			// preamble; fast starts stay to a single announce line.
			if !announced {
				announced = true
				_, _ = fmt.Fprintf(os.Stderr,
					"Waiting for the daemon to become ready (large archives may run migrations; timeout %s).\n",
					compactDuration(localDaemonAutoStartReadyTimeout))
			}

			elapsed := time.Since(started).Round(time.Second)
			line := latestDaemonLogLine(proc.LogPath)
			switch {
			case line != "" && line != lastLine:
				_, _ = fmt.Fprintf(os.Stderr, "Daemon startup (%s): %s\n", elapsed, humanizeDaemonLogLine(line))
				lastLine = line
			case proc.LogPath != "":
				_, _ = fmt.Fprintf(os.Stderr,
					"Daemon startup (%s): still waiting. Logs: %s\n",
					elapsed, proc.LogPath)
			default:
				_, _ = fmt.Fprintf(os.Stderr,
					"Daemon startup (%s): still waiting.\n",
					elapsed)
			}
			timer.Reset(localDaemonStartupProgressInterval)
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// compactDuration renders a duration without zero-valued trailing units,
// so a 30-minute timeout reads "30m" instead of "30m0s".
func compactDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// daemonStartupStepLabels maps internal startup step names to what a user
// should read while waiting. The label must stay truthful for the fast
// no-op case: init_archive_schema runs idempotent CREATEs and only
// sometimes migrates, so it reads "checking", not "migrating".
var daemonStartupStepLabels = map[string]string{
	"open_archive_database": "opening the archive database",
	"init_archive_schema":   "checking the database schema",
	"init_analytics_engine": "starting the analytics engine",
	"init_vector_backend":   "initializing vector search",
	"skip_vector_backend":   "vector search disabled",
	"start_api_server":      "starting the API server",
}

func daemonStartupStepLabel(step string) string {
	if label, ok := daemonStartupStepLabels[step]; ok {
		return label
	}
	return strings.ReplaceAll(step, "_", " ")
}

// humanizeDaemonLogLineKnownKeys are the logfmt keys the humanizer knows how
// to summarize or safely drop. A line carrying any other key (e.g. a panic
// record's panic/stack attrs) is returned raw instead, so summarizing never
// hides critical diagnostics.
var humanizeDaemonLogLineKnownKeys = map[string]bool{
	"time":   true,
	"level":  true,
	"msg":    true,
	"step":   true,
	"detail": true,
	"error":  true,
	"run_id": true,
	"source": true,
}

// humanizeDaemonLogLine turns a logfmt serve.log line into something
// readable for an interactive user. It keeps the msg value, appends
// the step (underscores replaced by spaces), and appends any error,
// dropping time/level/run_id/detail. When the line carries any key the
// humanizer does not recognize (e.g. panic/stack), it returns the raw line so
// no information is hidden. On any parse trouble it also returns the raw line.
func humanizeDaemonLogLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return line
	}
	fields, ok := parseLogfmt(line)
	if !ok {
		return line
	}
	msg, hasMsg := fields["msg"]
	if !hasMsg || msg == "" {
		return line
	}
	var sb strings.Builder
	step := fields["step"]
	switch {
	// The startup-step records repeat their own context in msg
	// ("daemon startup step: init archive schema" under a "Daemon
	// startup (2s):" prefix); collapse them to their user-facing
	// label. These records carry arbitrary progress attrs (database,
	// bind, enabled), which are detail — never a reason to fall back
	// to the raw line, so they skip the unknown-key guard below.
	case msg == "daemon startup step" && step != "":
		sb.WriteString(daemonStartupStepLabel(step))
	case msg == "daemon startup step complete" && step != "":
		sb.WriteString(daemonStartupStepLabel(step))
		sb.WriteString(" (done)")
	default:
		for key := range fields {
			if !humanizeDaemonLogLineKnownKeys[key] {
				return line
			}
		}
		sb.WriteString(msg)
		if step != "" {
			sb.WriteString(": ")
			sb.WriteString(daemonStartupStepLabel(step))
		}
	}
	if errVal := fields["error"]; errVal != "" {
		sb.WriteString(" : ")
		sb.WriteString(errVal)
	}
	return sb.String()
}

// parseLogfmt is a tolerant parser for slog's text output: space
// separated key=value pairs where values may be bare or double
// quoted with backslash escapes. It returns ok=false when the input
// isn't logfmt-shaped (e.g. a token with no '='), so callers can
// fall back to the raw line.
func parseLogfmt(line string) (map[string]string, bool) {
	fields := map[string]string{}
	i, n := 0, len(line)
	for i < n {
		for i < n && line[i] == ' ' {
			i++
		}
		if i >= n {
			break
		}
		keyStart := i
		for i < n && line[i] != '=' && line[i] != ' ' {
			i++
		}
		if i >= n || line[i] != '=' {
			return nil, false
		}
		key := line[keyStart:i]
		i++ // consume '='
		var val string
		if i < n && line[i] == '"' {
			i++
			var sb strings.Builder
			for i < n {
				c := line[i]
				if c == '\\' && i+1 < n {
					sb.WriteByte(line[i+1])
					i += 2
					continue
				}
				if c == '"' {
					i++
					break
				}
				sb.WriteByte(c)
				i++
			}
			val = sb.String()
		} else {
			valStart := i
			for i < n && line[i] != ' ' {
				i++
			}
			val = line[valStart:i]
		}
		if key != "" {
			fields[key] = val
		}
	}
	return fields, true
}

func latestDaemonLogLine(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	const maxTailBytes int64 = 32 * 1024
	start := max(st.Size()-maxTailBytes, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, rawLine := range slices.Backward(lines) {
		if line := strings.TrimSpace(rawLine); line != "" {
			return line
		}
	}
	return ""
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
	if c.Server.APIKey == "" && daemonRuntimeAuthFingerprint(rt) == daemonAPIKeyFingerprint("") {
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
	got := daemonRuntimeAuthFingerprint(rt)
	if got == "" && c.Server.APIKey == "" {
		return nil
	}
	if got != want {
		return localDaemonAPIKeyMismatchError(url)
	}
	return nil
}

func daemonRuntimeAuthFingerprint(rt *DaemonRuntime) string {
	if rt == nil || rt.Record.Metadata == nil {
		return ""
	}
	return rt.Record.Metadata[runtimeAuthFingerprint]
}

func localDaemonAPIKeyMismatchError(url string) error {
	return fmt.Errorf(
		"local msgvault daemon at %s was started with a different [server] api_key configuration. "+
			"Run `msgvault serve restart` or `msgvault serve stop` and retry",
		url,
	)
}

func waitForUsableBackgroundRuntimeOrLaunchLock(
	ctx context.Context,
	dataDir string,
	policy string,
	timeout time.Duration,
) (*DaemonRuntime, *flock.Flock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = localDaemonAutoStartReadyTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(daemonProbeTick)
	defer ticker.Stop()

	for {
		rt, err := findCompatibleDaemonRuntimeContext(ctx, dataDir)
		if err != nil {
			return nil, nil, err
		}
		if rt != nil && !shouldUpgradeDaemonRuntimeWithPolicy(rt, Version, policy) {
			return rt, nil, nil
		}
		if lock, ok := acquireBackgroundLaunchLock(dataDir); ok {
			inProgress, err := daemonStartInProgress(ctx, dataDir)
			if err != nil {
				_ = lock.Unlock()
				return nil, nil, err
			}
			if !inProgress {
				return nil, lock, nil
			}
			// A previous `serve start` released the launch lock after its
			// short readiness wait, but its background child is still
			// initializing (a live runtime record not yet answering the
			// daemon ping). Taking over now would spawn a duplicate daemon
			// that fails on the port/ownership lock and surfaces an opaque
			// error. Release and keep polling: the child's record becomes
			// ping-responsive when ready (the loop-top probe returns it),
			// and if the child dies its record stops being live so takeover
			// proceeds on a later iteration.
			_ = lock.Unlock()
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-ticker.C:
		case <-timer.C:
			return nil, nil, nil
		}
	}
}

// daemonStartInProgress reports whether a live msgvault process holds a
// runtime record that is not yet answering the daemon ping — i.e. a daemon
// child that is still initializing. A released launch lock is not proof the
// previous starter's child died: `serve start` releases the lock after a
// short readiness wait while its child may still be starting up. A record
// that IS answering the ping does not block takeover here (a compatible one
// is already returned by the loop-top probe; an incompatible or
// upgrade-eligible one is stopped by prepareBackgroundDaemonStart).
func daemonStartInProgress(ctx context.Context, dataDir string) (bool, error) {
	records, err := listLiveDaemonRuntimeRecords(dataDir)
	if err != nil {
		return false, err
	}
	for _, rec := range records {
		if _, probeErr := probeDaemonRuntimeRecord(ctx, rec); probeErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, ctxErr
			}
			return true, nil
		}
	}
	return false, nil
}
