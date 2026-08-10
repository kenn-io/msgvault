package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

// TestEmbeddingsBuildAccountFlagsForwardToDaemon proves the real build
// command's --account/--collection flags survive daemonCLIArgsFromCobra, so
// a scope passed to a daemon-fronted `embeddings build` reaches the
// daemon-spawned subprocess intact.
func TestEmbeddingsBuildAccountFlagsForwardToDaemon(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cmd := embeddingsBuildCmd
	oldAccounts, oldCollections := embedAccounts, embedCollections
	t.Cleanup(func() {
		embedAccounts, embedCollections = oldAccounts, oldCollections
		for _, name := range []string{"account", "collection"} {
			cmd.Flags().Lookup(name).Changed = false
		}
	})

	require.NoError(cmd.Flags().Set("account", "alice@example.com"))
	require.NoError(cmd.Flags().Set("account", "bob@example.com"))
	require.NoError(cmd.Flags().Set("collection", "family"))

	got, err := daemonCLIArgsFromCobra(cmd, nil)
	require.NoError(err, "daemon args")
	assert.Equal([]string{
		"embeddings", "build",
		"--account=alice@example.com",
		"--account=bob@example.com",
		"--collection=family",
	}, got)
}

// withEmbedScopeGlobals swaps the config and embed-scope flag globals for a
// resolution test and restores them afterwards.
func withEmbedScopeGlobals(t *testing.T, accounts []string) {
	t.Helper()
	oldCfg := cfg
	oldAccounts, oldCollections := embedAccounts, embedCollections
	c := &config.Config{}
	c.Vector.Embed.Scope.Accounts = accounts
	cfg = c
	embedAccounts, embedCollections = nil, nil
	t.Cleanup(func() {
		cfg = oldCfg
		embedAccounts, embedCollections = oldAccounts, oldCollections
	})
}

func TestResolveEmbedScopeSourceIDs_ConfiguredAccounts(t *testing.T) {
	f, accountID, _ := setupScopeFixture(t)
	withEmbedScopeGlobals(t, []string{accountID})

	require.NoError(t, resolveEmbedScopeSourceIDs(f.Store))
	assert.Equal(t, []int64{f.Source.ID}, cfg.Vector.Embed.Scope.SourceIDs,
		"configured account resolves to its source ID")
}

func TestResolveEmbedScopeSourceIDs_UnknownAccountFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, _, _ := setupScopeFixture(t)
	withEmbedScopeGlobals(t, []string{"nobody@example.com"})

	err := resolveEmbedScopeSourceIDs(f.Store)
	require.Error(err, "unknown configured account must fail loudly")
	assert.Contains(err.Error(), "[vector.embed.scope] accounts")
	assert.Contains(err.Error(), "nobody@example.com")
	assert.Nil(cfg.Vector.Embed.Scope.SourceIDs, "failed resolution leaves SourceIDs unset")
}

func TestResolveEmbedScopeSourceIDs_NoScopeLeavesCorpusWide(t *testing.T) {
	f, _, _ := setupScopeFixture(t)
	withEmbedScopeGlobals(t, nil)

	require.NoError(t, resolveEmbedScopeSourceIDs(f.Store))
	assert.Nil(t, cfg.Vector.Embed.Scope.SourceIDs)
}

func TestResolveEmbedScopeSourceIDs_AccountFlagOverridesConfig(t *testing.T) {
	require := require.New(t)
	f, accountID, _ := setupScopeFixture(t)
	withEmbedScopeGlobals(t, []string{accountID})

	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err, "GetOrCreateSource")
	embedAccounts = []string{other.Identifier}

	require.NoError(resolveEmbedScopeSourceIDs(f.Store))
	assert.Equal(t, []int64{other.ID}, cfg.Vector.Embed.Scope.SourceIDs,
		"--account replaces the configured accounts for the run")
}

func TestResolveEmbedScopeSourceIDs_CollectionFlagExpands(t *testing.T) {
	require := require.New(t)
	f, _, collectionName := setupScopeFixture(t)
	withEmbedScopeGlobals(t, nil)

	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err, "GetOrCreateSource")
	_, err = f.Store.CreateCollection("both", "", []int64{f.Source.ID, other.ID})
	require.NoError(err, "CreateCollection both")
	// The fixture collection covers only f.Source; "both" covers both.
	embedCollections = []string{collectionName, "both"}

	require.NoError(resolveEmbedScopeSourceIDs(f.Store))
	assert.ElementsMatch(t, []int64{f.Source.ID, other.ID}, cfg.Vector.Embed.Scope.SourceIDs,
		"--collection expands to the union of member sources")
}
