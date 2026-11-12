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
	server  *http.Server
	cancel  context.CancelFunc
	release func() // ends the receiver's hold on idle shutdown
	done    chan struct{}
	errs    chan error
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
		{Object: "page", Name: "feed"}:               metaFeedHandler(cfg, st, work),
		{Object: "instagram", Name: "comments"}:      instagramCommentsHandler(cfg, st, work),
		{Object: "instagram", Name: "live_comments"}: instagramCommentsHandler(cfg, st, work),
		{Object: "instagram", Name: "mentions"}:      instagramMentionsHandler(cfg, st, work),
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
	// Meta cannot start a daemon, so a daemon that would idle out would lose
	// every delivery Meta gives up on. The listener holds the daemon up
	// instead of any request, so unsigned requests do not count as activity.
	release, ok := idle.BeginMaintenanceWorkContext(ctx)
	if !ok {
		_ = listener.Close()
		return nil, errors.New("meta webhook receiver: daemon is stopping")
	}
	if idle != nil {
		slog.Info("Meta webhook receiver keeps the daemon from idle shutdown while it listens")
	}
	runCtx, cancel := context.WithCancel(ctx)
	r := &metaWebhookRuntime{
		server: &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second},
		cancel: cancel, release: release, done: make(chan struct{}), errs: make(chan error, 2),
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := r.server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			release()
			r.errs <- fmt.Errorf("meta webhook listener: %w", err)
		}
	})
	wg.Go(func() {
		if err := worker.Run(runCtx); runCtx.Err() == nil {
			release()
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
	r.release()
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
		return applyMetaWebhook(ctx, work, func() error {
			return meta.ApplyFeed(ctx, st, client, change.App, change.EntryID, change.Values)
		})
	}
}

// instagramCommentsHandler applies Instagram comment and live comment changes
// to the comment source of the enabled [[instagram_comments]] profile for the
// app and account.
func instagramCommentsHandler(cfg *config.Config, st *store.Store, work scheduler.WorkTracker) metawebhook.FieldHandler {
	return func(ctx context.Context, change metawebhook.Change) error {
		i := slices.IndexFunc(cfg.InstagramComments, func(p config.InstagramCommentsSource) bool {
			return p.Enabled && p.AppID == change.App && p.InstagramAccountID == change.EntryID
		})
		if i < 0 {
			return fmt.Errorf("%w: no enabled [[instagram_comments]] profile for app %s and account %s", metawebhook.ErrUnrecoverable, change.App, change.EntryID)
		}
		profile := cfg.InstagramComments[i]
		client, err := configuredInstagramCommentsClient(profile)
		if err != nil {
			return fmt.Errorf("%w: %w", metawebhook.ErrUnrecoverable, err)
		}
		return applyMetaWebhook(ctx, work, func() error {
			return meta.ApplyInstagramComments(ctx, st, client, profile.AppID, profile.PageID, profile.InstagramAccountID, change.Values)
		})
	}
}

// instagramMentionsHandler applies Instagram mention changes to the mentions
// source of the enabled [[instagram_comments]] profile that sets
// mentions = true for the app and account.
func instagramMentionsHandler(cfg *config.Config, st *store.Store, work scheduler.WorkTracker) metawebhook.FieldHandler {
	return func(ctx context.Context, change metawebhook.Change) error {
		i := slices.IndexFunc(cfg.InstagramComments, func(p config.InstagramCommentsSource) bool {
			return p.Enabled && p.Mentions && p.AppID == change.App && p.InstagramAccountID == change.EntryID
		})
		if i < 0 {
			return fmt.Errorf("%w: no enabled [[instagram_comments]] profile with mentions = true for app %s and account %s", metawebhook.ErrUnrecoverable, change.App, change.EntryID)
		}
		profile := cfg.InstagramComments[i]
		client, err := configuredInstagramCommentsClient(profile)
		if err != nil {
			return fmt.Errorf("%w: %w", metawebhook.ErrUnrecoverable, err)
		}
		return applyMetaWebhook(ctx, work, func() error {
			return meta.ApplyMentions(ctx, st, client, profile.AppID, profile.PageID, profile.InstagramAccountID, change.Values)
		})
	}
}

// applyMetaWebhook runs apply as daemon work, asks for an analytics cache
// refresh, and marks errors that retrying cannot fix.
func applyMetaWebhook(ctx context.Context, work scheduler.WorkTracker, apply func() error) error {
	done, ok := work.BeginWorkContext(ctx)
	if !ok {
		return errors.New("daemon is stopping")
	}
	defer done()
	err := apply()
	// A failed change may follow writes that committed, such as its post.
	if refresher := daemonCacheRefresher; refresher != nil {
		refresher.RequestAfter(30*time.Second, "meta-webhooks")
	}
	return metaWebhookError(err)
}

// graphRetryable reports whether retrying a Graph request can succeed. A
// nonzero Code decides at any status, since Meta can refuse with HTTP 200;
// the status decides only when there is no code.
func graphRetryable(e *meta.APIError) bool {
	switch code := e.Code; {
	case code == 100:
		return false // missing or unreadable object (subcode 33) or invalid request, from Meta's "Handle errors" page
	case code == 10 || code >= 200 && code <= 299 || code == 3:
		return false // permission or capability missing, from Meta's "Handle errors" page
	case code == 190 || code == 102:
		return true // credential failures, from Meta's "Handle errors" page
	case code == 1 || code == 2 || code == 341 || code == 368:
		return true // temporary failures, from Meta's "Handle errors" page
	case code == 4 || code == 17 || code == 32 || code == 613 || code >= 80000 && code <= 80014:
		return true // throttling, including business use cases, from Meta's "Rate limits" page
	case code != 0:
		return true // unknown code: dropping a change cannot be undone, and its retries are bounded
	default:
		return e.Status == http.StatusUnauthorized || e.Status == http.StatusTooManyRequests ||
			e.Status < 400 || e.Status >= 500
	}
}

// metaWebhookError marks errors that retrying a delivery cannot fix, and a
// source that a polling sync holds as busy.
func metaWebhookError(err error) error {
	apiErr, isAPI := errors.AsType[*meta.APIError](err)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrSyncAlreadyActive):
		return fmt.Errorf("%w: %w", metawebhook.ErrBusy, err)
	case errors.Is(err, meta.ErrInvalidChange), errors.Is(err, store.ErrSourceNotFound), errors.Is(err, store.ErrSourceRetired),
		isAPI && !graphRetryable(apiErr):
		return fmt.Errorf("%w: %w", metawebhook.ErrUnrecoverable, err)
	default:
		return err
	}
}
