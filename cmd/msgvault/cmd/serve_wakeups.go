package cmd

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/wakeups"
)

func configureWakeupJob(ctx context.Context, sched *scheduler.Scheduler, gate api.LabeledOperationGate, st *store.Store, cfg *config.Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	names := make([]string, 0, len(cfg.Wakeups))
	subs := make([]wakeups.Subscription, 0, len(cfg.Wakeups))
	for _, entry := range cfg.Wakeups {
		names = append(names, entry.Name)
		secret, err := cfg.ResolveWakeupSecret(entry)
		if err != nil {
			logger.Warn("wakeup unavailable", "name", entry.Name, "category", "secret_unavailable")
			continue
		}
		key, err := wakeups.DecodeSecret(secret)
		if err != nil {
			logger.Warn("wakeup unavailable", "name", entry.Name, "category", "invalid_secret")
			continue
		}
		origin, addresses, err := entry.TrustedDestination()
		if err != nil {
			logger.Warn("wakeup unavailable", "name", entry.Name, "category", "invalid_callback")
			continue
		}
		filter := wakeups.FilterFromConfig(entry)
		subs = append(subs, wakeups.NewSubscription(entry.Name, entry.URL, key, filter, entry.MaxMessageAge, entry.Timeout, origin, addresses))
	}
	slices.SortFunc(subs, func(a, b wakeups.Subscription) int { return strings.Compare(a.Name, b.Name) })
	engine := &wakeups.Engine{Store: st, Subscriptions: subs, Gate: beeperMediaGate(gate, "Wakeup state"), Logger: logger}
	setup := func(ctx context.Context) error {
		return engine.GatedSetup(ctx, func() error {
			pruned, err := st.PruneWakeupStates(ctx, names)
			if err != nil {
				return err
			}
			if pruned > 0 {
				logger.Info("wakeup states pruned", "count", pruned)
			}
			if err := engine.InitializeMissing(ctx); err != nil {
				return err
			}
			warnUnresolvedWakeupSelectors(ctx, st, subs, logger)
			return nil
		})
	}
	logSetupError := func(err error) {
		if category := wakeups.StoreErrorCategory(err); category != "" {
			logger.Warn("wakeup setup unavailable", "category", category)
		}
	}
	setupErr := setup(ctx)
	retrySetup := setupErr != nil
	if retrySetup {
		logSetupError(setupErr)
	}
	if len(subs) == 0 {
		return nil
	}
	return sched.AddJob(scheduler.Job{Name: "wakeups", Interval: 15 * time.Second, Background: true, Run: func(ctx context.Context) error {
		if retrySetup {
			if err := setup(ctx); err != nil {
				logSetupError(err)
			} else {
				retrySetup = false
			}
		}
		return engine.RunPass(ctx)
	}})
}

// warnUnresolvedWakeupSelectors is called only under the setup gate.
func warnUnresolvedWakeupSelectors(ctx context.Context, st *store.Store, subs []wakeups.Subscription, logger *slog.Logger) {
	for _, sub := range subs {
		for _, field := range []struct {
			name string
			ids  []int64
		}{
			{"source_ids", sub.Filter.SourceIDs},
			{"conversation_ids", sub.Filter.ConversationIDs},
		} {
			missing := 0
			for _, id := range field.ids {
				var exists bool
				var err error
				if field.name == "source_ids" {
					_, err = st.GetSourceByIDContext(ctx, id)
					if errors.Is(err, store.ErrSourceNotFound) {
						err = nil
					} else {
						exists = err == nil
					}
				} else {
					exists, err = st.ConversationExistsContext(ctx, id)
				}
				if err != nil {
					if category := wakeups.StoreErrorCategory(err); category != "" {
						logger.Warn("wakeup selector check unavailable", "name", sub.Name, "field", field.name, "category", category)
					}
					continue
				}
				if !exists {
					missing++
				}
			}
			if missing > 0 {
				logger.Warn("wakeup selector unresolved", "name", sub.Name, "field", field.name, "category", "selector_unresolved", "count", missing)
			}
		}
	}
}
