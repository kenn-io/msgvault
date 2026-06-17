package store_test

import (
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAddAndListAccountIdentities(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"), "AddAccountIdentity")

	ids, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(ids, 1)
	got := ids[0]
	assert.Equal("me@example.com", got.Address, "address")
	assert.Equal("manual", got.SourceSignal, "source_signal")
	assert.Equal(f.Source.ID, got.SourceID, "source_id")
	assert.False(got.ConfirmedAt.IsZero(), "confirmed_at should be set after first insert")
}

func TestAddAccountIdentity_Idempotent(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"), "AddAccountIdentity (1)")
	ids1, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities (1)")
	require.Len(ids1, 1, "after first insert")
	first := ids1[0].ConfirmedAt

	time.Sleep(2 * time.Millisecond)

	require.NoError(st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"), "AddAccountIdentity (2)")
	ids2, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities (2)")
	assert.Len(ids2, 1, "after idempotent re-add")
	assert.True(ids2[0].ConfirmedAt.Equal(first),
		"confirmed_at moved on idempotent re-add: %v -> %v", first, ids2[0].ConfirmedAt)
}

// TestAddAccountIdentity_PreservesCase verifies that the first
// add of an email-shaped identifier wins the stored casing. Subsequent
// adds with different cases merge into the same row (case-insensitive
// match) rather than producing duplicate rows. This preserves the
// "case-preserved storage, email-case-insensitive logical identity"
// contract that the add/remove paths share.
func TestAddAccountIdentity_PreservesCase(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "Alice@Example.com", "manual"), "AddAccountIdentity Alice")
	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"), "AddAccountIdentity alice")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 1, "want 1 row (email is case-insensitive)")
	assertpkg.Equal(t, "Alice@Example.com", rows[0].Address,
		"address (case-preserved first-write)")
}

func TestAddAccountIdentity_AdditionalSignal(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	rows1, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	first := rows1[0].ConfirmedAt
	time.Sleep(2 * time.Millisecond)

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "account-identifier"))
	rows2, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities after second signal")
	assert.Equal("account-identifier,manual", rows2[0].SourceSignal, "signal")
	assert.True(rows2[0].ConfirmedAt.Equal(first), "confirmed_at moved on signal augment")
}

func TestAddAccountIdentity_ThreeSignalAccumulation(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	for _, sig := range []string{"manual", "account-identifier", "is_from_me"} {
		requirepkg.NoError(t, st.AddAccountIdentity(f.Source.ID, "alice@example.com", sig))
	}
	rows, err := st.ListAccountIdentities(f.Source.ID)
	requirepkg.NoError(t, err, "ListAccountIdentities")
	assertpkg.Equal(t, "account-identifier,is_from_me,manual", rows[0].SourceSignal, "signal")
}

func TestAddAccountIdentity_EmptySignalOnExistingRow(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", ""))
	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	assertpkg.Equal(t, "manual", rows[0].SourceSignal,
		"signal (empty signal on existing row should be no-op)")
}

func TestAddAccountIdentity_EmptySignalOnMissingRow(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", ""))
	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 1)
	require.Empty(rows[0].SourceSignal, "want one row with empty signal")
	assertpkg.False(t, rows[0].ConfirmedAt.IsZero(), "confirmed_at should be set even with empty signal")
}

func TestAddAccountIdentity_NonEmptySignalReplacesEmptyRow(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", ""))
	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	assertpkg.Equal(t, "manual", rows[0].SourceSignal, "signal")
}

func TestAddAccountIdentity_RejectsCommaInSignal(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", "a,b")
	requirepkg.Error(t, err, "expected error for comma in signal")
	assertpkg.ErrorContains(t, err, "comma")
}

func TestAddAccountIdentity_AllWhitespaceIdentifierIsNoOp(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	requirepkg.NoError(t, st.AddAccountIdentity(f.Source.ID, "   ", "manual"))
	rows, err := st.ListAccountIdentities(f.Source.ID)
	requirepkg.NoError(t, err, "ListAccountIdentities")
	assertpkg.Empty(t, rows, "whitespace identifier should not insert")
}

func TestAccountIdentities_FKCascadeOnSourceDelete(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	require.NoError(st.RemoveSource(f.Source.ID))
	var n int
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM account_identities WHERE source_id = ?`), f.Source.ID,
	).Scan(&n))
	assertpkg.Equal(t, 0, n, "FK cascade failed: %d rows remain", n)
}

func TestGetIdentitiesForScope_MultiSource(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store

	src2, err := st.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err, "GetOrCreateSource")

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"), "add alice")
	require.NoError(st.AddAccountIdentity(src2.ID, "bob@example.com", "manual"), "add bob")

	scope, err := st.GetIdentitiesForScope([]int64{f.Source.ID, src2.ID})
	require.NoError(err, "GetIdentitiesForScope")

	require.Len(scope, 2)
	assert.Contains(scope, "alice@example.com")
	assert.Contains(scope, "bob@example.com")
}

func TestGetIdentitiesForScope_EmptyInput(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"), "add identity")

	scope, err := st.GetIdentitiesForScope([]int64{})
	require.NoError(err, "GetIdentitiesForScope empty")
	assert.NotNil(scope, "expected non-nil map for empty scope")
	assert.Empty(scope, "want empty scope")
}

func TestRemoveAccountIdentity_Hit(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"), "add identity")
	removed, err := st.RemoveAccountIdentity(f.Source.ID, "alice@example.com")
	require.NoError(err, "RemoveAccountIdentity")
	assert.Equal(int64(1), removed, "removed")
	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	assert.Empty(rows)
}

func TestRemoveAccountIdentity_Miss(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	removed, err := st.RemoveAccountIdentity(f.Source.ID, "nope@example.com")
	requirepkg.NoError(t, err, "RemoveAccountIdentity")
	assertpkg.Equal(t, int64(0), removed, "removed on miss")
}

// TestRemoveAccountIdentity_EmailIsCaseInsensitive verifies that an
// email-shaped identifier removed with different casing matches the
// stored row, since email addresses are case-insensitive in practice.
func TestRemoveAccountIdentity_EmailIsCaseInsensitive(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@Example.com", "manual"),
		"add identity")

	removed, err := st.RemoveAccountIdentity(f.Source.ID, "ALICE@example.com")
	require.NoError(err, "RemoveAccountIdentity")
	require.Equal(int64(1), removed, "removed (email match should be case-insensitive)")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	assertpkg.Empty(t, rows)
}

// TestAddAccountIdentity_EmailIsCaseInsensitive verifies that a second
// add with different casing merges signals into the existing row
// instead of inserting a duplicate. This pairs with
// TestRemoveAccountIdentity_EmailIsCaseInsensitive: add/remove must
// agree on case-folding for "@"-shaped identifiers, otherwise an
// 'identity add Foo@x.com' followed by 'identity remove foo@x.com'
// could leave (or remove) the wrong row.
func TestAddAccountIdentity_EmailIsCaseInsensitive(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"),
		"first add (lowercase)")
	require.NoError(st.AddAccountIdentity(f.Source.ID, "ALICE@Example.com", "is_from_me"),
		"second add (different case)")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 1, "want case-folded merge")
	assert.Equal("alice@example.com", rows[0].Address, "first-write")
	assert.Contains(rows[0].SourceSignal, "manual", "source_signal merged")
	assert.Contains(rows[0].SourceSignal, "is_from_me", "source_signal merged")
}

// TestAddAccountIdentity_NonEmailStaysCaseSensitive guards the
// chat-handle invariant: synthetic identifiers can be case-significant
// so two distinct cases must produce two rows.
func TestAddAccountIdentity_NonEmailStaysCaseSensitive(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "AliceHandle", "manual"),
		"first add")
	require.NoError(st.AddAccountIdentity(f.Source.ID, "alicehandle", "manual"),
		"second add (different case)")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 2, "want 2 distinct rows for non-email")
}

// TestAddAccountIdentity_MatrixMXIDStaysCaseSensitive guards against an
// over-broad email heuristic: Matrix MXIDs like "@user:server.org" start
// with "@" and contain a "." but are not emails. Two distinct cases must
// produce two distinct rows.
func TestAddAccountIdentity_MatrixMXIDStaysCaseSensitive(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "@Alice:matrix.org", "manual"),
		"first add (Matrix MXID, mixed case)")
	require.NoError(st.AddAccountIdentity(f.Source.ID, "@alice:matrix.org", "manual"),
		"second add (Matrix MXID, lower case)")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 2, "want 2 distinct rows for Matrix MXID")
}

// TestRemoveAccountIdentity_NonEmailIsCaseSensitive guards the
// case-preserving path for synthetic identifiers (chat handles, etc.):
// removing with different casing on a non-email value must not match.
func TestRemoveAccountIdentity_NonEmailIsCaseSensitive(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	requirepkg.NoError(t,
		st.AddAccountIdentity(f.Source.ID, "AliceHandle", "manual"),
		"add identity")

	removed, err := st.RemoveAccountIdentity(f.Source.ID, "alicehandle")
	requirepkg.NoError(t, err, "RemoveAccountIdentity")
	requirepkg.Equal(t, int64(0), removed, "removed on case-mismatch for non-email identifier")
}
