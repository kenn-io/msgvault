package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
)

func runMuesliClientAdd(cmd *cobra.Command, args []string) error {
	state := invocationFromCommand(cmd)
	sources, err := muesliSources(state.cfg).selected(args)
	if err != nil {
		return err
	}
	if len(sources) != 1 {
		return errors.New("multiple Muesli sources configured; pass an identifier")
	}
	source := sources[0]
	email, err := source.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	if err := probeMuesliDatabase(cmd.Context(), source.EffectiveDBPath()); err != nil {
		return err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	client.SetBusyNotifier(func(string) {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Muesli daemon is busy; waiting to retry (Ctrl+C to cancel).")
	})
	_, err = client.ImportMuesli(cmd.Context(), muesli.RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: source.Identifier, AccountEmail: email}})
	if err != nil {
		return err
	}
	// The daemon may no longer hold meetings this Mac uploaded before, such as
	// after the source was removed, so the next sync offers every meeting.
	opts := muesliImportOptions(source)
	opts.RemoteTarget, opts.LockDir = state.cfg.Remote.URL, muesliSyncStateDir(state.cfg)
	if err := muesli.ForgetRemoteUploads(cmd.Context(), opts); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Muesli source registered. Run msgvault sync-muesli.")
	return nil
}

func runMuesliClientSync(cmd *cobra.Command, args []string, meetingID int64) error {
	state := invocationFromCommand(cmd)
	sources, err := muesliSources(state.cfg).selected(args)
	if err != nil {
		return err
	}
	if meetingID > 0 && len(sources) != 1 {
		return errors.New("muesli completion hook requires exactly one configured source")
	}
	force, skip, err := manualSyncCacheFlags(cmd)
	if err != nil {
		return err
	}
	var after time.Time
	if syncMuesliAfter != "" {
		after, err = time.Parse(time.DateOnly, syncMuesliAfter)
		if err != nil {
			return errors.New("invalid --after (expected YYYY-MM-DD)")
		}
	}
	var watched []muesliWatchSource
	if syncMuesliWatch && meetingID > 0 {
		return errors.New("--watch cannot combine with --meeting-id")
	}
	if syncMuesliWatch && meetingID == 0 {
		if syncMuesliLimit != 0 || syncMuesliAfter != "" || syncMuesliFull || force {
			return errors.New("--watch cannot combine with --limit, --after, --full or --build-cache")
		}
		watched, err = muesliWatchSources(sources)
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
	run := &muesliRemoteRun{cmd: cmd, cfg: state.cfg, meetingID: meetingID, after: after, skipCache: skip}
	defer run.close()
	if len(watched) > 0 {
		return runMuesliWatch(cmd.Context(), watched, run.watchScan, waitMuesliSchedule, func(err error) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Muesli scan failed: %s. The next scheduled rescan will retry.\n", muesliWatchFailureReason(err))
		})
	}
	if err := run.connect(cmd.Context()); err != nil {
		return err
	}
	var failures []error
	var acknowledged, sent []config.MuesliSource
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
			if !muesliRecordErrorsOnly(err) {
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
		if err := requestMuesliBuildCache(cmd.Context(), run.client.ImportMuesli, candidates); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// muesliRemoteRun uploads configured sources to one remote daemon.
type muesliRemoteRun struct {
	cmd       *cobra.Command
	cfg       *config.Config
	meetingID int64
	after     time.Time
	skipCache bool
	client    *daemonclient.Client
}

// connect opens the daemon transport once, including its schema check.
func (r *muesliRemoteRun) connect(ctx context.Context) error {
	if r.client != nil {
		return nil
	}
	client, _, err := OpenHTTPStore(ctx)
	if err != nil {
		return err
	}
	client.SetBusyNotifier(func(string) {
		_, _ = fmt.Fprintln(r.cmd.ErrOrStderr(), "Muesli daemon is busy; waiting to retry (Ctrl+C to cancel).")
	})
	r.client = client
	return nil
}

func (r *muesliRemoteRun) close() {
	if r.client != nil {
		_ = r.client.Close()
	}
}

// scan uploads one source. uploaded reports whether any request was sent,
// since a request that failed may still have committed.
func (r *muesliRemoteRun) scan(ctx context.Context, source config.MuesliSource) (summary *muesli.ImportSummary, uploaded bool, err error) {
	opts := muesliImportOptions(source)
	opts.AccountEmail, _ = source.EffectiveAccountEmail()
	opts.MeetingID, opts.RemoteTarget, opts.LockDir = r.meetingID, r.cfg.Remote.URL, muesliSyncStateDir(r.cfg)
	opts.Full, opts.Limit, opts.StartedAfter = syncMuesliFull, syncMuesliLimit, r.after
	sender := func(ctx context.Context, req muesli.RemoteRequest) (muesli.RemoteResult, error) {
		uploaded = true
		req.NoBuildCache = r.skipCache
		return r.client.ImportMuesli(ctx, req)
	}
	summary, err = muesli.ScanRemote(ctx, opts, sender)
	if summary != nil && r.meetingID == 0 {
		writeMuesliSummary(r.cmd.OutOrStdout(), summary)
	}
	return summary, uploaded, err
}

// watchScan connects on demand, so a watcher started during a daemon outage
// keeps its schedule and resumes once the daemon returns. Errors a rescan
// cannot fix end the watch: a schema mismatch or rejected key while
// connecting, or credentials the daemon rejects later.
func (r *muesliRemoteRun) watchScan(ctx context.Context, source config.MuesliSource) error {
	if err := r.connect(ctx); err != nil {
		if transientDaemonError(err) {
			return err
		}
		return muesliWatchStopError{err: err}
	}
	_, _, err := r.scan(ctx, source)
	if apiErr, ok := errors.AsType[*daemonclient.APIError](err); ok &&
		(apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden) {
		return muesliWatchStopError{err: err}
	}
	return err
}

// transientDaemonError reports failures that a later attempt can clear: the
// daemon or network is down, restarting, rate limited, or behind an
// unavailable gateway.
func transientDaemonError(err error) bool {
	if opErr, ok := errors.AsType[*net.OpError](err); ok {
		return opErr.Op == "dial" || opErr.Op == "read" || opErr.Op == "write"
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return true
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if apiErr, ok := errors.AsType[*daemonclient.APIError](err); ok {
		switch apiErr.Status {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}

// requestMuesliBuildCache asks for an explicit cache build through the first
// candidate the daemon recognizes, moving past sources it does not know. The
// caller lists sources the daemon acknowledged, then sources that sent
// uploads, so a later failure cannot strand meetings already committed. A run
// that sent nothing tries each source, because an unregistered source can be
// configured first.
func requestMuesliBuildCache(ctx context.Context, send muesli.RemoteSender, candidates []config.MuesliSource) error {
	var err error
	for _, source := range candidates {
		email, _ := source.EffectiveAccountEmail()
		_, err = send(ctx, muesli.RemoteRequest{Action: "refresh", Source: meetingimport.Source{Identifier: source.Identifier, AccountEmail: email}, BuildCache: true})
		apiErr, ok := errors.AsType[*daemonclient.APIError](err)
		if !ok || apiErr.Code != "source_not_found" {
			return err
		}
	}
	return err
}

// muesliSyncStateDir holds the recorder's source locks and upload records.
func muesliSyncStateDir(cfg *config.Config) string {
	return filepath.Join(cfg.Data.DataDir, "muesli-sync")
}

// Keep failure diagnostics useful without logging source/contact values or
// arbitrary daemon response text. Fatal API errors take priority in a join.
func muesliWatchFailureReason(err error) string {
	if apiErr, ok := errors.AsType[*daemonclient.APIError](err); ok {
		switch apiErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "daemon credentials were rejected; check [remote] credentials"
		case http.StatusNotFound:
			if apiErr.Code == "source_not_found" {
				return "source is not registered; run msgvault add-muesli"
			}
			return "daemon endpoint is unavailable; check client and daemon versions"
		case http.StatusUnprocessableEntity:
			return "daemon rejected the source or meeting; check registration and recorder identity"
		default:
			return fmt.Sprintf("daemon request failed (HTTP %d)", apiErr.Status)
		}
	}
	if muesliRecordErrorsOnly(err) {
		if errors.Is(err, muesli.ErrRemoteTooLarge) {
			return "a meeting exceeds the 16 MiB transfer limit"
		}
		return "a meeting failed transfer validation; run sync-muesli manually for details"
	}
	return "local files or network are unavailable; run sync-muesli manually for details"
}

// A joined record error must not conceal an authentication or transport failure.
func muesliRecordErrorsOnly(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !muesliRecordErrorsOnly(child) {
				return false
			}
		}
		return true
	}
	return errors.Is(err, muesli.ErrRemoteValidation) || errors.Is(err, muesli.ErrRemoteTooLarge)
}
