package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

var (
	startServeBackgroundProcessForRun = startServeBackgroundProcess
	waitForBackgroundServeReadyForRun = waitForBackgroundServeReady
	stopDaemonRuntimeForUpgrade       = stopDaemonRuntimeForUpgradeImpl
	requestDaemonShutdownForRun       = requestDaemonShutdown
)

var serveStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start msgvault daemon in the background",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServeStart(cmd, cfg)
	},
}

var serveStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show msgvault daemon status",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServeStatus(cmd, cfg.Data.DataDir)
	},
}

var serveStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop msgvault daemon",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServeStop(cmd, cfg.Data.DataDir)
	},
}

var serveRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart msgvault daemon in the background",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServeRestart(cmd, cfg)
	},
}

func runServeStatus(cmd *cobra.Command, dataDir string) error {
	out := cmd.OutOrStdout()
	if rt := findDaemonRuntime(dataDir); rt != nil {
		for _, line := range serveStatusLines(rt) {
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
		return fmt.Errorf(
			"server exited before becoming ready: %w\nLogs: %s",
			err, proc.LogPath,
		)
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

func runServeStop(cmd *cobra.Command, dataDir string) error {
	return stopLiveDaemons(cmd, dataDir, false)
}

func runServeRestart(cmd *cobra.Command, c *config.Config) error {
	if c == nil {
		return errors.New("nil config")
	}
	if err := stopLiveDaemons(cmd, c.Data.DataDir, true); err != nil {
		return err
	}
	return runServeStart(cmd, c)
}

func stopLiveDaemons(cmd *cobra.Command, dataDir string, quietNoDaemon bool) error {
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
		if err := stopDaemonProcess(rec, serveStopGraceTimeout); err != nil {
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

func stopDaemonRuntimeForUpgradeImpl(_ config.Config, rt *DaemonRuntime) error {
	if rt == nil {
		return nil
	}
	if !stopTargetConfirmed(rt.Record) {
		return fmt.Errorf("cannot confirm pid %d is the recorded msgvault daemon", rt.Record.PID)
	}
	if err := stopDaemonProcess(rt.Record, serveStopGraceTimeout); err != nil {
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

func stopDaemonProcess(rec daemon.RuntimeRecord, grace time.Duration) error {
	process, err := os.FindProcess(rec.PID)
	if err != nil {
		return fmt.Errorf("find process: %w", err)
	}
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
	if waitForRecordedDaemonExit(rec, grace, daemonProbeTick, recordedDaemonStillPresent) {
		return nil
	}
	if err := killDaemonProcess(process); err != nil {
		return fmt.Errorf("kill process: %w", err)
	}
	if waitForRecordedDaemonExit(rec, grace, daemonProbeTick, recordedDaemonStillPresent) {
		return nil
	}
	return errors.New("process still alive")
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
