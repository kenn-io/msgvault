package cmd

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/wakeups"
)

func configureWakeupJob(ctx context.Context, sched *scheduler.Scheduler, gate api.LabeledOperationGate, st *store.Store, entries []config.WakeupConfig, logger *slog.Logger) error {
	sched.RemoveJob("wakeups")
	if logger == nil {
		logger = slog.Default()
	}
	names := make([]string, 0, len(entries))
	subs := make([]wakeups.Subscription, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
		secret, err := providercredentials.ResolveSecret("", entry.SecretFile, entry.SecretEnv)
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
	var pruned int
	if err := withBeeperMediaGate(ctx, gate, "Wakeup state", func() error {
		var err error
		pruned, err = st.PruneWakeupStates(ctx, names)
		return err
	}); err != nil {
		return err
	}
	slices.SortFunc(subs, func(a, b wakeups.Subscription) int { return strings.Compare(a.Name, b.Name) })
	if pruned > 0 {
		logger.Info("wakeup states pruned", "count", pruned)
	}
	if len(subs) == 0 {
		return nil
	}
	engine := &wakeups.Engine{Store: st, Subscriptions: subs, Gate: beeperMediaGate(gate, "Wakeup state"), Logger: logger}
	return sched.AddJob(scheduler.Job{Name: "wakeups", Interval: 15 * time.Second, Background: true, Run: engine.RunPass})
}
