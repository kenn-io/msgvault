package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/tiktok"
	"go.kenn.io/msgvault/internal/tiktokwebhook"
)

type tikTokReceiverSnapshot struct {
	Listener  string    `json:"listener"`
	Address   string    `json:"address,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	WrittenAt time.Time `json:"written_at"`
}
type tikTokWebhookRuntime struct {
	server                 *http.Server
	spool                  *tiktokwebhook.Spool
	root, disabledReason   string
	cancel                 context.CancelFunc
	errors                 chan error
	serverDone, workerDone chan struct{}
	serverErr, workerErr   error
	failed, stopped        atomic.Bool
}

func tikTokSpoolRoot(cfg *config.Config) string {
	return filepath.Join(cfg.Data.DataDir, "tiktok-webhooks")
}
func startTikTokWebhooks(ctx context.Context, cfg *config.Config, st *store.Store, idle *api.IdleTracker, gate api.OperationGate, maintenance *attachmentMaintenance) (*tikTokWebhookRuntime, error) {
	return startTikTokWebhooksWithHandler(ctx, cfg, idle, coordinateTikTokDeliveries(gate, maintenance, tikTokDeliveryHandler(cfg, st)))
}

// Every delivery that can publish attachment bytes shares the daemon's archive
// gate and mutation lease with CLI imports, scheduled syncs, and maintenance.
func coordinateTikTokDeliveries(gate api.OperationGate, maintenance *attachmentMaintenance, handle tiktokwebhook.HandlerFunc) tiktokwebhook.HandlerFunc {
	return func(ctx context.Context, app string, raw []byte) (outcome tiktokwebhook.Outcome, err error) {
		if gate != nil {
			var done func()
			var ok bool
			if labeled, yes := gate.(api.LabeledOperationGate); yes {
				done, ok = labeled.BeginLabeledWorkContext(ctx, "TikTok webhook import")
			} else {
				done, ok = gate.BeginWorkContext(ctx)
			}
			if !ok {
				if err := ctx.Err(); err != nil {
					return outcome, err
				}
				return outcome, errors.New("archive is busy or shutting down")
			}
			defer done()
		}
		err = runScheduledSource(ctx, maintenance, true, func(ctx context.Context) error {
			var handleErr error
			outcome, handleErr = handle(ctx, app, raw)
			return handleErr
		})
		return outcome, err
	}
}

func startTikTokWebhooksWithHandler(ctx context.Context, cfg *config.Config, idle *api.IdleTracker, handle tiktokwebhook.HandlerFunc) (*tikTokWebhookRuntime, error) {
	r := &tikTokWebhookRuntime{root: tikTokSpoolRoot(cfg), errors: make(chan error, 2)}
	if !cfg.TikTokWebhooks.Enabled() {
		r.disabledReason = "not configured"
		if _, err := os.Stat(r.root); err == nil {
			return r, r.writeSnapshot()
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return r, nil
	}
	apps := map[string][]byte{}
	for _, app := range cfg.TikTokWebhooks.Apps {
		secret := os.Getenv(app.AppSecretEnv)
		if secret == "" {
			r.disabledReason = app.AppSecretEnv + " missing"
			return r, r.writeSnapshot()
		}
		apps[app.AppID] = []byte(secret)
	}
	bodyCap, spoolCap, retention := cfg.TikTokWebhooks.Effective()
	spool, err := tiktokwebhook.OpenSpool(r.root, spoolCap)
	if err != nil {
		return nil, err
	}
	r.spool = spool
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", cfg.TikTokWebhooks.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen for TikTok webhooks: %w", err)
	}
	wake := make(chan struct{}, 1)
	handler := &tiktokwebhook.Handler{Apps: apps, Spool: spool, MaxBodyBytes: bodyCap, Wake: func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}}
	var httpHandler http.Handler = handler
	if idle != nil {
		httpHandler = idle.Wrap(handler)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.server = &http.Server{Addr: listener.Addr().String(), Handler: httpHandler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return workerCtx }}
	r.serverDone, r.workerDone = make(chan struct{}), make(chan struct{})
	if err := r.writeSnapshot(); err != nil {
		cancel()
		_ = listener.Close()
		return nil, err
	}
	report := func(err error) { r.failed.Store(true); _ = r.writeSnapshot(); r.errors <- err }
	go func() {
		defer close(r.serverDone)
		if err := r.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.serverErr = err
			report(err)
		}
	}()
	go func() {
		defer close(r.workerDone)
		r.workerErr = r.runWorker(workerCtx, idle, wake, retention, handle)
		if r.workerErr != nil {
			report(r.workerErr)
		}
	}()
	return r, nil
}
func (r *tikTokWebhookRuntime) snapshot() tikTokReceiverSnapshot {
	snapshot := tikTokReceiverSnapshot{Listener: "disabled", Reason: r.disabledReason, WrittenAt: time.Now().UTC()}
	if r.server != nil {
		snapshot.Listener, snapshot.Address = "listening", r.server.Addr
		if r.failed.Load() {
			snapshot.Listener = "failed"
		}
		if r.stopped.Load() {
			snapshot.Listener = "stopped"
		}
	}
	return snapshot
}
func (r *tikTokWebhookRuntime) writeSnapshot() error {
	if err := fileutil.SecureMkdirAll(r.root, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(r.snapshot())
	if err != nil {
		return err
	}
	return fileutil.SecureReplaceFile(filepath.Join(r.root, "receiver.json"), data, 0600)
}
func (r *tikTokWebhookRuntime) runWorker(ctx context.Context, idle *api.IdleTracker, wake <-chan struct{}, retention time.Duration, handle tiktokwebhook.HandlerFunc) (retErr error) {
	defer func() {
		if ctx.Err() != nil && errors.Is(retErr, ctx.Err()) {
			retErr = nil
		}
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		done := func() {}
		if idle != nil {
			var ok bool
			done, ok = idle.BeginMaintenanceWorkContext(ctx)
			if !ok {
				return nil
			}
		}
		applied, err := r.spool.Drain(ctx, handle)
		done()
		if err != nil {
			return err
		}
		if applied > 0 {
			requestTikTokWebhookCacheRefresh()
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := r.spool.Prune(ctx, retention); err != nil {
			return err
		}
		if err := r.writeSnapshot(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		case <-ticker.C:
		}
	}
}
func requestTikTokWebhookCacheRefresh() {
	if refresher := daemonCacheRefresher; refresher != nil {
		refresher.RequestAfter(30*time.Second, "tiktok-webhooks")
	}
}

func (r *tikTokWebhookRuntime) Shutdown(ctx context.Context) error {
	if r == nil || r.server == nil || r.stopped.Load() {
		return nil
	}
	err := r.server.Shutdown(ctx)
	r.cancel()
	for _, done := range []chan struct{}{r.serverDone, r.workerDone} {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		}
	}
	r.stopped.Store(true)
	return errors.Join(err, r.serverErr, r.workerErr, r.writeSnapshot())
}
func tikTokMessageImporter(st *store.Store, client *tiktok.Client, cfg *config.Config, profile config.TikTokSource) *tiktok.MessageImporter {
	return tiktok.NewMessageImporter(st, client, tiktok.MessageImportOptions{AttachmentsDir: cfg.AttachmentsDir(), MaxMediaBytes: int64(profile.MaxMediaMB) << 20})
}
func tikTokDeliveryHandler(cfg *config.Config, st *store.Store) tiktokwebhook.HandlerFunc {
	return func(ctx context.Context, app string, raw []byte) (tiktokwebhook.Outcome, error) {
		event, message, err := tiktok.DecodeMessageEvent(app, raw)
		if err != nil {
			return tiktokwebhook.Outcome{Class: "invalid"}, nil
		}
		var profile *config.TikTokSource
		for _, candidate := range cfg.TikTok {
			if candidate.Enabled && candidate.MessagesEnabled && candidate.AppID == app && candidate.AccountID == string(event.Account) {
				profile = &candidate
				break
			}
		}
		if profile == nil {
			return tiktokwebhook.Outcome{Class: "unrouted"}, nil
		}
		if event.Event == "im_receive_msg_eu" {
			return tiktokwebhook.Outcome{Class: "unsupported_eu"}, nil
		}
		if message == nil {
			return tiktokwebhook.Outcome{Class: "unhandled"}, nil
		}
		client, err := configuredTikTokClient(cfg, *profile)
		if err != nil {
			return tiktokwebhook.Outcome{}, err
		}
		defer client.Close()
		sum, err := tikTokMessageImporter(st, client, cfg, *profile).Apply(ctx, app, event, *message)
		if errors.Is(err, store.ErrSourceNotFound) || errors.Is(err, store.ErrSourceRetired) {
			return tiktokwebhook.Outcome{Class: "unrouted"}, nil
		}
		if err != nil {
			if sum != nil && sum.MessagesProcessed > 0 {
				requestTikTokWebhookCacheRefresh()
			}
			return tiktokwebhook.Outcome{}, err
		}
		return tiktokwebhook.Outcome{Class: "applied"}, nil
	}
}
