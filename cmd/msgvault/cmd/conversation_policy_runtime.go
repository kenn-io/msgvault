package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
)

// Reload only sanitized policy data, preserving the invocation's CLI overrides.
func currentConversationPolicySettingsSource(state *invocation) func() (conversationpolicy.Settings, error) {
	return func() (conversationpolicy.Settings, error) {
		if state == nil || state.cfg == nil {
			return conversationpolicy.Settings{}, errors.New("conversation policy configuration is unavailable")
		}
		snapshot, err := config.ReadConfigFile(state.cfg.ConfigFilePath())
		if err != nil {
			return conversationpolicy.Settings{}, err
		}
		if !snapshot.Exists {
			return conversationpolicy.Settings{}, errors.New("conversation policy configuration file is missing")
		}
		loaded, err := state.cfg.ReloadConfigFile(snapshot)
		if err != nil {
			return conversationpolicy.Settings{}, err
		}
		return loaded.ConversationPolicySettings(), nil
	}
}

// Configuration/expiry failures block consumers.
func initializeConversationPolicy(ctx context.Context, st *store.Store, cfg *config.Config, now time.Time) error {
	if cfg == nil {
		return errors.New("conversation policy configuration is unavailable")
	}
	return initializeConversationPolicySettings(ctx, st, cfg.ConversationPolicySettings(), now)
}

func initializeConversationPolicySettings(ctx context.Context, st *store.Store, settings conversationpolicy.Settings, now time.Time) error {
	if _, err := st.ConfigureConversationPolicy(ctx, settings, now); err != nil {
		return fmt.Errorf("configure conversation policy: %w", err)
	}
	if _, err := st.ExpireConversationPolicy(ctx, now); err != nil {
		return fmt.Errorf("expire conversation policy: %w", err)
	}
	return nil
}

func refreshConversationPolicyRuntime(ctx context.Context, st *store.Store, settings conversationpolicy.Settings, now time.Time) error {
	_, configureErr := st.ConfigureConversationPolicy(ctx, settings, now)
	_, expireErr := st.ExpireConversationPolicy(ctx, now)
	return errors.Join(configureErr, expireErr)
}

func refreshConversationPolicyFromSource(ctx context.Context, st *store.Store, source func() (conversationpolicy.Settings, error), now time.Time) error {
	settings, err := source()
	if err != nil {
		// Refresh with the last validated persisted settings. A broken reload must
		// neither replace them with defaults nor freeze activity attribution.
		reloadErr := fmt.Errorf("reload conversation policy: %w", err)
		stored, readErr := st.ConversationPolicySettingsContext(ctx)
		if readErr != nil {
			_, expiryErr := st.ExpireConversationPolicy(ctx, now)
			return errors.Join(reloadErr, readErr, expiryErr)
		}
		if !conversationPolicyRefreshDue(ctx, st, stored, now) {
			return reloadErr
		}
		return errors.Join(reloadErr, refreshConversationPolicyRuntime(ctx, st, stored, now))
	}
	if !conversationPolicyRefreshDue(ctx, st, settings, now) {
		return nil
	}
	return refreshConversationPolicyRuntime(ctx, st, settings, now)
}

// conversationPolicyRefreshDue lets an idle tick skip the exclusive policy
// lock. A failed check refreshes anyway, which is always safe.
func conversationPolicyRefreshDue(ctx context.Context, st *store.Store, settings conversationpolicy.Settings, now time.Time) bool {
	due, err := st.ConversationPolicyRefreshDue(ctx, settings, now)
	if err != nil {
		loggerFromContext(ctx).Debug("conversation policy refresh check failed; refreshing", "error", err)
		return true
	}
	return due
}

// A bad reload does not stop a scheduled importer: enforce the persisted,
// validated settings before allowing it to ingest new messages.
func refreshConversationPolicyForSync(ctx context.Context, st *store.Store, source func() (conversationpolicy.Settings, error), now time.Time) error {
	settings, err := source()
	if err != nil {
		loggerFromContext(ctx).Warn("conversation policy reload failed; retaining validated settings", "error", err)
		settings, err = st.ConversationPolicySettingsContext(ctx)
		if err != nil {
			return fmt.Errorf("read retained conversation policy: %w", err)
		}
	}
	return initializeConversationPolicySettings(ctx, st, settings, now)
}

func runConversationPolicyRuntime(ctx context.Context, st *store.Store, source func() (conversationpolicy.Settings, error), ticks <-chan time.Time, afterRefresh func(error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			afterRefresh(refreshConversationPolicyFromSource(ctx, st, source, now))
		}
	}
}
