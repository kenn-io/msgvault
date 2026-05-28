package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

var (
	embedFullRebuild bool
	embedYes         bool
)

var embeddingsCmd = &cobra.Command{
	Use:   "embeddings",
	Short: "Manage vector embeddings",
}

var embeddingsBuildCmd = newEmbeddingsBuildCmd("build")
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
	return runEmbed(cmd.Context())
}

func init() {
	embedCmd.Deprecated = "use 'msgvault embeddings build' instead"
	embeddingsCmd.AddCommand(embeddingsBuildCmd)
	rootCmd.AddCommand(embeddingsCmd)
	rootCmd.AddCommand(embedCmd)
}
