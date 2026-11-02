package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
)

type metaWebhookRuntime struct {
	server                 *http.Server
	worker                 *metawebhook.Worker
	spool                  *metawebhook.Spool
	handler                *metawebhook.Handler
	disabledReason, root   string
	cancel                 context.CancelFunc
	serverDone, workerDone chan struct{}
	serverErr, workerErr   error
	errors                 chan error
	failed, stopped        atomic.Bool
}

func startMetaWebhooks(ctx context.Context, cfg *config.Config, st *store.Store) (*metaWebhookRuntime, error) {
	return startMetaWebhooksWithRegistry(ctx, cfg, st, nil)
}

func startMetaWebhooksWithRegistry(ctx context.Context, cfg *config.Config, st *store.Store, reg *metawebhook.Registry) (*metaWebhookRuntime, error) {
	r := &metaWebhookRuntime{root: filepath.Join(cfg.Data.DataDir, "meta-webhooks"), errors: make(chan error, 2)}
	if !cfg.MetaWebhooks.Enabled() {
		r.disabledReason = "not configured"
	}
	apps := map[string]metawebhook.AppCredentials{}
	for _, app := range cfg.MetaWebhooks.Apps {
		secret, verify := os.Getenv(app.AppSecretEnv), os.Getenv(app.VerifyTokenEnv)
		for _, missing := range []struct{ name, value string }{{app.AppSecretEnv, secret}, {app.VerifyTokenEnv, verify}} {
			if strings.TrimSpace(missing.value) == "" {
				r.disabledReason = missing.name + " missing"
				slog.Warn("Meta webhook receiver disabled", "missing_env", missing.name)
				break
			}
		}
		apps[app.AppID] = metawebhook.AppCredentials{Secret: []byte(secret), VerifyToken: verify}
	}
	if r.disabledReason != "" {
		return r, metawebhook.WriteReceiverSnapshot(r.root, r.Snapshot())
	}
	bodyCap, spoolCap, retention := cfg.MetaWebhooks.Effective()
	spool, err := metawebhook.OpenSpool(r.root, spoolCap, time.Now)
	if err != nil {
		return nil, fmt.Errorf("open Meta webhook spool: %w", err)
	}
	r.spool = spool
	if reg == nil {
		reg = metawebhook.NewRegistry()
		registerMetaWebhookHandlers(reg, cfg, st)
	}
	r.worker = metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg, Retention: retention, Snapshot: r.Snapshot, OnApplied: func() {
		if refresher := daemonCacheRefresher; refresher != nil {
			refresher.RequestAfter(30*time.Second, "meta-webhooks")
		}
	}})
	r.handler, err = metawebhook.NewHandler(metawebhook.HandlerOptions{Apps: apps, Spool: spool, MaxBodyBytes: bodyCap, Wake: r.worker.Wake})
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", cfg.MetaWebhooks.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen for Meta webhooks: %w", err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.server = &http.Server{Addr: listener.Addr().String(), Handler: r.handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return workerCtx }}
	r.serverDone, r.workerDone = make(chan struct{}), make(chan struct{})
	report := func(err error) {
		r.failed.Store(true)
		_ = metawebhook.WriteReceiverSnapshot(r.root, r.Snapshot())
		r.errors <- err
	}
	go func() {
		defer close(r.serverDone)
		if err := r.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.serverErr = err
			report(err)
		}
	}()
	go func() {
		defer close(r.workerDone)
		r.workerErr = r.worker.Run(workerCtx)
		if r.workerErr != nil {
			report(r.workerErr)
		}
	}()
	r.worker.Wake() // Recover deliveries accepted before a restart.
	return r, nil
}

func (r *metaWebhookRuntime) Snapshot() metawebhook.ReceiverSnapshot {
	snap := metawebhook.ReceiverSnapshot{Listener: "disabled", DisabledReason: r.disabledReason, WrittenAt: time.Now().UTC()}
	if r.server != nil {
		snap.Listener, snap.Addr = "listening", r.server.Addr
		if r.failed.Load() {
			snap.Listener = "failed"
		}
		if r.stopped.Load() {
			snap.Listener = "stopped"
		}
	}
	if r.handler != nil {
		snap.Counters = r.handler.Counters()
	}
	return snap
}
func (r *metaWebhookRuntime) Shutdown(ctx context.Context) error {
	if r == nil || r.server == nil {
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
	return errors.Join(err, r.serverErr, r.workerErr, metawebhook.WriteReceiverSnapshot(r.root, r.Snapshot()))
}

func registerMetaWebhookHandlers(reg *metawebhook.Registry, cfg *config.Config, st *store.Store) {
	reg.Register("page", "feed", feedHandler(cfg, st, func(token string) (meta.ObjectReader, error) { return meta.NewClient(token) }))
}

func feedHandler(cfg *config.Config, st *store.Store, newReader func(string) (meta.ObjectReader, error)) metawebhook.ItemHandler {
	return func(ctx context.Context, app string, items []metawebhook.Item) []metawebhook.ItemOutcome {
		out := make([]metawebhook.ItemOutcome, len(items))
		groups := map[string][]int{}
		pages := []string{}
		for i, item := range items {
			if _, exists := groups[item.EntryID]; !exists {
				pages = append(pages, item.EntryID)
			}
			groups[item.EntryID] = append(groups[item.EntryID], i)
		}
		for _, page := range pages {
			var profile *config.FacebookCommentsSource
			for _, source := range cfg.FacebookComments {
				if source.Enabled && source.AppID == app && source.PageID == page {
					profile = &source
					break
				}
			}
			indices := groups[page]
			set := func(class metawebhook.Class, reason string) {
				for _, i := range indices {
					out[i] = metawebhook.ItemOutcome{Class: class, ErrorClass: reason, CountAttempt: class == metawebhook.Retry}
				}
			}
			if profile == nil {
				set(metawebhook.Unrouted, "profile_missing")
				continue
			}
			token := os.Getenv(profile.TokenEnv)
			if strings.TrimSpace(token) == "" {
				set(metawebhook.Retry, "token_missing")
				continue
			}
			reader, err := newReader(token)
			if err != nil {
				set(metawebhook.Retry, "token_invalid")
				continue
			}
			events, valid := []meta.FeedEvent{}, []int{}
			for _, i := range indices {
				var ev meta.FeedEvent
				if json.Unmarshal(items[i].Value, &ev) != nil {
					out[i] = metawebhook.ItemOutcome{Class: metawebhook.Invalid, ErrorClass: "invalid_feed"}
					continue
				}
				ev.Time = items[i].Time
				events, valid = append(events, ev), append(valid, i)
			}
			results, err := meta.NewEventImporter(st, reader).ApplyFeedEvents(ctx, app, page, events, meta.EventOptions{})
			for j, i := range valid {
				out[i] = metawebhook.ItemOutcome{Class: metawebhook.Retry, ErrorClass: "archive_unavailable", CountAttempt: true}
				if j < len(results) {
					result := results[j]
					out[i] = metawebhook.ItemOutcome{Class: metawebhook.Class(result.Class), ErrorClass: result.ErrorClass, CountAttempt: result.CountAttempt}
					if err != nil && result.Class == "applied" {
						out[i] = metawebhook.ItemOutcome{Class: metawebhook.Retry, ErrorClass: "archive_unavailable", CountAttempt: true}
					}
				}
			}
		}
		return out
	}
}
