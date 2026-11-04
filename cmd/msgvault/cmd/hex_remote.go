package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingimport"
)

func runHexClientAdd(cmd *cobra.Command, args []string) error {
	state := invocationFromCommand(cmd)
	sources, err := hexSources(state.cfg).selected(args)
	if err != nil {
		return err
	}
	if len(sources) != 1 {
		return errors.New("multiple Hex sources configured; pass an identifier")
	}
	source := sources[0]
	email, err := source.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	if err := probeHexMeetings(cmd.Context(), source.EffectiveMeetingsDir()); err != nil {
		return err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	client.SetBusyNotifier(func(string) {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Hex daemon is busy; waiting to retry (Ctrl+C to cancel).")
	})
	_, err = client.ImportHex(cmd.Context(), hex.RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: source.Identifier, AccountEmail: email}})
	if err != nil {
		return err
	}
	// The daemon may no longer hold meetings this Mac uploaded before, such as
	// after the source was removed, so the next sync offers every meeting.
	opts := hexImportOptions(source)
	opts.RemoteTarget, opts.LockDir = state.cfg.Remote.URL, hexSyncStateDir(state.cfg)
	if err := hex.ForgetRemoteUploads(cmd.Context(), opts); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Hex source registered. Run msgvault sync-hex.")
	return nil
}

func runHexClientSync(cmd *cobra.Command, args []string) error {
	state := invocationFromCommand(cmd)
	sources, err := hexSources(state.cfg).selected(args)
	if err != nil {
		return err
	}
	force, skip, err := manualSyncCacheFlags(cmd)
	if err != nil {
		return err
	}
	var after time.Time
	if syncHexAfter != "" {
		after, err = time.Parse(time.DateOnly, syncHexAfter)
		if err != nil {
			return errors.New("invalid --after (expected YYYY-MM-DD)")
		}
	}
	var watched []meetingWatchSource[config.HexSource]
	if syncHexWatch {
		if syncHexLimit != 0 || syncHexAfter != "" || syncHexFull || force {
			return errors.New("--watch cannot combine with --limit, --after, --full or --build-cache")
		}
		watched, err = meetingWatchSources(sources, func(s config.HexSource) bool { return s.Enabled }, func(s config.HexSource) string { return s.Schedule })
		if err != nil {
			return err
		}
	}
	// Validate before opening any transport or source database.
	for _, source := range sources {
		if _, err := source.EffectiveAccountEmail(); err != nil {
			return err
		}
	}
	run := &hexRemoteRun{cmd: cmd, cfg: state.cfg, after: after, skipCache: skip}
	defer run.close()
	if len(watched) > 0 {
		return runMeetingWatch(cmd.Context(), watched, run.watchScan, waitMeetingSchedule, func(err error) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Hex scan failed: %s. The next scheduled rescan will retry.\n", hexWatchFailureReason(err))
		})
	}
	if err := run.connect(cmd.Context()); err != nil {
		return err
	}
	var failures []error
	var acknowledged, sent []config.HexSource
	fatal := false
	for _, source := range sources {
		summary, uploaded, err := run.scan(cmd.Context(), source)
		if summary != nil && summary.SourceID != 0 {
			acknowledged = append(acknowledged, source)
		} else if uploaded {
			sent = append(sent, source)
		}
		if err != nil {
			failures = append(failures, err)
			if !hexRecordErrorsOnly(err) {
				fatal = true
				break
			}
		}
	}
	if err := cmd.Context().Err(); err != nil {
		return errors.Join(errors.Join(failures...), err)
	}
	if force {
		// A failed or unanswered upload may still have committed, so a source
		// that sent anything stays a candidate after the daemon acknowledged
		// none. A run that sent nothing and failed committed nothing.
		candidates := slices.Concat(acknowledged, sent)
		if len(candidates) == 0 && !fatal {
			candidates = sources
		}
		if err := requestHexBuildCache(cmd.Context(), run.client.ImportHex, candidates); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		reportCLISyncFinished(cmd, run.client)
	}
	return errors.Join(failures...)
}

// hexRemoteRun uploads configured sources to one remote daemon.
type hexRemoteRun struct {
	cmd       *cobra.Command
	cfg       *config.Config
	after     time.Time
	skipCache bool
	client    *daemonclient.Client
}

// connect opens the daemon transport once, including its schema check.
func (r *hexRemoteRun) connect(ctx context.Context) error {
	if r.client != nil {
		return nil
	}
	client, _, err := OpenHTTPStore(ctx)
	if err != nil {
		return err
	}
	client.SetBusyNotifier(func(string) {
		_, _ = fmt.Fprintln(r.cmd.ErrOrStderr(), "Hex daemon is busy; waiting to retry (Ctrl+C to cancel).")
	})
	r.client = client
	return nil
}

func (r *hexRemoteRun) close() {
	if r.client != nil {
		_ = r.client.Close()
	}
}

// scan uploads one source. uploaded reports whether any request was sent,
// since a request that failed may still have committed.
func (r *hexRemoteRun) scan(ctx context.Context, source config.HexSource) (summary *hex.ImportSummary, uploaded bool, err error) {
	opts := hexImportOptions(source)
	opts.AccountEmail, _ = source.EffectiveAccountEmail()
	opts.RemoteTarget, opts.LockDir = r.cfg.Remote.URL, hexSyncStateDir(r.cfg)
	opts.SendRecording = func(ctx context.Context, upload hex.RecordingUpload, recording *meetingarchive.Recording) error {
		uploaded = true
		upload.NoBuildCache = r.skipCache
		return r.client.ImportHexRecording(ctx, upload, recording)
	}
	opts.Full, opts.Limit, opts.StartedAfter = syncHexFull, syncHexLimit, r.after
	sender := func(ctx context.Context, req hex.RemoteRequest) (hex.RemoteResult, error) {
		uploaded = true
		req.NoBuildCache = r.skipCache
		return r.client.ImportHex(ctx, req)
	}
	summary, err = hex.ScanRemote(ctx, opts, sender)
	if summary != nil {
		writeHexSummary(r.cmd.OutOrStdout(), summary)
	}
	return summary, uploaded, err
}

// watchScan connects on demand, so a watcher started during a daemon outage
// keeps its schedule and resumes once the daemon returns. Errors a rescan
// cannot fix end the watch: a schema mismatch or rejected key while
// connecting, or credentials the daemon rejects later.
func (r *hexRemoteRun) watchScan(ctx context.Context, source config.HexSource) error {
	if err := r.connect(ctx); err != nil {
		if transientDaemonError(err) {
			return err
		}
		return meetingWatchStopError{err: err}
	}
	_, _, err := r.scan(ctx, source)
	if apiErr, ok := errors.AsType[*daemonclient.APIError](err); ok &&
		(apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden) {
		return meetingWatchStopError{err: err}
	}
	return err
}

// requestHexBuildCache asks for an explicit cache build through the first
// candidate the daemon recognizes, moving past sources it does not know. The
// caller lists sources the daemon acknowledged, then sources that sent
// uploads, so a later failure cannot strand meetings already committed. A run
// that sent nothing tries each source, because an unregistered source can be
// configured first.
func requestHexBuildCache(ctx context.Context, send hex.RemoteSender, candidates []config.HexSource) error {
	var err error
	for _, source := range candidates {
		email, _ := source.EffectiveAccountEmail()
		_, err = send(ctx, hex.RemoteRequest{Action: "refresh", Source: meetingimport.Source{Identifier: source.Identifier, AccountEmail: email}, BuildCache: true})
		apiErr, ok := errors.AsType[*daemonclient.APIError](err)
		if !ok || apiErr.Code != "source_not_found" {
			return err
		}
	}
	return err
}

// hexSyncStateDir holds the recorder's source locks and upload records.
func hexSyncStateDir(cfg *config.Config) string {
	return filepath.Join(cfg.Data.DataDir, "hex-sync")
}

// Keep failure diagnostics useful without logging source/contact values or
// arbitrary daemon response text. Fatal API errors take priority in a join.
func hexWatchFailureReason(err error) string {
	if apiErr, ok := errors.AsType[*daemonclient.APIError](err); ok {
		switch apiErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "daemon credentials were rejected; check [remote] credentials"
		case http.StatusNotFound:
			if apiErr.Code == "source_not_found" {
				return "source is not registered; run msgvault add-hex"
			}
			return "daemon endpoint is unavailable; check client and daemon versions"
		case http.StatusUnprocessableEntity:
			return "daemon rejected the source or meeting; check registration and recorder identity"
		default:
			return fmt.Sprintf("daemon request failed (HTTP %d)", apiErr.Status)
		}
	}
	if hexRecordErrorsOnly(err) {
		if errors.Is(err, meetingarchive.ErrRecordingTooLarge) {
			return "a recording exceeds its size cap; increase recording_max_bytes and the daemon hex_recording_max_bytes, then retry"
		}
		if errors.Is(err, hex.ErrRemoteTooLarge) {
			return "a meeting exceeds the 16 MiB transfer limit"
		}
		return "a meeting failed transfer validation; run sync-hex manually for details"
	}
	return "local files or network are unavailable; run sync-hex manually for details"
}

// A joined record error must not conceal an authentication or transport failure.
func hexRecordErrorsOnly(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !hexRecordErrorsOnly(child) {
				return false
			}
		}
		return true
	}
	return errors.Is(err, hex.ErrRemoteRecordValidation) || errors.Is(err, hex.ErrRemoteValidation) || errors.Is(err, hex.ErrRemoteTooLarge) || errors.Is(err, meetingarchive.ErrRecordingTooLarge)
}
