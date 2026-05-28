package cmd

import (
	"testing"

	requirepkg "github.com/stretchr/testify/require"
)

func TestEmbeddingsBuildCommandRegistration(t *testing.T) {
	buildCmd, _, err := rootCmd.Find([]string{"embeddings", "build"})
	requirepkg.NoError(t, err)
	requirepkg.Equal(t, "build", buildCmd.Name())
	requirepkg.NotNil(t, buildCmd.Flags().Lookup("full-rebuild"))
	requirepkg.NotNil(t, buildCmd.Flags().Lookup("yes"))

	legacyCmd, _, err := rootCmd.Find([]string{"build-embeddings"})
	requirepkg.NoError(t, err)
	requirepkg.Equal(t, "build-embeddings", legacyCmd.Name())
	requirepkg.NotEmpty(t, legacyCmd.Deprecated)
	requirepkg.NotNil(t, legacyCmd.Flags().Lookup("full-rebuild"))
	requirepkg.NotNil(t, legacyCmd.Flags().Lookup("yes"))
}
