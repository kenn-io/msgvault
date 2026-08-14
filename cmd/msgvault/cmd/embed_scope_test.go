package cmd

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
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

func TestResolveEmbedScopeSourceIDs_EmptyCollectionFailsClosed(t *testing.T) {
	require := require.New(t)
	f, _, _ := setupScopeFixture(t)
	withEmbedScopeGlobals(t, nil)

	_, err := f.Store.CreateCollection("empty", "", []int64{f.Source.ID})
	require.NoError(err, "CreateCollection")
	require.NoError(f.Store.RemoveSourcesFromCollection("empty", []int64{f.Source.ID}), "empty collection")
	embedCollections = []string{"empty"}

	err = resolveEmbedScopeSourceIDs(f.Store)
	require.Error(err, "an explicit empty collection must not widen to the full archive")
	assert.Contains(t, err.Error(), "has no accounts")
	assert.Nil(t, cfg.Vector.Embed.Scope.SourceIDs)
}

// TestResolvedVectorConfigLeavesGlobalUntouched pins the daemon-side
// resolution contract: the returned copy carries the resolved source IDs
// while the shared package-global cfg stays unmutated, so concurrent daemon
// goroutines can resolve without racing.
func TestResolvedVectorConfigLeavesGlobalUntouched(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, accountID, _ := setupScopeFixture(t)
	withEmbedScopeGlobals(t, []string{accountID})

	vecCfg, err := resolvedVectorConfig(f.Store)
	require.NoError(err)
	assert.Equal([]int64{f.Source.ID}, vecCfg.Embed.Scope.SourceIDs,
		"the copy carries the resolved scope")
	assert.Nil(cfg.Vector.Embed.Scope.SourceIDs,
		"the global config must stay unmutated")
}

// TestConfiguredEmbedBuildScope_ClassifiesResolutionFailures pins the
// transient/deterministic split the daemon's drift detection relies on: a
// removed (or never-existing) account is ErrScopeUnresolvable — it cannot
// heal on retry and must latch searches stale — while a resolvable
// configuration re-resolves cleanly.
func TestConfiguredEmbedBuildScope_ClassifiesResolutionFailures(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, accountID, _ := setupScopeFixture(t)

	withEmbedScopeGlobals(t, []string{accountID})
	scope, err := configuredEmbedBuildScope(f.Store)
	require.NoError(err)
	assert.Equal([]int64{f.Source.ID}, scope.SourceIDs)

	cfg.Vector.Embed.Scope.Accounts = []string{"gone@example.com"}
	_, err = configuredEmbedBuildScope(f.Store)
	require.ErrorIs(err, vector.ErrScopeUnresolvable,
		"a configured account that no longer exists is a deterministic failure")
	assert.Contains(err.Error(), "gone@example.com")
}

// TestDurableEmbedScopeRejectsDisplayNames pins the identity rule for the
// privacy boundary: drift detection compares resolved source IDs, and a
// display name is not a stable identity — a recycled source ID plus a
// same-named replacement account would re-resolve identically and silently
// embed the replacement account's text. Durable configuration must name the
// canonical identifier; the one-run --account flag stays permissive.
func TestDurableEmbedScopeRejectsDisplayNames(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, accountID, _ := setupScopeFixture(t)
	_, err := f.Store.DB().Exec(
		f.Store.Rebind(`UPDATE sources SET display_name = 'Work Mail' WHERE id = ?`), f.Source.ID)
	require.NoError(err, "set display name")

	withEmbedScopeGlobals(t, []string{"Work Mail"})
	_, err = configuredEmbedBuildScope(f.Store)
	require.ErrorIs(err, vector.ErrScopeUnresolvable,
		"a display name in durable config must fail closed")
	assert.Contains(err.Error(), accountID, "the error names the canonical identifier to use")

	err = resolveEmbedScopeSourceIDs(f.Store)
	require.Error(err, "startup resolution must reject the display name too")

	// The one-run --account flag still accepts the display name.
	embedAccounts = []string{"Work Mail"}
	require.NoError(resolveEmbedScopeSourceIDs(f.Store))
	assert.Equal([]int64{f.Source.ID}, cfg.Vector.Embed.Scope.SourceIDs)
}

// TestEmbedScopeDriftCheck covers the daemon preflight callback end to end
// against a real store: no drift, drift to a different account, and a
// deterministically unresolvable account all report the right (detail, err)
// so the API latches stale exactly when the scope no longer matches.
func TestEmbedScopeDriftCheck(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, accountID, _ := setupScopeFixture(t)
	withEmbedScopeGlobals(t, []string{accountID})
	initialized := vector.NewBuildScope(nil, []int64{f.Source.ID})
	check := embedScopeDriftCheck(f.Store, initialized)

	detail, err := check(t.Context())
	require.NoError(err)
	assert.Empty(detail, "a matching scope must not latch")

	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err, "GetOrCreateSource")
	cfg.Vector.Embed.Scope.Accounts = []string{other.Identifier}
	detail, err = check(t.Context())
	require.NoError(err)
	assert.Contains(detail, "src-", "a drifted scope latches with both fingerprints")

	cfg.Vector.Embed.Scope.Accounts = []string{"gone@example.com"}
	detail, err = check(t.Context())
	require.NoError(err, "an unresolvable account is drift, not a retryable error")
	assert.Contains(detail, "gone@example.com")
	assert.Contains(detail, "[vector.embed.scope]")
}

func TestResolveEmbedScopeSourceIDs_ConfiguredNumericIDFails(t *testing.T) {
	require := require.New(t)
	f, _, _ := setupScopeFixture(t)
	withEmbedScopeGlobals(t, []string{strconv.FormatInt(f.Source.ID, 10)})

	err := resolveEmbedScopeSourceIDs(f.Store)
	require.Error(err, "durable account configuration must not accept source IDs")
	assert.Nil(t, cfg.Vector.Embed.Scope.SourceIDs)
}
