package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
)

type conversationPolicyDeps struct {
	open     func(context.Context) (*store.Store, func(), error)
	openRead func(context.Context) (*store.Store, func(), error)
	config   func(context.Context) *config.Config
}

func newPolicyCmd() *cobra.Command {
	cmd := newPolicyLocalCmd(conversationPolicyDeps{
		openRead: func(ctx context.Context) (*store.Store, func(), error) {
			return openInitializedStoreForInvocation(ctx, invocationFromContext(ctx))
		},
		open: func(ctx context.Context) (*store.Store, func(), error) {
			state := invocationFromContext(ctx)
			if state == nil || state.cfg == nil {
				return nil, nil, errors.New("configuration is unavailable")
			}
			return openWritableStoreAndInitForIngestInvocation(state)
		},
		config: func(ctx context.Context) *config.Config {
			if state := invocationFromContext(ctx); state != nil {
				return state.cfg
			}
			return nil
		},
	})
	for _, child := range cmd.Commands() {
		local := child.RunE
		child.RunE = func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			return local(cmd, args)
		}
	}
	return cmd
}

type conversationPolicyView struct {
	store.ConversationPolicy

	SelectedOverride conversationpolicy.Tier `json:"selected_override,omitempty"`
	OverrideOrigin   string                  `json:"override_origin,omitempty"`
	Staged           bool                    `json:"staged"`
}

func newPolicyLocalCmd(deps conversationPolicyDeps) *cobra.Command {
	cmd := &cobra.Command{Use: "policy", Short: "Show room tiers and overrides"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List rooms, effective tiers and overrides as JSON", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			open := deps.openRead
			if open == nil {
				open = deps.open
			}
			st, closeStore, err := open(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore()
			rows, err := st.ListConversationPolicies(cmd.Context())
			if err != nil {
				return err
			}
			views := make([]conversationPolicyView, 0, len(rows))
			settings, err := st.ConversationPolicySettingsContext(cmd.Context())
			if err != nil {
				return err
			}
			for _, row := range rows {
				view := conversationPolicyView{ConversationPolicy: row}
				for _, override := range settings.Overrides {
					if override.SourceType == row.SourceType && override.SourceIdentifier == row.SourceIdentifier && override.ConversationID == row.ProviderID {
						view.SelectedOverride = override.Tier
						view.OverrideOrigin = "config"
					}
				}
				if row.Override != "" {
					view.SelectedOverride = row.Override
					view.OverrideOrigin = "cli"
				}
				views = append(views, view)
			}
			data, err := json.Marshal(views)
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintln(cmd.OutOrStdout(), string(data)); err != nil {
				return fmt.Errorf("write conversation policies: %w", err)
			}
			return nil
		},
	})
	for _, operation := range []string{"set", "reset"} {
		argCount := 2
		use := operation + " SOURCE_ID CONVERSATION_ID"
		if operation == "set" {
			argCount = 3
			use += " TIER"
		}
		cmd.AddCommand(&cobra.Command{Use: use, Short: operation + " an enforced override (full or cold)", Args: cobra.ExactArgs(argCount),
			RunE: func(cmd *cobra.Command, args []string) error {
				sourceID, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil || sourceID <= 0 {
					return errors.New("SOURCE_ID must be a positive integer from policy list")
				}
				var tier conversationpolicy.Tier
				if operation == "set" {
					tier, err = conversationpolicy.ParseTier(args[2])
					if err != nil {
						return err
					}
				}
				st, closeStore, err := deps.open(cmd.Context())
				if err != nil {
					return err
				}
				defer closeStore()
				cfg := deps.config(cmd.Context())
				if err := initializeConversationPolicy(cmd.Context(), st, cfg, time.Now()); err != nil {
					return err
				}
				if err := st.SetConversationPolicyOverride(cmd.Context(), sourceID, args[1], tier); err != nil {
					return err
				}
				status := "Conversation override enforced; analytics rebuild scheduled when enabled."
				if cleanupErr := cleanupConversationPolicyRuntime(cmd.Context(), st); cleanupErr != nil {
					status += " Derived cleanup pending; the daemon retries each minute."
					loggerFromContext(cmd.Context()).Warn("conversation policy derived cleanup pending", "error", cleanupErr)
				}
				if _, err = fmt.Fprintln(cmd.OutOrStdout(), status); err != nil {
					return fmt.Errorf("write conversation policy result: %w", err)
				}
				return nil
			},
		})
	}
	return cmd
}

func init() { rootCmd.AddCommand(newPolicyCmd()) }
