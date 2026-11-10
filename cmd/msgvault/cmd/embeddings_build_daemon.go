package cmd

import (
	"context"
	"errors"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
)

func (a *storeAPIAdapter) runCLIEmbeddingsBuild(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	if !a.embeddingsBuildMu.TryLock() {
		return errors.New("embeddings build is already running")
	}
	defer a.embeddingsBuildMu.Unlock()
	return runManagedEmbeddingsCLI(ctx, req, emit, func(root *cobra.Command) {
		newCommand := func(use string, rebuild bool) *cobra.Command {
			var opts managedEmbedOptions
			cmd := &cobra.Command{Use: use, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
				closeLogs, err := setupManagedEmbeddingsLogging(cmd)
				if err != nil {
					return err
				}
				defer closeLogs()
				state := invocationFromCommand(cmd)
				if state == nil || state.cfg == nil {
					return errors.New("configuration is unavailable")
				}
				if !state.cfg.Vector.Enabled {
					return errors.New("vector search not enabled; add [vector] enabled=true to config.toml first")
				}
				if (!state.cfg.Vector.Embeddings.LocalOnly && state.cfg.Vector.Embeddings.Endpoint == "") || state.cfg.Vector.Embeddings.Model == "" {
					return errors.New("[vector.embeddings] endpoint and model are required")
				}
				lease, err := acquireLabeledEmbeddingsGate(cmd.Context(), a.operationGate, "msgvault embeddings build")
				if err != nil {
					return err
				}
				defer lease.close()
				opts.yield = lease.yield
				opts.lookupEnv = func(name string) (string, bool) {
					if value, ok := req.Env[name]; ok {
						return value, true
					}
					return os.LookupEnv(name)
				}
				if a.embeddingsScheduler != nil {
					opts.passContext = func(ctx context.Context) (context.Context, func()) {
						return a.embeddingsScheduler.CooperativeJobContext(ctx, "embeddings build")
					}
				}
				return runEmbedWithOptions(cmd, opts)
			}}
			if rebuild {
				cmd.Flags().BoolVar(&opts.fullRebuild, "full-rebuild", false, "Create a new generation and rebuild from scratch")
				cmd.Flags().BoolVar(&opts.yes, "yes", false, "Skip confirmation prompts")
			}
			cmd.Flags().BoolVar(&opts.backstop, "backstop", false, "Run a full-scan pass")
			cmd.Flags().StringArrayVar(&opts.accounts, "account", nil, "Limit embedding to this account")
			cmd.Flags().StringArrayVar(&opts.collections, "collection", nil, "Limit embedding to this collection")
			return cmd
		}
		group := &cobra.Command{Use: "embeddings"}
		group.AddCommand(newCommand("build", true), newCommand("resume", false))
		root.AddCommand(group, newCommand("build-embeddings", true))
	})
}
