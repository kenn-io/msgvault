package cmd

import (
	"context"
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
	sched.RemoveJob("wakeups")
	if logger == nil {
		logger = slog.Default()
	}
	names := make([]string, 0, len(cfg.Wakeups))
	subs := make([]wakeups.Subscription, 0, len(cfg.Wakeups))
	for _, entry := range cfg.Wakeups {
		names = append(names, entry.Name)
		secret, err := cfg.ResolveWakeupSecret(entry)
		if err != nil {
			logger.Warn("wakeup unavailable", "name", entry.Name, "category", "invalid_secret")
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
	var pruned int
	if err := engine.GatedSetup(ctx, func() error {
		var err error
		pruned, err = st.PruneWakeupStates(ctx, names)
		if err != nil {
			return err
		}
		if err := engine.InitializeMissing(ctx); err != nil {
			if category := wakeups.StoreErrorCategory(err); category != "" {
				logger.Warn("wakeup initialization unavailable", "category", category)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if pruned > 0 {
		logger.Info("wakeup states pruned", "count", pruned)
	}
	if len(subs) == 0 {
		return nil
	}
	return sched.AddJob(scheduler.Job{Name: "wakeups", Interval: 15 * time.Second, Background: true, Run: engine.RunPass})
}
