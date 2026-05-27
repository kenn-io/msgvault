package store_test

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMigrateLegacyIdentityConfig_Basic(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store

	src2, err := st.GetOrCreateSource("gmail", "second@example.com")
	require.NoError(err, "GetOrCreateSource")

	addresses := []string{"alice@example.com", "alice@work.com", "shared@example.com"}

	applied, deferred, sources, addrs, err := st.MigrateLegacyIdentityConfig(addresses)
	require.NoError(err, "MigrateLegacyIdentityConfig")

	assert.True(applied, "applied should be true on first run")
	assert.False(deferred, "deferred should be false when sources exist")
	assert.Equal(2, sources, "sources")
	assert.Equal(3, addrs, "addrs")

	// Verify rows: 2 sources × 3 addresses = 6 rows total.
	for _, srcID := range []int64{f.Source.ID, src2.ID} {
		ids, listErr := st.ListAccountIdentities(srcID)
		require.NoError(listErr, "ListAccountIdentities")
		assert.Len(ids, 3, "source %d", srcID)
		for _, id := range ids {
			assert.Equal("config_migration", id.SourceSignal, "source_signal")
		}
	}
}

func TestMigrateLegacyIdentityConfig_MergesExistingSignal(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "account-identifier"), "AddAccountIdentity")

	applied, _, _, _, err := st.MigrateLegacyIdentityConfig([]string{"alice@example.com"}) //nolint:dogsled // 5-return migration; test needs only applied+err
	require.NoError(err, "MigrateLegacyIdentityConfig")
	require.True(applied, "applied should be true on first run")

	ids, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(ids, 1)
	assertpkg.Equal(t, "account-identifier,config_migration", ids[0].SourceSignal, "source_signal")
}

func TestMigrateLegacyIdentityConfig_SecondCallNoOp(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store

	addresses := []string{"alice@example.com"}

	_, _, _, _, err := st.MigrateLegacyIdentityConfig(addresses) //nolint:dogsled // 5-return migration; test needs only err
	require.NoError(err, "first migration")

	applied, _, sources, addrs, err := st.MigrateLegacyIdentityConfig(addresses)
	require.NoError(err, "second migration")

	assert.False(applied, "applied should be false on second call")
	assert.Equal(0, sources, "second call sources")
	assert.Equal(0, addrs, "second call addrs")
}

func TestMigrateLegacyIdentityConfig_DeferredUntilSourceExists(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)

	applied, deferred, sources, addrs, err := st.MigrateLegacyIdentityConfig([]string{"alice@example.com"})
	require.NoError(err, "first migration")
	require.False(applied, "applied should be false before any sources exist")
	require.True(deferred, "deferred should be true when addresses exist but no sources")
	// On the deferred path we report the post-normalization address
	// count so the user-facing notice doesn't overstate (raw input may
	// include blanks/dupes). Sources is still 0 because nothing was
	// written.
	require.Equal(0, sources, "deferred sources")
	require.Equal(1, addrs, "deferred addrs")

	_, err = st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")

	applied, deferred, sources, addrs, err = st.MigrateLegacyIdentityConfig([]string{"alice@example.com"})
	require.NoError(err, "second migration")
	require.True(applied, "applied should be true after a source exists")
	require.False(deferred, "deferred should be false once a source exists")
	require.Equal(1, sources)
	require.Equal(1, addrs)
}

func TestMigrateLegacyIdentityConfig_EmptyAddresses(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store

	applied, _, sources, addrs, err := st.MigrateLegacyIdentityConfig(nil)
	require.NoError(err, "MigrateLegacyIdentityConfig empty")

	assert.False(applied, "applied should be false for empty address list")
	assert.Equal(0, sources)
	assert.Equal(0, addrs)

	// Migration should be marked so it won't re-run.
	wasMigrated, err := st.IsMigrationApplied("legacy_identity_to_per_account")
	require.NoError(err, "IsMigrationApplied")
	assert.True(wasMigrated, "migration sentinel should be set even for empty address list")
}

func TestMigrateLegacyIdentityConfig_TrimsWhitespace(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	st := f.Store

	_, _, _, _, err := st.MigrateLegacyIdentityConfig([]string{"  ME@Example.COM  "}) //nolint:dogsled // 5-return migration; test needs only err
	require.NoError(err, "MigrateLegacyIdentityConfig")

	ids, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(ids, 1)
	assertpkg.Equal(t, "ME@Example.COM", ids[0].Address, "address")
}

func TestMigrateLegacyIdentityConfig_PreservesCase(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	st := f.Store

	applied, _, _, _, err := st.MigrateLegacyIdentityConfig([]string{"Alice@Example.com"}) //nolint:dogsled // 5-return migration; test needs only applied+err
	require.NoError(err, "MigrateLegacyIdentityConfig")
	require.True(applied, "expected applied=true on first run")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 1)
	assertpkg.Equal(t, "Alice@Example.com", rows[0].Address, "address")
}

// TestMigrateLegacyIdentityConfig_DedupesEmailCaseVariants verifies that
// the migration's input-list dedupe applies the same case-aware rule as
// the rest of the identity subsystem. Email-shaped variants like
// `Alice@Example.com` and `alice@example.com` should collapse to a single
// row per source. Synthetic identifiers (Matrix MXIDs, chat handles)
// remain case-sensitive and are NOT collapsed by dedupe.
func TestMigrateLegacyIdentityConfig_DedupesEmailCaseVariants(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store

	// Email variants: should dedupe to one row, preserving first-seen case.
	// Synthetic identifier variants: should NOT dedupe — they're stored
	// case-sensitively in the rest of the system.
	addresses := []string{
		"Alice@Example.com",
		"alice@example.com",
		"ALICE@EXAMPLE.COM",
		"@user:matrix.org",
		"@User:matrix.org",
	}

	applied, _, _, addrs, err := st.MigrateLegacyIdentityConfig(addresses)
	require.NoError(err, "MigrateLegacyIdentityConfig")
	require.True(applied, "expected applied=true on first run")
	// Want: 1 email (first-seen), 2 distinct MXIDs.
	assert.Equal(3, addrs, "addrs (1 email collapse + 2 distinct MXIDs)")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 3)
	got := make(map[string]bool, len(rows))
	for _, r := range rows {
		got[r.Address] = true
	}
	for _, want := range []string{"Alice@Example.com", "@user:matrix.org", "@User:matrix.org"} {
		assert.True(got[want], "missing identity %q (have %v)", want, got)
	}
}
