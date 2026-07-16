package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
)

const (
	backgroundServeReadyTimeout = 5 * time.Second
	serveAPIShutdownTimeout     = 10 * time.Second
	serveSchedulerStopTimeout   = 30 * time.Second
	serveOperationDrainTimeout  = 30 * time.Minute
	serveStopGraceTimeout       = serveAPIShutdownTimeout + serveSchedulerStopTimeout + serveOperationDrainTimeout + 5*time.Second
	serveBackgroundChildEnv     = "MSGVAULT_BACKGROUND_DAEMON"
)

// serveStopQuietWindow is how long `serve stop` waits silently before
// explaining what the daemon is still doing; serveStopProgressInterval paces
// the "still waiting" updates after that. Variables only so tests can shorten
// them.
var (
	serveStopQuietWindow      = 2 * time.Second
	serveStopProgressInterval = 30 * time.Second
)

var (
	startServeBackgroundProcessForRun = startServeBackgroundProcess
	waitForBackgroundServeReadyForRun = waitForBackgroundServeReady
	stopDaemonRuntimeForUpgrade       = stopDaemonRuntimeForUpgradeImpl
	requestDaemonShutdownForRun       = requestDaemonShutdown
)

func newLifecycleCommand(name string, hidden bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:    name,
		Hidden: hidden,
		Args:   cobra.NoArgs,
	}
	switch name {
	case "start":
		cmd.Short = "Start msgvault daemon in the background"
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			return runServeStart(cmd, cfg)
		}
	case "status":
		cmd.Short = "Show msgvault daemon status"
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			return runServeStatusWithAPIKey(cmd, cfg.Data.DataDir, cfg.Server.APIKey)
		}
	case "stop":
		cmd.Short = "Stop msgvault daemon"
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			return runServeStopWithAPIKey(cmd, cfg.Data.DataDir, cfg.Server.APIKey)
		}
	case "restart":
		cmd.Short = "Restart msgvault daemon in the background"
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			return runServeRestart(cmd, cfg)
		}
	default:
		panic("unknown daemon lifecycle command: " + name)
	}
	return cmd
}

func addServeLifecycleCommands(parent *cobra.Command) {
	for _, name := range []string{"start", "status", "stop", "restart"} {
		parent.AddCommand(newLifecycleCommand(name, true))
	}
}

func newDaemonCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the background daemon",
		Args:  cobra.NoArgs,
	}
	for _, name := range []string{"start", "status", "stop", "restart"} {
		cmd.AddCommand(newLifecycleCommand(name, false))
	}
	return cmd
}

var daemonCmd = newDaemonCommand()

func runServeStatus(cmd *cobra.Command, dataDir string) error {
	return runServeStatusWithAPIKey(cmd, dataDir, "")
}

func runServeStatusWithAPIKey(cmd *cobra.Command, dataDir string, apiKey string) error {
	out := cmd.OutOrStdout()
	if rt := findDaemonRuntime(dataDir); rt != nil {
		lines := serveStatusLines(rt)
		if health := fetchDaemonHealthWithAPIKey(cmd.Context(), urlFromDaemonRuntime(rt), apiKey); health != nil {
			lines = append(lines, vectorStatusLines(health.Vector)...)
			lines = append(lines, operationStatusLines(health.Operation)...)
		}
		for _, line := range lines {
			_, _ = fmt.Fprintln(out, line)
		}
		return nil
	}
	recs, err := listLiveDaemonRuntimeRecords(dataDir)
	if err != nil {
		return err
	}
	if len(recs) > 0 {
		_, _ = fmt.Fprintf(out,
			"msgvault process running (pid %d) but not responding to daemon ping.\n",
			recs[0].PID,
		)
		return nil
	}
	_, _ = fmt.Fprintln(out, "No msgvault daemon is running.")
	return nil
}

func serveStatusLines(rt *DaemonRuntime) []string {
	lines := []string{
		"msgvault running at " + urlFromDaemonRuntime(rt),
		fmt.Sprintf("  pid:     %d", rt.Record.PID),
	}
	if rt.Record.Version != "" {
		lines = append(lines, "  version: "+rt.Record.Version)
	}
	if rt.APISchemaVersion != "" {
		lines = append(lines, "  api:     "+rt.APISchemaVersion)
	}
	if !rt.Record.StartedAt.IsZero() {
		lines = append(lines, fmt.Sprintf("  uptime:  %s",
			time.Since(rt.Record.StartedAt).Round(time.Second)))
	}
	return lines
}

// fetchDaemonHealth fetches /health from a running daemon. Best-effort: any
// transport/decode failure returns nil and callers simply omit the health
// details.
func fetchDaemonHealth(ctx context.Context, baseURL string) *api.HealthResponse {
	return fetchDaemonHealthWithAPIKey(ctx, baseURL, "")
}

// fetchDaemonHealthWithAPIKey prefers the authenticated health endpoint so
// local lifecycle commands can show detailed operation status when they have
// the daemon's configured API key. It falls back to public /health for older
// daemons, keyless callers, or auth mismatch.
func fetchDaemonHealthWithAPIKey(ctx context.Context, baseURL string, apiKey string) *api.HealthResponse {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	if health := fetchDaemonHealthEndpoint(ctx, baseURL+"/api/v1/health", apiKey); health != nil {
		return health
	}
	return fetchDaemonHealthEndpoint(ctx, baseURL+"/health", "")
}

func fetchDaemonHealthEndpoint(ctx context.Context, url string, apiKey string) *api.HealthResponse {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var health api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil
	}
	return &health
}

func vectorStatusLines(vh *api.VectorHealth) []string {
	if vh == nil {
		return nil
	}
	line := "  vector:  " + vh.Status
	if vh.Error != "" {
		line += " (" + vh.Error + ")"
	}
	return []string{line}
}

func operationStatusLines(op *api.OperationHealth) []string {
	if op == nil || (!op.Busy && op.Label == "") {
		return nil
	}
	if op.Label == "" {
		return []string{"  busy:    archive operation in progress"}
	}
	if op.StartedAt == nil {
		return []string{"  busy:    " + op.Label}
	}
	return []string{fmt.Sprintf("  busy:    %s (running for %s)",
		op.Label, time.Since(*op.StartedAt).Round(time.Second))}
}

func daemonRunningLine(state string, rt *DaemonRuntime, pid int) string {
	return fmt.Sprintf("msgvault %s at %s (pid %d)\n", state, urlFromDaemonRuntime(rt), pid)
}

type backgroundDaemonStartPreparation struct {
	Reusable *DaemonRuntime
}

type backgroundServeStartOptions struct {
	ExecutablePath string
}

func prepareBackgroundDaemonStart(
	c *config.Config,
	incompatibleGuidance string,
) (backgroundDaemonStartPreparation, error) {
	if rt := findDaemonRuntime(c.Data.DataDir); rt != nil {
		if !shouldUpgradeDaemonRuntimeWithPolicy(rt, Version, c.Server.DaemonAutoRestart) {
			return backgroundDaemonStartPreparation{Reusable: rt}, nil
		}
		if err := stopDaemonRuntimeForUpgrade(*c, rt); err != nil {
			return backgroundDaemonStartPreparation{}, fmt.Errorf("stop older daemon before restart: %w", err)
		}
	}
	rt, foundIncompatible, compatErr := findIncompatibleDaemonRuntime(c.Data.DataDir)
	if compatErr != nil && !foundIncompatible {
		return backgroundDaemonStartPreparation{}, fmt.Errorf("inspect daemon runtimes: %w", compatErr)
	}
	if foundIncompatible {
		if !shouldUpgradeIncompatibleDaemonRuntimeWithPolicy(rt, Version, c.Server.DaemonAutoRestart) {
			return backgroundDaemonStartPreparation{}, incompatibleDaemonError(compatErr, incompatibleGuidance)
		}
		if err := stopDaemonRuntimeForUpgrade(*c, rt); err != nil {
			return backgroundDaemonStartPreparation{}, fmt.Errorf("stop older daemon before restart: %w", err)
		}
	}
	return backgroundDaemonStartPreparation{}, nil
}

func incompatibleDaemonError(err error, guidance string) error {
	return fmt.Errorf("incompatible daemon is already running: %w; %s", err, guidance)
}

func runServeStart(cmd *cobra.Command, c *config.Config) error {
	return runServeStartWithOptions(cmd, c, backgroundServeStartOptions{})
}

func runServeStartWithOptions(cmd *cobra.Command, c *config.Config, opts backgroundServeStartOptions) error {
	if c == nil {
		return errors.New("nil config")
	}
	if err := os.MkdirAll(c.Data.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	launchLock, ok := acquireBackgroundLaunchLock(c.Data.DataDir)
	if !ok {
		reportBackgroundLaunchInProgress(cmd, c.Data.DataDir)
		return nil
	}
	defer func() { _ = launchLock.Unlock() }()

	prep, err := prepareBackgroundDaemonStart(c, "run `msgvault serve stop` before starting this version")
	if err != nil {
		return err
	}
	if rt := prep.Reusable; rt != nil {
		_, _ = fmt.Fprint(cmd.OutOrStdout(), daemonRunningLine("already running", rt, rt.Record.PID))
		return nil
	}

	proc, err := startServeBackgroundProcessForRun(c, opts)
	if err != nil {
		return fmt.Errorf("start background daemon: %w", err)
	}
	rt, ready, err := waitForBackgroundServeReadyForRun(
		cmd.Context(), c.Data.DataDir, proc.Wait, backgroundServeReadyTimeout,
	)
	if err != nil {
		return backgroundServeStartupError(err, proc)
	}
	if ready {
		_, _ = fmt.Fprint(cmd.OutOrStdout(), daemonRunningLine("running", rt, proc.PID))
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Logs: %s\n", proc.LogPath)
		return nil
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"msgvault starting in background (pid %d)\n", proc.PID)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Logs: %s\n", proc.LogPath)
	return nil
}

func runServeStopWithAPIKey(cmd *cobra.Command, dataDir string, apiKey string) error {
	return stopLiveDaemonsWithAPIKey(cmd, dataDir, apiKey, false)
}

func runServeRestart(cmd *cobra.Command, c *config.Config) error {
	if c == nil {
		return errors.New("nil config")
	}
	if err := stopLiveDaemonsWithAPIKey(cmd, c.Data.DataDir, c.Server.APIKey, true); err != nil {
		return err
	}
	return runServeStart(cmd, c)
}

func stopLiveDaemons(cmd *cobra.Command, dataDir string, quietNoDaemon bool) error {
	return stopLiveDaemonsWithAPIKey(cmd, dataDir, "", quietNoDaemon)
}

func stopLiveDaemonsWithAPIKey(cmd *cobra.Command, dataDir string, apiKey string, quietNoDaemon bool) error {
	records, err := listLiveDaemonRuntimeRecords(dataDir)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		if !quietNoDaemon {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No msgvault daemon is running.")
		}
		return nil
	}
	stopped := 0
	skipped := 0
	for _, rec := range records {
		if !stopTargetConfirmed(rec) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"Skipping pid %d: cannot confirm it is the recorded msgvault daemon.\n",
				rec.PID)
			skipped++
			continue
		}
		if err := stopDaemonProcess(cmd.OutOrStdout(), rec, apiKey, serveStopGraceTimeout); err != nil {
			return fmt.Errorf("stop pid %d: %w", rec.PID, err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Stopped msgvault (pid %d).\n", rec.PID)
		stopped++
	}
	if stopped == 0 && skipped > 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(),
			"No msgvault daemon was stopped; runtime records may be stale.")
	}
	return nil
}

func stopDaemonRuntimeForUpgradeImpl(c config.Config, rt *DaemonRuntime) error {
	if rt == nil {
		return nil
	}
	if !stopTargetConfirmed(rt.Record) {
		return fmt.Errorf("cannot confirm pid %d is the recorded msgvault daemon", rt.Record.PID)
	}
	if err := stopDaemonProcess(os.Stdout, rt.Record, c.Server.APIKey, serveStopGraceTimeout); err != nil {
		return fmt.Errorf("stop pid %d: %w", rt.Record.PID, err)
	}
	return nil
}

func stopTargetConfirmed(rec daemon.RuntimeRecord) bool {
	return daemonRecordPingConfirmed(rec) || processIdentityConfirmed(rec)
}

func daemonRecordPingConfirmed(rec daemon.RuntimeRecord) bool {
	info, err := probeDaemonRuntimeRecord(context.Background(), rec)
	return err == nil && info.PID == rec.PID
}

func processIdentityConfirmed(rec daemon.RuntimeRecord) bool {
	if rec.Metadata == nil {
		return false
	}
	return processCreateTimeMatches(rec.PID, rec.Metadata[runtimeCreateTime])
}

func stopDaemonProcess(out io.Writer, rec daemon.RuntimeRecord, apiKey string, grace time.Duration) error {
	process, err := os.FindProcess(rec.PID)
	if err != nil {
		return fmt.Errorf("find process: %w", err)
	}
	// Capture what the daemon is working on BEFORE requesting shutdown: the
	// API listener closes as soon as shutdown starts, so this is the last
	// chance to learn what a long operation drain is waiting on.
	op := fetchDaemonOperation(rec, apiKey)
	shutdownRequested, shutdownErr := requestDaemonShutdownForRun(rec)
	if shutdownErr != nil {
		logger.Warn("daemon shutdown request failed; falling back to process signal",
			"pid", rec.PID, "error", shutdownErr)
	}
	if !shutdownRequested {
		if err := signalDaemonProcess(process); err != nil {
			return fmt.Errorf("signal process: %w", err)
		}
	}
	if waitForDaemonExitWithProgress(out, rec, op, grace, daemonProbeTick, recordedDaemonStillPresent) {
		return nil
	}
	_, _ = fmt.Fprintf(out, "msgvault (pid %d) did not exit within %s; force-killing it.\n",
		rec.PID, grace.Round(time.Second))
	if err := killDaemonProcess(process); err != nil {
		return fmt.Errorf("kill process: %w", err)
	}
	if waitForRecordedDaemonExit(rec, grace, daemonProbeTick, recordedDaemonStillPresent) {
		return nil
	}
	return errors.New("process still alive")
}

// fetchDaemonOperation asks a running daemon what archive operation it is
// working on. Best-effort: nil when the daemon is idle or unreachable.
func fetchDaemonOperation(rec daemon.RuntimeRecord, apiKey string) *api.OperationHealth {
	url := urlFromDaemonRuntime(daemonRuntimeFromRecord(rec))
	if url == "" {
		return nil
	}
	health := fetchDaemonHealthWithAPIKey(context.Background(), url, apiKey)
	if health == nil {
		return nil
	}
	return health.Operation
}

// waitForDaemonExitWithProgress waits like waitForRecordedDaemonExit but
// explains long waits: fast exits stay quiet, while a daemon that is still
// draining work after serveStopQuietWindow gets described (what it is
// finishing and how long the wait is bounded to) with periodic progress
// updates until it exits or the grace deadline passes.
func waitForDaemonExitWithProgress(
	out io.Writer,
	rec daemon.RuntimeRecord,
	op *api.OperationHealth,
	grace time.Duration,
	tick time.Duration,
	stillPresent func(daemon.RuntimeRecord) bool,
) bool {
	start := time.Now()
	quiet := min(serveStopQuietWindow, grace)
	if waitForRecordedDaemonExit(rec, quiet, tick, stillPresent) {
		return true
	}
	deadline := start.Add(grace)
	if !time.Now().Before(deadline) {
		return false
	}
	_, _ = fmt.Fprint(out, describeDaemonStopWait(rec.PID, op, grace))
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		chunk := min(serveStopProgressInterval, remaining)
		if waitForRecordedDaemonExit(rec, chunk, tick, stillPresent) {
			return true
		}
		if time.Until(deadline) <= 0 {
			return false
		}
		_, _ = fmt.Fprintf(out, "Still waiting for msgvault (pid %d) to exit (%s elapsed)...\n",
			rec.PID, time.Since(start).Round(time.Second))
	}
}

func describeDaemonStopWait(pid int, op *api.OperationHealth, grace time.Duration) string {
	var b strings.Builder
	if op != nil && op.Label != "" {
		if op.StartedAt != nil {
			fmt.Fprintf(&b, "msgvault (pid %d) is finishing %s (running for %s) before exiting.\n",
				pid, op.Label, time.Since(*op.StartedAt).Round(time.Second))
		} else {
			fmt.Fprintf(&b, "msgvault (pid %d) is finishing %s before exiting.\n",
				pid, op.Label)
		}
	} else if op != nil && op.Busy {
		fmt.Fprintf(&b, "msgvault (pid %d) is finishing an archive operation before exiting.\n",
			pid)
	}
	fmt.Fprintf(&b, "Waiting up to %s for msgvault (pid %d) to exit; "+
		"press Ctrl+C to stop waiting (shutdown continues in the daemon).\n",
		grace.Round(time.Second), pid)
	return b.String()
}

func waitForRecordedDaemonExit(
	rec daemon.RuntimeRecord,
	grace time.Duration,
	tick time.Duration,
	stillPresent func(daemon.RuntimeRecord) bool,
) bool {
	deadline := time.Now().Add(grace)
	for {
		if !stillPresent(rec) {
			removeRuntimeRecord(rec)
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(tick)
	}
}

func requestDaemonShutdown(rec daemon.RuntimeRecord) (bool, error) {
	if rec.Metadata == nil || rec.Metadata[runtimeShutdownToken] == "" {
		return false, nil
	}
	rt := daemonRuntimeFromRecord(rec)
	url := urlFromDaemonRuntime(rt)
	if url == "" {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+api.DaemonShutdownPath, nil)
	if err != nil {
		return false, fmt.Errorf("create shutdown request: %w", err)
	}
	req.Header.Set(api.DaemonShutdownTokenHeader, rec.Metadata[runtimeShutdownToken])

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("send shutdown request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK, http.StatusNoContent:
		return true, nil
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden, http.StatusMethodNotAllowed:
		return false, nil
	default:
		return false, fmt.Errorf("shutdown endpoint returned %s", resp.Status)
	}
}

func recordedDaemonStillPresent(rec daemon.RuntimeRecord) bool {
	if !daemon.ProcessAlive(rec.PID) {
		return false
	}
	if rec.Metadata == nil || rec.Metadata[runtimeCreateTime] == "" {
		return true
	}
	return processCreateTimeMatches(rec.PID, rec.Metadata[runtimeCreateTime])
}

func removeRuntimeRecord(rec daemon.RuntimeRecord) {
	if rec.SourcePath != "" {
		_ = os.Remove(rec.SourcePath)
	}
}

func backgroundLaunchLockPath(dataDir string) string {
	return filepath.Join(dataDir, "serve.background.lock")
}

func acquireBackgroundLaunchLock(dataDir string) (*flock.Flock, bool) {
	lock := flock.New(backgroundLaunchLockPath(dataDir))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		return nil, false
	}
	return lock, true
}

func reportBackgroundLaunchInProgress(cmd *cobra.Command, dataDir string) {
	if rt := waitForBackgroundRuntime(cmd.Context(), dataDir, backgroundServeReadyTimeout); rt != nil {
		_, _ = fmt.Fprint(cmd.OutOrStdout(), daemonRunningLine("already running", rt, rt.Record.PID))
		return
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "msgvault serve start is already in progress.")
}

type backgroundServeProcess struct {
	PID     int
	LogPath string
	Wait    <-chan error
}

func startServeBackgroundProcess(c *config.Config, opts backgroundServeStartOptions) (*backgroundServeProcess, error) {
	exe := opts.ExecutablePath
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("find executable: %w", err)
		}
	}
	logPath := filepath.Join(c.Data.DataDir, "serve.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open serve log: %w", err)
	}
	closeLog := true
	defer func() {
		if closeLog {
			_ = logFile.Close()
		}
	}()
	if _, err := fmt.Fprintf(logFile,
		"\n--- msgvault serve background start %s ---\n",
		time.Now().Format(time.RFC3339),
	); err != nil {
		return nil, fmt.Errorf("write serve log header: %w", err)
	}

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, fmt.Errorf("open null device: %w", err)
	}
	defer func() { _ = devNull.Close() }()

	//nolint:gosec // exe is this binary and args are reconstructed from fixed global flags.
	child := exec.Command(exe, serveBackgroundChildArgs()...)
	child.Env = append(os.Environ(), "MSGVAULT_HOME="+c.HomeDir, serveBackgroundChildEnv+"=1")
	child.Stdin = devNull
	child.Stdout = logFile
	child.Stderr = logFile
	configureServeBackgroundCommand(child)
	if err := child.Start(); err != nil {
		return nil, fmt.Errorf("start server: %w", err)
	}
	closeLog = false
	_ = logFile.Close()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- child.Wait()
	}()
	return &backgroundServeProcess{
		PID:     child.Process.Pid,
		LogPath: logPath,
		Wait:    waitCh,
	}, nil
}

func serveBackgroundChildArgs() []string {
	args := make([]string, 0, 16)
	if cfgFile != "" {
		args = append(args, "--config", cfgFile)
	}
	if homeDir != "" {
		args = append(args, "--home", homeDir)
	}
	if verbose {
		args = append(args, "--verbose")
	}
	if logFile != "" {
		args = append(args, "--log-file", logFile)
	}
	if logLevel != "" {
		args = append(args, "--log-level", logLevel)
	}
	if noLogFile {
		args = append(args, "--no-log-file")
	}
	if logSQL {
		args = append(args, "--log-sql")
	}
	if logSQLSlow != 0 {
		args = append(args, "--log-sql-slow-ms", strconv.FormatInt(logSQLSlow, 10))
	}
	return append(args, "serve")
}

func waitForBackgroundServeReady(
	ctx context.Context,
	dataDir string,
	waitCh <-chan error,
	timeout time.Duration,
) (*DaemonRuntime, bool, error) {
	if timeout <= 0 {
		timeout = backgroundServeReadyTimeout
	}
	return waitForDaemonRuntime(ctx, dataDir, timeout, daemonRuntimeReady, waitCh)
}

func waitForBackgroundRuntime(ctx context.Context, dataDir string, timeout time.Duration) *DaemonRuntime {
	rt, ready, _ := waitForDaemonRuntime(ctx, dataDir, timeout, daemonRuntimeReady, nil)
	if !ready {
		return nil
	}
	return rt
}

func waitForDaemonRuntime(
	ctx context.Context,
	dataDir string,
	timeout time.Duration,
	accept func(*DaemonRuntime) bool,
	waitCh <-chan error,
) (*DaemonRuntime, bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(daemonProbeTick)
	defer ticker.Stop()
	for {
		rt, err := findCompatibleDaemonRuntimeContext(ctx, dataDir)
		if err != nil {
			return nil, false, err
		}
		if accept(rt) {
			return rt, true, nil
		}
		select {
		case err := <-waitCh:
			if err == nil {
				err = errors.New("server process exited")
			}
			return nil, false, err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-ticker.C:
		case <-timer.C:
			return nil, false, nil
		}
	}
}

func daemonRuntimeReady(rt *DaemonRuntime) bool {
	return rt != nil
}
