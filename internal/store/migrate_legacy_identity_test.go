package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSourceTypeUsesEmailIdentity(t *testing.T) {
	tests := []struct {
		sourceType string
		want       bool
	}{
		// Email archives: identity column holds email addresses.
		{"gmail", true},
		{"imap", true},
		{"o365", true},
		{"mbox", true},
		{"hey", true},
		{"apple-mail", true},
		{"pst", true},
		// Phone/handle-keyed sources.
		{"whatsapp", false},
		{"apple_messages", false},
		{"synctech_sms", false},
		{"google_voice", false},
		{"beeper", false},
		{"discord", false},
		{"facebook_messenger", false},
		// Email-keyed chat/meeting sources confirm their own identifier
		// at add time and are excluded from the legacy email migration.
		{"teams", false},
		{"gcal", false},
		{"granola", false},
		{"circleback", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.sourceType, func(t *testing.T) {
			assert.Equal(t, tt.want, store.SourceTypeUsesEmailIdentity(tt.sourceType))
		})
	}
}

func TestMigrateLegacyIdentityConfig_Basic(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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

// TestMigrateLegacyIdentityConfig_PstIncludedPhoneSkipped verifies that
// a default PST import source ("pst") receives migrated legacy email
// addresses while a phone-keyed source is skipped. Regression test:
// "pst" was missing from SourceTypeUsesEmailIdentity, so normal PST
// imports were bypassed by the legacy identity migration.
func TestMigrateLegacyIdentityConfig_PstIncludedPhoneSkipped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	pstSrc, err := st.GetOrCreateSource("pst", "archive@example.com")
	require.NoError(err, "create pst source")
	phoneSrc, err := st.GetOrCreateSource("whatsapp", "+15550001111")
	require.NoError(err, "create whatsapp source")

	applied, deferred, sources, addrs, err := st.MigrateLegacyIdentityConfig([]string{"alice@example.com"})
	require.NoError(err, "MigrateLegacyIdentityConfig")

	assert.True(applied, "applied")
	assert.False(deferred, "deferred")
	assert.Equal(2, sources, "eligible sources: default gmail fixture + pst")
	assert.Equal(1, addrs, "addrs")

	pstIDs, err := st.ListAccountIdentities(pstSrc.ID)
	require.NoError(err, "ListAccountIdentities pst")
	require.Len(pstIDs, 1, "pst source should receive the legacy address")
	assert.Equal("alice@example.com", pstIDs[0].Address, "pst address")
	assert.Equal("config_migration", pstIDs[0].SourceSignal, "pst source_signal")

	phoneIDs, err := st.ListAccountIdentities(phoneSrc.ID)
	require.NoError(err, "ListAccountIdentities whatsapp")
	assert.Empty(phoneIDs, "phone-keyed source must not receive email identities")
}

// TestMigrateLegacyIdentityConfig_BumpsRevisionsOnlyWhenItInserts verifies
// that the legacy-config startup migration bumps both the identity revision
// and the account-identity revision when it actually inserts confirmed
// account_identities rows (so a daemon that started before the migration ran
// invalidates its cached owner_participants/is_from_me), and that the
// idempotent no-op re-run leaves both revisions unchanged.
func TestMigrateLegacyIdentityConfig_BumpsRevisionsOnlyWhenItInserts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	identityRevBefore, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision before migration")
	acctRevBefore, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision before migration")

	applied, _, _, _, err := st.MigrateLegacyIdentityConfig([]string{"alice@example.com"}) //nolint:dogsled // 5-return migration; test needs only applied+err
	require.NoError(err, "MigrateLegacyIdentityConfig")
	require.True(applied, "applied should be true on first run")

	identityRevAfter, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision after migration")
	assert.Equal(identityRevBefore+1, identityRevAfter,
		"an actual insert must bump the identity revision")
	acctRevAfter, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after migration")
	assert.Equal(acctRevBefore+1, acctRevAfter,
		"an actual insert must bump the account identity revision")

	// Re-running is a no-op (already applied): neither revision should move.
	_, _, _, _, err = st.MigrateLegacyIdentityConfig([]string{"alice@example.com"}) //nolint:dogsled // 5-return migration; test needs only err
	require.NoError(err, "second MigrateLegacyIdentityConfig call")

	identityRevSecond, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision after second call")
	assert.Equal(identityRevAfter, identityRevSecond,
		"a no-op re-run must not bump the identity revision")
	acctRevSecond, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after second call")
	assert.Equal(acctRevAfter, acctRevSecond,
		"a no-op re-run must not bump the account identity revision")
}

func TestMigrateLegacyIdentityConfig_MergesExistingSignal(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "account-identifier"), "AddAccountIdentity")

	applied, _, _, _, err := st.MigrateLegacyIdentityConfig([]string{"alice@example.com"}) //nolint:dogsled // 5-return migration; test needs only applied+err
	require.NoError(err, "MigrateLegacyIdentityConfig")
	require.True(applied, "applied should be true on first run")

	ids, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(ids, 1)
	assert.Equal(t, "account-identifier,config_migration", ids[0].SourceSignal, "source_signal")
}

func TestMigrateLegacyIdentityConfig_SecondCallNoOp(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require := require.New(t)
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
	require := require.New(t)
	assert := assert.New(t)
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
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	_, _, _, _, err := st.MigrateLegacyIdentityConfig([]string{"  ME@Example.COM  "}) //nolint:dogsled // 5-return migration; test needs only err
	require.NoError(err, "MigrateLegacyIdentityConfig")

	ids, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(ids, 1)
	assert.Equal(t, "ME@Example.COM", ids[0].Address, "address")
}

func TestMigrateLegacyIdentityConfig_PreservesCase(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	applied, _, _, _, err := st.MigrateLegacyIdentityConfig([]string{"Alice@Example.com"}) //nolint:dogsled // 5-return migration; test needs only applied+err
	require.NoError(err, "MigrateLegacyIdentityConfig")
	require.True(applied, "expected applied=true on first run")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 1)
	assert.Equal(t, "Alice@Example.com", rows[0].Address, "address")
}

// TestMigrateLegacyIdentityConfig_DedupesEmailCaseVariants verifies that
// the migration's input-list dedupe applies the same case-aware rule as
// the rest of the identity subsystem. Email-shaped variants like
// `Alice@Example.com` and `alice@example.com` should collapse to a single
// row per source. Synthetic identifiers (Matrix MXIDs, chat handles)
// remain case-sensitive and are NOT collapsed by dedupe.
func TestMigrateLegacyIdentityConfig_DedupesEmailCaseVariants(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
