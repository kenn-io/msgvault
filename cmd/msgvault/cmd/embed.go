package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

var (
	embedFullRebuild            bool
	embedYes                    bool
	embedBackstop               bool
	embeddingsRetireYes         bool
	embeddingsRetireForceActive bool
	embeddingsActivateForce     bool
	embeddingsActivateYes       bool
)

const embeddingsCommandName = "embeddings"

var embeddingsCmd = &cobra.Command{
	Use:   embeddingsCommandName,
	Short: "Manage vector embeddings",
}

var embeddingsBuildCmd = newEmbeddingsBuildCmd("build")
var embeddingsResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume or top up the current vector embedding generation",
	Long: `Resume or top up the current vector embedding generation.
If a matching generation is building, this embeds any messages still
needing embedding for it and activates it when complete. Otherwise it
embeds any messages still needing embedding for the active generation.
Pass --backstop for a full-scan pass that ignores the per-generation
watermark, catching any straggler messages the incremental scan skipped.`,
	RunE: runEmbeddingsResume,
}
var embeddingsListCmd = &cobra.Command{
	Use:   cmdUseList,
	Short: "List vector embedding generations",
	RunE:  runEmbeddingsListCommand,
}
var embeddingsRetireCmd = &cobra.Command{
	Use:   "retire <generation-id>",
	Short: "Retire a vector embedding generation",
	Args:  cobra.ExactArgs(1),
	RunE:  runEmbeddingsRetireCommand,
}
var embeddingsActivateCmd = &cobra.Command{
	Use:   "activate <generation-id>",
	Short: "Activate a completed vector embedding generation",
	Args:  cobra.ExactArgs(1),
	RunE:  runEmbeddingsActivateCommand,
}
var embedCmd = newEmbeddingsBuildCmd("build-embeddings")

func newEmbeddingsBuildCmd(use string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: "Build or update the vector embedding index (incremental by default; --full-rebuild for a new generation)",
		Long: `Build or update the vector embedding index for hybrid search.
Writes vectors to the co-located vectors.db. In the default incremental
mode, the command embeds any messages still needing embedding for the
active generation. With --full-rebuild, it creates a new building
generation, embeds the entire corpus, and (on a clean completion)
atomically activates it.

Requires [vector] to be enabled in config.toml and [vector.embeddings]
to point at a running OpenAI-compatible endpoint.`,
		RunE: runEmbeddingsBuild,
	}
	cmd.Flags().BoolVar(&embedFullRebuild, "full-rebuild", false, "Create a new generation and rebuild from scratch")
	cmd.Flags().BoolVar(&embedYes, "yes", false, "Skip confirmation prompts")
	cmd.Flags().BoolVar(&embedBackstop, "backstop", false,
		"Full-scan pass that ignores the per-generation watermark, catching any straggler messages the incremental scan skipped (idempotent)")
	return cmd
}

func runEmbeddingsBuild(cmd *cobra.Command, args []string) error {
	if !isDaemonCLISubprocess() {
		return runEmbeddingsBuildHTTP(cmd, args)
	}
	return runEmbeddingsBuildLocal(cmd)
}

func runEmbeddingsBuildLocal(cmd *cobra.Command) error {
	if !cfg.Vector.Enabled {
		return errors.New("vector search not enabled; add [vector] enabled=true to config.toml first")
	}
	if cfg.Vector.Embeddings.Endpoint == "" || cfg.Vector.Embeddings.Model == "" {
		return errors.New("[vector.embeddings] endpoint and model are required")
	}
	return runEmbed(cmd)
}

func runEmbeddingsBuildHTTP(cmd *cobra.Command, args []string) error {
	if embedFullRebuild && !embedYes {
		if !confirmEmbed(cmd, "Start a full rebuild? This builds a new generation and atomically swaps it in when complete. ") {
			return errors.New("aborted")
		}
		if err := cmd.Flags().Set("yes", "true"); err != nil {
			return fmt.Errorf("set --yes after confirmation: %w", err)
		}
	}
	return runDaemonCLICommandHTTPFromCobra(cmd, args)
}

func runEmbeddingsResume(cmd *cobra.Command, args []string) error {
	oldFullRebuild := embedFullRebuild
	oldYes := embedYes
	embedFullRebuild = false
	embedYes = false
	defer func() {
		embedFullRebuild = oldFullRebuild
		embedYes = oldYes
	}()
	return runEmbeddingsBuild(cmd, args)
}

func runEmbeddingsListCommand(cmd *cobra.Command, args []string) error {
	if !isDaemonCLISubprocess() {
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
	}
	return runEmbeddingsList(cmd, args)
}

func init() {
	embedCmd.Deprecated = "use 'msgvault embeddings build' instead"
	embeddingsResumeCmd.Flags().BoolVar(&embedBackstop, "backstop", false,
		"Full-scan pass that ignores the per-generation watermark, catching any straggler messages the incremental scan skipped (idempotent)")
	embeddingsRetireCmd.Flags().BoolVar(&embeddingsRetireYes, "yes", false, "Skip confirmation prompt")
	embeddingsRetireCmd.Flags().BoolVar(&embeddingsRetireForceActive, "force-active", false, "Allow retiring the active generation")
	embeddingsActivateCmd.Flags().BoolVar(&embeddingsActivateYes, "yes", false, "Skip confirmation prompt")
	embeddingsActivateCmd.Flags().BoolVar(&embeddingsActivateForce, "force", false, "Allow activation while messages still need embedding, or with a fingerprint mismatch")
	embeddingsCmd.AddCommand(embeddingsBuildCmd)
	embeddingsCmd.AddCommand(embeddingsResumeCmd)
	embeddingsCmd.AddCommand(embeddingsListCmd)
	embeddingsCmd.AddCommand(embeddingsRetireCmd)
	embeddingsCmd.AddCommand(embeddingsActivateCmd)
	rootCmd.AddCommand(embeddingsCmd)
	rootCmd.AddCommand(embedCmd)
}
