package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
)

type conversationPolicyDeps struct {
	open   func(context.Context) (*store.Store, func(), error)
	config func(context.Context) *config.Config
}

func newPolicyCmd() *cobra.Command {
	cmd := newPolicyLocalCmd(conversationPolicyDeps{
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
		Use: "list", Short: "List rooms, effective tiers and staged overrides as JSON", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, closeStore, err := deps.open(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore()
			rows, err := st.ListConversationPolicies(cmd.Context())
			if err != nil {
				return err
			}
			views := make([]conversationPolicyView, 0, len(rows))
			cfg := deps.config(cmd.Context())
			for _, row := range rows {
				view := conversationPolicyView{ConversationPolicy: row, Staged: true}
				if cfg != nil {
					view.SelectedOverride = cfg.ConversationOverride(row.SourceType, row.SourceIdentifier, row.ProviderID)
					if view.SelectedOverride != "" {
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
		cmd.AddCommand(&cobra.Command{Use: use, Short: operation + " a staged override (full or cold)", Args: cobra.ExactArgs(argCount),
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
				if err := st.SetConversationPolicyOverride(cmd.Context(), sourceID, args[1], tier); err != nil {
					return err
				}
				if _, err = fmt.Fprintln(cmd.OutOrStdout(), "Conversation override saved (staged; effective tier remains full)."); err != nil {
					return fmt.Errorf("write conversation policy result: %w", err)
				}
				return nil
			},
		})
	}
	return cmd
}

func init() { rootCmd.AddCommand(newPolicyCmd()) }
