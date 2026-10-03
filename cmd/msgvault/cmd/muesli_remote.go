package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
)

func runMuesliClientAdd(cmd *cobra.Command, args []string) error {
	state := invocationFromCommand(cmd)
	sources, err := resolveMuesliSources(args, state.cfg)
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
	if err == nil {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Muesli source registered. Run msgvault sync-muesli.")
	}
	return err
}

func runMuesliClientSync(cmd *cobra.Command, args []string, meetingID int64) error {
	state := invocationFromCommand(cmd)
	sources, err := resolveMuesliSources(args, state.cfg)
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
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	client.SetBusyNotifier(func(string) {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Muesli daemon is busy; waiting to retry (Ctrl+C to cancel).")
	})
	scan := func(ctx context.Context, source config.MuesliSource) error {
		opts := muesliImportOptions(source)
		opts.AccountEmail, _ = source.EffectiveAccountEmail()
		opts.MeetingID = meetingID
		opts.Full, opts.Limit, opts.StartedAfter = syncMuesliFull, syncMuesliLimit, after
		sender := func(ctx context.Context, req muesli.RemoteRequest) (muesli.RemoteResult, error) {
			req.NoBuildCache = skip
			return client.ImportMuesli(ctx, req)
		}
		summary, scanErr := muesli.ScanRemote(ctx, opts, sender)
		if summary != nil && meetingID == 0 {
			writeMuesliSummary(cmd.OutOrStdout(), summary)
		}
		return scanErr
	}
	if len(watched) > 0 {
		return runMuesliWatch(cmd.Context(), watched, scan, waitMuesliSchedule, func(err error) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Muesli scan failed: %s. The next scheduled rescan will retry.\n", muesliWatchFailureReason(err))
		})
	}
	var failures []error
	for _, source := range sources {
		if err := scan(cmd.Context(), source); err != nil {
			failures = append(failures, err)
			if !muesliRecordErrorsOnly(err) {
				return errors.Join(failures...)
			}
		}
		if err := cmd.Context().Err(); err != nil {
			return errors.Join(errors.Join(failures...), err)
		}
	}
	if force {
		source := sources[0]
		email, _ := source.EffectiveAccountEmail()
		_, err := client.ImportMuesli(cmd.Context(), muesli.RemoteRequest{Action: "refresh", Source: meetingimport.Source{Identifier: source.Identifier, AccountEmail: email}, BuildCache: true})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
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
