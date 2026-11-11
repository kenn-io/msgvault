package cmd

import (
	"context"
	"log/slog"
	"net/netip"
	"net/url"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/netguard"
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
		var origin *url.URL
		var addresses []netip.Addr
		if len(entry.TrustedAddresses) > 0 {
			target, err := url.Parse(entry.URL)
			if err != nil {
				logger.Warn("wakeup unavailable", "name", entry.Name, "category", "invalid_callback")
				continue
			}
			target.Path = ""
			target.RawPath = ""
			target.RawQuery = ""
			target.ForceQuery = false
			for _, v := range entry.TrustedAddresses {
				addr, err := netip.ParseAddr(v)
				if err != nil {
					addresses = nil
					break
				}
				addresses = append(addresses, addr)
			}
			origin, addresses, err = netguard.ValidateTrustedDestination(target, addresses)
			if err != nil {
				logger.Warn("wakeup unavailable", "name", entry.Name, "category", "invalid_callback")
				continue
			}
		}
		sources := slices.Clone(entry.SourceIDs)
		slices.Sort(sources)
		sources = slices.Compact(sources)
		if len(sources) == 0 {
			sources = nil
		}
		conversations := slices.Clone(entry.ConversationIDs)
		slices.Sort(conversations)
		conversations = slices.Compact(conversations)
		if len(conversations) == 0 {
			conversations = nil
		}
		incoming := true
		if entry.IncomingOnly != nil {
			incoming = *entry.IncomingOnly
		}
		timeout := entry.Timeout
		if timeout == 0 {
			timeout = 10 * time.Second
		}
		age := entry.MaxMessageAge
		if age == 0 {
			age = 72 * time.Hour
		}
		filter := changefeed.Filter{PersonID: entry.PersonID, SourceIDs: sources, ConversationIDs: conversations, IncomingOnly: incoming}
		subs = append(subs, wakeups.NewSubscription(entry.Name, entry.URL, key, filter, age, timeout, origin, addresses))
	}
	if err := withBeeperMediaGate(ctx, gate, "Wakeup state", func() error { return st.PruneWakeupStates(ctx, names) }); err != nil {
		return err
	}
	if len(subs) == 0 {
		return nil
	}
	engine := &wakeups.Engine{Store: st, Subscriptions: subs, Gate: beeperMediaGate(gate, "Wakeup state"), Logger: logger}
	return sched.AddJob(scheduler.Job{Name: "wakeups", Interval: 15 * time.Second, Background: true, Run: engine.RunPass})
}
