package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meetingarchive"
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
	return runRecorderClientSync(cmd, sources, recorderSyncFlags{syncMuesliAfter, syncMuesliLimit, syncMuesliFull, syncMuesliWatch, meetingID},
		func(s config.MuesliSource) bool { return s.Enabled }, func(s config.MuesliSource) string { return s.Schedule },
		func(after time.Time, skip bool) *recorderRemoteRun[config.MuesliSource] {
			return newMuesliRemoteRun(cmd, state.cfg, meetingID, after, skip)
		})
}

func newMuesliRemoteRun(cmd *cobra.Command, cfg *config.Config, meetingID int64, after time.Time, skipCache bool) *recorderRemoteRun[config.MuesliSource] {
	r := &recorderRemoteRun[config.MuesliSource]{cmd: cmd, name: "Muesli", kind: "muesli", recordErrors: muesliRecordErrors, tooLarge: muesli.ErrRemoteTooLarge}
	r.scan = func(ctx context.Context, source config.MuesliSource) (_ int64, uploaded bool, _ error) {
		opts := muesliImportOptions(source)
		opts.AccountEmail, _ = source.EffectiveAccountEmail()
		opts.MeetingID, opts.RemoteTarget, opts.LockDir = meetingID, cfg.Remote.URL, muesliSyncStateDir(cfg)
		opts.SendRecording = func(ctx context.Context, upload muesli.RecordingUpload, recording *muesli.Recording) error {
			uploaded = true
			upload.NoBuildCache = skipCache
			return r.client.ImportMuesliRecording(ctx, upload, recording)
		}
		opts.Full, opts.Limit, opts.StartedAfter = syncMuesliFull, syncMuesliLimit, after
		sender := func(ctx context.Context, req muesli.RemoteRequest) (muesli.RemoteResult, error) {
			uploaded = true
			req.NoBuildCache = skipCache
			return r.client.ImportMuesli(ctx, req)
		}
		summary, err := muesli.ScanRemote(ctx, opts, sender)
		if summary != nil && meetingID == 0 {
			writeMuesliSummary(cmd.OutOrStdout(), summary)
		}
		var sourceID int64
		if summary != nil {
			sourceID = summary.SourceID
		}
		return sourceID, uploaded, err
	}
	r.refresh = func(ctx context.Context, source config.MuesliSource) error {
		email, _ := source.EffectiveAccountEmail()
		_, err := r.client.ImportMuesli(ctx, muesli.RemoteRequest{Action: "refresh", Source: meetingimport.Source{Identifier: source.Identifier, AccountEmail: email}, BuildCache: true})
		return err
	}
	return r
}

var muesliRecordErrors = []error{muesli.ErrRemoteValidation, muesli.ErrRemoteTooLarge, meetingarchive.ErrRecordingTooLarge}

func muesliSyncStateDir(cfg *config.Config) string {
	return filepath.Join(cfg.Data.DataDir, "muesli-sync")
}
