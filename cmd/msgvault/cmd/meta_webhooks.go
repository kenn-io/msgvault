package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

// metaWebhookRuntime is the daemon's Meta webhook listener and worker.
type metaWebhookRuntime struct {
	server *http.Server
	cancel context.CancelFunc
	done   chan struct{}
	errs   chan error
}

// startMetaWebhooks starts the receiver when [meta_webhooks] sets a listen
// address. The returned runtime does nothing when the receiver is not
// configured, or when an app's secret or verify token is missing from the
// daemon environment.
func startMetaWebhooks(ctx context.Context, cfg *config.Config, st *store.Store, idle *api.IdleTracker, work scheduler.WorkTracker) (*metaWebhookRuntime, error) {
	if cfg.MetaWebhooks.ListenAddr == "" {
		return &metaWebhookRuntime{}, nil
	}
	apps := map[string]metawebhook.App{}
	for _, app := range cfg.MetaWebhooks.Apps {
		secret, verify := os.Getenv(app.AppSecretEnv), os.Getenv(app.VerifyTokenEnv)
		if secret == "" || verify == "" {
			slog.Warn("Meta webhook receiver not started: set the app secret and verify token in the daemon environment",
				"app", app.AppID, "app_secret_env", app.AppSecretEnv, "verify_token_env", app.VerifyTokenEnv)
			return &metaWebhookRuntime{}, nil
		}
		apps[app.AppID] = metawebhook.App{Secret: []byte(secret), VerifyToken: verify}
	}
	pending := filepath.Join(cfg.Data.DataDir, "meta-webhooks", "pending")
	worker := metawebhook.NewWorker(pending, map[metawebhook.Field]metawebhook.FieldHandler{
		{Object: "page", Name: "feed"}: metaFeedHandler(cfg, st, work),
	})
	handler, err := metawebhook.NewHandler(apps, pending, worker.Wake)
	if err != nil {
		return nil, err
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", cfg.MetaWebhooks.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen for Meta webhooks: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	r := &metaWebhookRuntime{
		server: &http.Server{Handler: idle.Wrap(handler), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second},
		cancel: cancel, done: make(chan struct{}), errs: make(chan error, 2),
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := r.server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			r.errs <- fmt.Errorf("meta webhook listener: %w", err)
		}
	})
	wg.Go(func() {
		if err := worker.Run(runCtx); runCtx.Err() == nil {
			r.errs <- fmt.Errorf("meta webhook worker: %w", err)
		}
	})
	go func() {
		wg.Wait()
		close(r.done)
	}()
	slog.Info("Meta webhook receiver listening", "addr", listener.Addr().String())
	return r, nil
}

// Errors reports a listener or worker failure. The channel is nil, so it
// never reports, when the receiver did not start.
func (r *metaWebhookRuntime) Errors() <-chan error {
	if r == nil {
		return nil
	}
	return r.errs
}

// Shutdown stops accepting deliveries, then stops the worker. Deliveries not
// yet applied stay on disk for the next start.
func (r *metaWebhookRuntime) Shutdown(ctx context.Context) error {
	if r == nil || r.server == nil {
		return nil
	}
	err := r.server.Shutdown(ctx)
	r.cancel()
	select {
	case <-r.done:
		return err
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
}

// metaFeedHandler applies Page feed changes to the comment source of the
// enabled [[facebook_comments]] profile for the app and Page.
func metaFeedHandler(cfg *config.Config, st *store.Store, work scheduler.WorkTracker) metawebhook.FieldHandler {
	return func(ctx context.Context, change metawebhook.Change) error {
		i := slices.IndexFunc(cfg.FacebookComments, func(p config.FacebookCommentsSource) bool {
			return p.Enabled && p.AppID == change.App && p.PageID == change.EntryID
		})
		if i < 0 {
			return fmt.Errorf("%w: no enabled [[facebook_comments]] profile for app %s and Page %s", metawebhook.ErrUnrecoverable, change.App, change.EntryID)
		}
		client, err := configuredFacebookCommentsClient(cfg.FacebookComments[i])
		if err != nil {
			return fmt.Errorf("%w: %w", metawebhook.ErrUnrecoverable, err)
		}
		done, ok := work.BeginWorkContext(ctx)
		if !ok {
			return errors.New("daemon is stopping")
		}
		defer done()
		err = meta.ApplyFeed(ctx, st, client, change.App, change.EntryID, change.Values)
		if err == nil {
			if refresher := daemonCacheRefresher; refresher != nil {
				refresher.RequestAfter(30*time.Second, "meta-webhooks")
			}
		}
		return metaWebhookError(err)
	}
}

// graphRateLimitCodes are Meta error codes for throttling, which Graph can
// return with a 400 or 403 status.
var graphRateLimitCodes = []int{4, 17, 32, 613}

// metaWebhookError marks errors that retrying a delivery cannot fix.
func metaWebhookError(err error) error {
	apiErr, isAPI := errors.AsType[*meta.APIError](err)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, meta.ErrInvalidChange), errors.Is(err, store.ErrSourceNotFound), errors.Is(err, store.ErrSourceRetired),
		isAPI && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != http.StatusTooManyRequests && !slices.Contains(graphRateLimitCodes, apiErr.Code):
		return fmt.Errorf("%w: %w", metawebhook.ErrUnrecoverable, err)
	default:
		return err
	}
}
