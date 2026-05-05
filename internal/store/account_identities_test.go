package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/wesm/msgvault/internal/testutil"
	"github.com/wesm/msgvault/internal/testutil/storetest"
)

func TestAddAndListAccountIdentities(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	if err := st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"); err != nil {
		t.Fatalf("AddAccountIdentity: %v", err)
	}

	ids, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	if len(ids) != 1 {
		t.Fatalf("got %d identities, want 1", len(ids))
	}
	got := ids[0]
	if got.Address != "me@example.com" {
		t.Errorf("address = %q, want me@example.com", got.Address)
	}
	if got.SourceSignal != "manual" {
		t.Errorf("source_signal = %q, want manual", got.SourceSignal)
	}
	if got.SourceID != f.Source.ID {
		t.Errorf("source_id = %d, want %d", got.SourceID, f.Source.ID)
	}
	if got.ConfirmedAt.IsZero() {
		t.Error("confirmed_at should be set after first insert")
	}
}

func TestAddAccountIdentity_Idempotent(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	if err := st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"); err != nil {
		t.Fatalf("AddAccountIdentity (1): %v", err)
	}
	ids1, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities (1)")
	if len(ids1) != 1 {
		t.Fatalf("after first insert: got %d rows, want 1", len(ids1))
	}
	first := ids1[0].ConfirmedAt

	time.Sleep(2 * time.Millisecond)

	if err := st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"); err != nil {
		t.Fatalf("AddAccountIdentity (2): %v", err)
	}
	ids2, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities (2)")
	if len(ids2) != 1 {
		t.Errorf("after idempotent re-add: got %d rows, want 1", len(ids2))
	}
	if !ids2[0].ConfirmedAt.Equal(first) {
		t.Errorf("confirmed_at moved on idempotent re-add: %v -> %v",
			first, ids2[0].ConfirmedAt)
	}
}

// TestAddAccountIdentity_PreservesCase verifies that the first
// add of an email-shaped identifier wins the stored casing. Subsequent
// adds with different cases merge into the same row (case-insensitive
// match) rather than producing duplicate rows. This preserves the
// "case-preserved storage, email-case-insensitive logical identity"
// contract that the add/remove paths share.
func TestAddAccountIdentity_PreservesCase(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	if err := st.AddAccountIdentity(f.Source.ID, "Alice@Example.com", "manual"); err != nil {
		t.Fatalf("AddAccountIdentity Alice: %v", err)
	}
	if err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"); err != nil {
		t.Fatalf("AddAccountIdentity alice: %v", err)
	}

	rows, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	if len(rows) != 1 {
		t.Fatalf("want 1 row (email is case-insensitive), got %d: %+v", len(rows), rows)
	}
	if rows[0].Address != "Alice@Example.com" {
		t.Errorf("address = %q, want first-write 'Alice@Example.com' (case-preserved)",
			rows[0].Address)
	}
}

func TestAddAccountIdentity_AdditionalSignal(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	if err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"); err != nil {
		t.Fatal(err)
	}
	rows1, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	first := rows1[0].ConfirmedAt
	time.Sleep(2 * time.Millisecond)

	if err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", "account-identifier"); err != nil {
		t.Fatal(err)
	}
	rows2, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities after second signal")
	if rows2[0].SourceSignal != "account-identifier,manual" {
		t.Errorf("signal=%q want %q", rows2[0].SourceSignal, "account-identifier,manual")
	}
	if !rows2[0].ConfirmedAt.Equal(first) {
		t.Errorf("confirmed_at moved on signal augment")
	}
}

func TestAddAccountIdentity_ThreeSignalAccumulation(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	for _, sig := range []string{"manual", "account-identifier", "is_from_me"} {
		if err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", sig); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	if rows[0].SourceSignal != "account-identifier,is_from_me,manual" {
		t.Errorf("signal=%q want account-identifier,is_from_me,manual", rows[0].SourceSignal)
	}
}

func TestAddAccountIdentity_EmptySignalOnExistingRow(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	if err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", ""); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	if rows[0].SourceSignal != "manual" {
		t.Errorf("signal=%q want manual (empty signal on existing row is no-op)", rows[0].SourceSignal)
	}
}

func TestAddAccountIdentity_EmptySignalOnMissingRow(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	if err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", ""); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	if len(rows) != 1 || rows[0].SourceSignal != "" {
		t.Fatalf("want one row with empty signal, got %+v", rows)
	}
	if rows[0].ConfirmedAt.IsZero() {
		t.Error("confirmed_at should be set even with empty signal")
	}
}

func TestAddAccountIdentity_NonEmptySignalReplacesEmptyRow(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	if err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	if rows[0].SourceSignal != "manual" {
		t.Errorf("signal=%q want manual", rows[0].SourceSignal)
	}
}

func TestAddAccountIdentity_RejectsCommaInSignal(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", "a,b")
	if err == nil {
		t.Fatal("expected error for comma in signal")
	}
	if !strings.Contains(err.Error(), "comma") {
		t.Errorf("error doesn't mention comma: %v", err)
	}
}

func TestAddAccountIdentity_AllWhitespaceIdentifierIsNoOp(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	if err := st.AddAccountIdentity(f.Source.ID, "   ", "manual"); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	if len(rows) != 0 {
		t.Errorf("whitespace identifier should not insert, got %+v", rows)
	}
}

func TestAccountIdentities_FKCascadeOnSourceDelete(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	if err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveSource(f.Source.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM account_identities WHERE source_id = ?`, f.Source.ID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("FK cascade failed: %d rows remain", n)
	}
}

func TestGetIdentitiesForScope_MultiSource(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	src2, err := st.GetOrCreateSource("gmail", "other@example.com")
	testutil.MustNoErr(t, err, "GetOrCreateSource")

	testutil.MustNoErr(t, st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"), "add alice")
	testutil.MustNoErr(t, st.AddAccountIdentity(src2.ID, "bob@example.com", "manual"), "add bob")

	scope, err := st.GetIdentitiesForScope([]int64{f.Source.ID, src2.ID})
	testutil.MustNoErr(t, err, "GetIdentitiesForScope")

	if len(scope) != 2 {
		t.Fatalf("got %d addresses, want 2", len(scope))
	}
	if _, ok := scope["alice@example.com"]; !ok {
		t.Error("alice@example.com missing from scope")
	}
	if _, ok := scope["bob@example.com"]; !ok {
		t.Error("bob@example.com missing from scope")
	}
}

func TestGetIdentitiesForScope_EmptyInput(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	testutil.MustNoErr(t, st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"), "add identity")

	scope, err := st.GetIdentitiesForScope([]int64{})
	testutil.MustNoErr(t, err, "GetIdentitiesForScope empty")
	if scope == nil {
		t.Error("expected non-nil map for empty scope")
	}
	if len(scope) != 0 {
		t.Errorf("got %d entries, want 0 for empty scope", len(scope))
	}
}

func TestRemoveAccountIdentity_Hit(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	testutil.MustNoErr(t, st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"), "add identity")
	removed, err := st.RemoveAccountIdentity(f.Source.ID, "alice@example.com")
	testutil.MustNoErr(t, err, "RemoveAccountIdentity")
	if removed != 1 {
		t.Errorf("removed=%d, want 1", removed)
	}
	rows, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	if len(rows) != 0 {
		t.Errorf("want empty, got %+v", rows)
	}
}

func TestRemoveAccountIdentity_Miss(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	removed, err := st.RemoveAccountIdentity(f.Source.ID, "nope@example.com")
	testutil.MustNoErr(t, err, "RemoveAccountIdentity")
	if removed != 0 {
		t.Errorf("removed=%d, want 0 on miss", removed)
	}
}

// TestRemoveAccountIdentity_EmailIsCaseInsensitive verifies that an
// email-shaped identifier removed with different casing matches the
// stored row, since email addresses are case-insensitive in practice.
func TestRemoveAccountIdentity_EmailIsCaseInsensitive(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	testutil.MustNoErr(t,
		st.AddAccountIdentity(f.Source.ID, "alice@Example.com", "manual"),
		"add identity")

	removed, err := st.RemoveAccountIdentity(f.Source.ID, "ALICE@example.com")
	testutil.MustNoErr(t, err, "RemoveAccountIdentity")
	if removed != 1 {
		t.Fatalf("removed=%d, want 1 (email match should be case-insensitive)", removed)
	}

	rows, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	if len(rows) != 0 {
		t.Errorf("want empty, got %+v", rows)
	}
}

// TestAddAccountIdentity_EmailIsCaseInsensitive verifies that a second
// add with different casing merges signals into the existing row
// instead of inserting a duplicate. This pairs with
// TestRemoveAccountIdentity_EmailIsCaseInsensitive: add/remove must
// agree on case-folding for "@"-shaped identifiers, otherwise an
// 'identity add Foo@x.com' followed by 'identity remove foo@x.com'
// could leave (or remove) the wrong row.
func TestAddAccountIdentity_EmailIsCaseInsensitive(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	testutil.MustNoErr(t,
		st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"),
		"first add (lowercase)")
	testutil.MustNoErr(t,
		st.AddAccountIdentity(f.Source.ID, "ALICE@Example.com", "is_from_me"),
		"second add (different case)")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 (case-folded merge expected); rows=%+v", len(rows), rows)
	}
	if rows[0].Address != "alice@example.com" {
		t.Errorf("address = %q, want first-write 'alice@example.com'", rows[0].Address)
	}
	if !strings.Contains(rows[0].SourceSignal, "manual") ||
		!strings.Contains(rows[0].SourceSignal, "is_from_me") {
		t.Errorf("source_signal = %q, want both 'manual' and 'is_from_me' merged",
			rows[0].SourceSignal)
	}
}

// TestAddAccountIdentity_NonEmailStaysCaseSensitive guards the
// chat-handle invariant: synthetic identifiers can be case-significant
// so two distinct cases must produce two rows.
func TestAddAccountIdentity_NonEmailStaysCaseSensitive(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	testutil.MustNoErr(t,
		st.AddAccountIdentity(f.Source.ID, "AliceHandle", "manual"),
		"first add")
	testutil.MustNoErr(t,
		st.AddAccountIdentity(f.Source.ID, "alicehandle", "manual"),
		"second add (different case)")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 distinct rows for non-email; rows=%+v", len(rows), rows)
	}
}

// TestAddAccountIdentity_MatrixMXIDStaysCaseSensitive guards against an
// over-broad email heuristic: Matrix MXIDs like "@user:server.org" start
// with "@" and contain a "." but are not emails. Two distinct cases must
// produce two distinct rows.
func TestAddAccountIdentity_MatrixMXIDStaysCaseSensitive(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	testutil.MustNoErr(t,
		st.AddAccountIdentity(f.Source.ID, "@Alice:matrix.org", "manual"),
		"first add (Matrix MXID, mixed case)")
	testutil.MustNoErr(t,
		st.AddAccountIdentity(f.Source.ID, "@alice:matrix.org", "manual"),
		"second add (Matrix MXID, lower case)")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	testutil.MustNoErr(t, err, "ListAccountIdentities")
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 distinct rows for Matrix MXID; rows=%+v", len(rows), rows)
	}
}

// TestRemoveAccountIdentity_NonEmailIsCaseSensitive guards the
// case-preserving path for synthetic identifiers (chat handles, etc.):
// removing with different casing on a non-email value must not match.
func TestRemoveAccountIdentity_NonEmailIsCaseSensitive(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	testutil.MustNoErr(t,
		st.AddAccountIdentity(f.Source.ID, "AliceHandle", "manual"),
		"add identity")

	removed, err := st.RemoveAccountIdentity(f.Source.ID, "alicehandle")
	testutil.MustNoErr(t, err, "RemoveAccountIdentity")
	if removed != 0 {
		t.Fatalf("removed=%d, want 0 on case-mismatch for non-email identifier", removed)
	}
}
