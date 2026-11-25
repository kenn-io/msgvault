package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
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
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Hex source registered. Keep this running on the recorder for scheduled capture:\n  msgvault sync-hex %s --watch\nHex normally expires unsynced meetings after 24 hours.\n", source.Identifier)
	return nil
}

func runHexClientSync(cmd *cobra.Command, args []string) error {
	state := invocationFromCommand(cmd)
	sources, err := hexSources(state.cfg).selected(args)
	if err != nil {
		return err
	}
	return runRecorderClientSync(cmd, sources, recorderSyncFlags{syncHexAfter, syncHexLimit, syncHexFull, syncHexWatch, 0},
		func(s config.HexSource) bool { return s.Enabled }, func(s config.HexSource) string { return s.Schedule },
		func(after time.Time, skip bool) *recorderRemoteRun[config.HexSource] {
			return newHexRemoteRun(cmd, state.cfg, after, skip)
		})
}

func newHexRemoteRun(cmd *cobra.Command, cfg *config.Config, after time.Time, skipCache bool) *recorderRemoteRun[config.HexSource] {
	r := &recorderRemoteRun[config.HexSource]{cmd: cmd, name: "Hex", kind: "hex", recordErrors: hexRecordErrors, tooLarge: hex.ErrRemoteTooLarge}
	r.scan = func(ctx context.Context, source config.HexSource) (_ int64, uploaded bool, _ error) {
		opts := hexImportOptions(source)
		opts.AccountEmail, _ = source.EffectiveAccountEmail()
		opts.RemoteTarget, opts.LockDir = cfg.Remote.URL, hexSyncStateDir(cfg)
		opts.SendRecording = func(ctx context.Context, upload hex.RecordingUpload, recording *meetingarchive.Recording) error {
			uploaded = true
			upload.NoBuildCache = skipCache
			return r.client.ImportHexRecording(ctx, upload, recording)
		}
		opts.Full, opts.Limit, opts.StartedAfter = syncHexFull, syncHexLimit, after
		sender := func(ctx context.Context, req hex.RemoteRequest) (hex.RemoteResult, error) {
			uploaded = true
			req.NoBuildCache = skipCache
			return r.client.ImportHex(ctx, req)
		}
		summary, err := hex.ScanRemote(ctx, opts, sender)
		if summary != nil {
			writeHexSummary(cmd.OutOrStdout(), summary)
		}
		var sourceID int64
		if summary != nil {
			sourceID = summary.SourceID
		}
		return sourceID, uploaded, err
	}
	r.refresh = func(ctx context.Context, source config.HexSource) error {
		email, _ := source.EffectiveAccountEmail()
		_, err := r.client.ImportHex(ctx, hex.RemoteRequest{Action: "refresh", Source: meetingimport.Source{Identifier: source.Identifier, AccountEmail: email}, BuildCache: true})
		return err
	}
	return r
}

var hexRecordErrors = []error{hex.ErrRemoteValidation, hex.ErrRemoteTooLarge, meetingarchive.ErrRecordingTooLarge}

func hexSyncStateDir(cfg *config.Config) string { return filepath.Join(cfg.Data.DataDir, "hex-sync") }
