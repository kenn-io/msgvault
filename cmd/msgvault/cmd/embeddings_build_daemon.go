package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
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
				if state != nil && state.cfg != nil {
					snapshot, err := config.ReadConfigFile(state.cfg.ConfigFilePath())
					if err != nil {
						return fmt.Errorf("read embedding configuration: %w", err)
					}
					if snapshot.Exists {
						saved, err := state.cfg.ReloadConfigFile(snapshot)
						if err != nil {
							return fmt.Errorf("load embedding configuration: %w", err)
						}
						running := *state.cfg
						saved.Vector.DBPath = running.Vector.DBPath
						saved.Vector.Backend = running.Vector.Backend
						saved.Vector.SkipExtensionCreate = running.Vector.SkipExtensionCreate
						running.Vector = saved.Vector
						state.cfg = &running
					}
				}
				if err := validateEmbeddingBuildConfig(invocationFromCommand(cmd)); err != nil {
					return err
				}
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "Waiting for archive operation gate..."); err != nil {
					return fmt.Errorf("report operation gate wait: %w", err)
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
				registerEmbeddingBuildFlags(cmd, &opts.fullRebuild, &opts.yes, &opts.backstop, &opts.accounts, &opts.collections)
			} else {
				registerEmbeddingBuildFlags(cmd, nil, nil, &opts.backstop, &opts.accounts, &opts.collections)
			}
			return cmd
		}
		group := &cobra.Command{Use: "embeddings"}
		group.AddCommand(newCommand("build", true), newCommand("resume", false))
		root.AddCommand(group, newCommand("build-embeddings", true))
	})
}
