package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

var (
	embedFullRebuild            bool
	embedYes                    bool
	embeddingsRetireYes         bool
	embeddingsRetireForceActive bool
	embeddingsActivateForce     bool
	embeddingsActivateYes       bool
)

var embeddingsCmd = &cobra.Command{
	Use:   "embeddings",
	Short: "Manage vector embeddings",
}

var embeddingsBuildCmd = newEmbeddingsBuildCmd("build")
var embeddingsResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume or top up the current vector embedding generation",
	Long: `Resume or top up the current vector embedding generation.
If a matching generation is building, this drains its pending queue and
activates it when complete. Otherwise it embeds pending rows for the
active generation.`,
	RunE: runEmbeddingsResume,
}
var embeddingsListCmd = &cobra.Command{
	Use:   cmdUseList,
	Short: "List vector embedding generations",
	RunE:  runEmbeddingsList,
}
var embeddingsRetireCmd = &cobra.Command{
	Use:   "retire <generation-id>",
	Short: "Retire a vector embedding generation",
	Args:  cobra.ExactArgs(1),
	RunE:  runEmbeddingsRetire,
}
var embeddingsActivateCmd = &cobra.Command{
	Use:   "activate <generation-id>",
	Short: "Activate a completed vector embedding generation",
	Args:  cobra.ExactArgs(1),
	RunE:  runEmbeddingsActivate,
}
var embedCmd = newEmbeddingsBuildCmd("build-embeddings")

func newEmbeddingsBuildCmd(use string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: "Build or update the vector embedding index (incremental by default; --full-rebuild for a new generation)",
		Long: `Build or update the vector embedding index for hybrid search.
Writes vectors to the co-located vectors.db. In the default incremental
mode, the command drains any pending rows in the active generation. With
--full-rebuild, it creates a new building generation, embeds the entire
corpus, and (on a clean completion) atomically activates it.

Requires [vector] to be enabled in config.toml and [vector.embeddings]
to point at a running OpenAI-compatible endpoint.`,
		RunE: runEmbeddingsBuild,
	}
	cmd.Flags().BoolVar(&embedFullRebuild, "full-rebuild", false, "Create a new generation and rebuild from scratch")
	cmd.Flags().BoolVar(&embedYes, "yes", false, "Skip confirmation prompts")
	return cmd
}

func runEmbeddingsBuild(cmd *cobra.Command, args []string) error {
	if !cfg.Vector.Enabled {
		return errors.New("vector search not enabled; add [vector] enabled=true to config.toml first")
	}
	if cfg.Vector.Embeddings.Endpoint == "" || cfg.Vector.Embeddings.Model == "" {
		return errors.New("[vector.embeddings] endpoint and model are required")
	}
	return runEmbed(cmd)
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

func init() {
	embedCmd.Deprecated = "use 'msgvault embeddings build' instead"
	embeddingsRetireCmd.Flags().BoolVar(&embeddingsRetireYes, "yes", false, "Skip confirmation prompt")
	embeddingsRetireCmd.Flags().BoolVar(&embeddingsRetireForceActive, "force-active", false, "Allow retiring the active generation")
	embeddingsActivateCmd.Flags().BoolVar(&embeddingsActivateYes, "yes", false, "Skip confirmation prompt")
	embeddingsActivateCmd.Flags().BoolVar(&embeddingsActivateForce, "force", false, "Allow activation with pending rows or a fingerprint mismatch")
	embeddingsCmd.AddCommand(embeddingsBuildCmd)
	embeddingsCmd.AddCommand(embeddingsResumeCmd)
	embeddingsCmd.AddCommand(embeddingsListCmd)
	embeddingsCmd.AddCommand(embeddingsRetireCmd)
	embeddingsCmd.AddCommand(embeddingsActivateCmd)
	rootCmd.AddCommand(embeddingsCmd)
	rootCmd.AddCommand(embedCmd)
}
