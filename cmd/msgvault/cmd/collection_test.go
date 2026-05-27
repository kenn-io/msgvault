package cmd

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestCollectionShowPrintsReadableSourceNames(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	tmpDir := t.TempDir()
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	dbPath := filepath.Join(tmpDir, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "init schema")

	alice, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create alice source")
	require.NoError(st.UpdateSourceDisplayName(alice.ID, "Personal"), "set display name")
	bob, err := st.GetOrCreateSource("imap", "bob@example.com")
	require.NoError(err, "create bob source")
	_, err = st.CreateCollection("team", "", []int64{alice.ID, bob.ID})
	require.NoError(err, "create collection")
	require.NoError(st.Close(), "close setup store")

	done := captureStdout(t)
	require.NoError(runCollectionShow(&cobra.Command{}, []string{"team"}), "runCollectionShow")
	out := done()

	assert.Contains(out, "Personal (id ", "missing display name in output")
	assert.Contains(out, "bob@example.com (id ", "missing identifier in output")
}

func TestResolveAccountListRejectsMissingNumericID(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	st, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = st.Close() }()
	require.NoError(st.InitSchema(), "init schema")

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source")

	ids, err := resolveAccountList(nil, st, strconv.FormatInt(src.ID, 10))
	require.NoError(err, "resolveAccountList(existing id)")
	require.Equal([]int64{src.ID}, ids, "resolveAccountList(existing id)")

	// "999999" is neither an existing source ID nor an existing
	// identifier/display name, so resolveAccountList errors via the
	// final ResolveAccountFlag fall-through. Iter12 codex flagged that
	// the prior shape errored *before* the fall-through, so a numeric
	// identifier (e.g. unprefixed phone "15551234567") that wasn't a
	// source ID would never get a chance to match by identifier. The
	// test below asserts the fall-through path is reachable.
	_, err = resolveAccountList(nil, st, "999999")
	require.Error(err, "expected error for missing numeric source ID")
}

// TestResolveAccountListNumericFallthroughResolvesIdentifier verifies
// that a plain-digit token that does NOT match a source ID falls
// through to identifier resolution. Regression test for iter12 codex
// Low: previously, a numeric identifier (e.g. an unprefixed phone
// number) that happened to not match a source ID would error
// immediately instead of being looked up by identifier.
func TestResolveAccountListNumericFallthroughResolvesIdentifier(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	st, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = st.Close() }()
	require.NoError(st.InitSchema(), "init schema")

	// Create a source with a numeric identifier that is unlikely to
	// collide with the auto-assigned source ID. Use a 12-digit string
	// (way past any plausible primary-key value) so the test stays
	// stable.
	phoneIdentifier := "987654321098"
	src, err := st.GetOrCreateSource("whatsapp", phoneIdentifier)
	require.NoError(err, "create source")
	require.NotEqual(phoneIdentifier, strconv.FormatInt(src.ID, 10),
		"test assumption broken: source id %d collides with identifier", src.ID)

	ids, err := resolveAccountList(nil, st, phoneIdentifier)
	require.NoError(err, "resolveAccountList(numeric identifier)")
	require.Equal([]int64{src.ID}, ids, "resolveAccountList(numeric identifier)")
}
