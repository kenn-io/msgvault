package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meetingarchive"
)

type recorderCommandSource interface{ EffectiveAccountEmail() (string, error) }
type recorderSyncFlags struct {
	after       string
	limit       int
	full, watch bool
	meetingID   int64
}

// recorderRemoteRun shares transport, retries and cache orchestration; scan and refresh remain typed provider callbacks.
type recorderRemoteRun[S any] struct {
	cmd          *cobra.Command
	name, kind   string
	client       *daemonclient.Client
	recordErrors []error
	tooLarge     error
	scan         func(context.Context, S) (int64, bool, error)
	refresh      func(context.Context, S) error
}

func runRecorderClientSync[S recorderCommandSource](cmd *cobra.Command, sources []S, flags recorderSyncFlags, enabled func(S) bool, schedule func(S) string, makeRun func(time.Time, bool) *recorderRemoteRun[S]) error {
	force, skip, err := manualSyncCacheFlags(cmd)
	if err != nil {
		return err
	}
	var after time.Time
	if flags.after != "" {
		after, err = time.Parse(time.DateOnly, flags.after)
		if err != nil {
			return errors.New("invalid --after (expected YYYY-MM-DD)")
		}
	}
	var watched []meetingWatchSource[S]
	if flags.watch && flags.meetingID > 0 {
		return errors.New("--watch cannot combine with --meeting-id")
	}
	if flags.watch {
		if flags.limit != 0 || flags.after != "" || flags.full || force {
			return errors.New("--watch cannot combine with --limit, --after, --full or --build-cache")
		}
		watched, err = meetingWatchSources(sources, enabled, schedule)
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
	run := makeRun(after, skip)
	defer run.close()
	if len(watched) > 0 {
		return runMeetingWatch(cmd.Context(), watched, run.watchScan, waitMeetingSchedule, func(err error) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s scan failed: %s. The next scheduled rescan will retry.\n", run.name, recorderWatchFailureReason(run.kind, err, run.recordErrors, run.tooLarge))
		})
	}
	if err := run.connect(cmd.Context()); err != nil {
		return err
	}
	var failures []error
	var acknowledged, sent []S
	fatal := false
	for _, source := range sources {
		sourceID, uploaded, err := run.scan(cmd.Context(), source)
		if sourceID != 0 {
			acknowledged = append(acknowledged, source)
		} else if uploaded {
			sent = append(sent, source)
		}
		if err != nil {
			failures = append(failures, err)
			if !recorderRecordErrorsOnly(err, run.recordErrors) {
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
		if err := requestRecorderBuildCache(cmd.Context(), candidates, run.refresh); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		reportCLISyncFinished(cmd, run.client)
	}
	return errors.Join(failures...)
}

// connect opens the daemon transport once, including its schema check.
func (r *recorderRemoteRun[S]) connect(ctx context.Context) error {
	if r.client != nil {
		return nil
	}
	client, _, err := OpenHTTPStore(ctx)
	if err != nil {
		return err
	}
	client.SetBusyNotifier(func(string) {
		_, _ = fmt.Fprintln(r.cmd.ErrOrStderr(), r.name+" daemon is busy; waiting to retry (Ctrl+C to cancel).")
	})
	r.client = client
	return nil
}

func (r *recorderRemoteRun[S]) close() {
	if r.client != nil {
		_ = r.client.Close()
	}
}

// watchScan connects on demand, so a watcher started during a daemon outage
// keeps its schedule and resumes once the daemon returns. Errors a rescan
// cannot fix end the watch: a schema mismatch or rejected key while
// connecting, or credentials the daemon rejects later.
func (r *recorderRemoteRun[S]) watchScan(ctx context.Context, source S) error {
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

func requestRecorderBuildCache[S any](ctx context.Context, candidates []S, send func(context.Context, S) error) error {
	var err error
	for _, source := range candidates {
		err = send(ctx, source)
		apiErr, ok := errors.AsType[*daemonclient.APIError](err)
		if !ok || apiErr.Code != "source_not_found" {
			return err
		}
	}
	return err
}

// Keep failure diagnostics useful without logging source/contact values or
// arbitrary daemon response text. Fatal API errors take priority in a join.
func recorderWatchFailureReason(kind string, err error, recordErrors []error, tooLarge error) string {
	if apiErr, ok := errors.AsType[*daemonclient.APIError](err); ok {
		switch apiErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "daemon credentials were rejected; check [remote] credentials"
		case http.StatusNotFound:
			if apiErr.Code == "source_not_found" {
				return "source is not registered; run msgvault add-" + kind
			}
			return "daemon endpoint is unavailable; check client and daemon versions"
		case http.StatusUnprocessableEntity:
			return "daemon rejected the source or meeting; check registration and recorder identity"
		default:
			return fmt.Sprintf("daemon request failed (HTTP %d)", apiErr.Status)
		}
	}
	if recorderRecordErrorsOnly(err, recordErrors) {
		if errors.Is(err, meetingarchive.ErrRecordingTooLarge) {
			return "a recording exceeds its size cap; increase recording_max_bytes and the daemon " + kind + "_recording_max_bytes, then retry"
		}
		if errors.Is(err, tooLarge) {
			return "a meeting exceeds the 16 MiB transfer limit"
		}
		return "a meeting failed transfer validation; run sync-" + kind + " manually for details"
	}
	return "local files or network are unavailable; run sync-" + kind + " manually for details"
}

// A joined record error must not conceal an authentication or transport failure.
func recorderRecordErrorsOnly(err error, recordErrors []error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !recorderRecordErrorsOnly(child, recordErrors) {
				return false
			}
		}
		return true
	}
	for _, target := range recordErrors {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
