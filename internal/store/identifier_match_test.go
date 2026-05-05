package store

import "testing"

// TestNormalizeIdentifierForCompare locks down the identity-map
// canonicalization rule used by the dedup engine's per-source
// identity lookup. Email-shaped tokens lowercase; everything else
// passes through. Calling it on both sides of a map insertion and
// lookup gives the same case-aware semantics as EqualIdentifier
// without paying for pairwise comparison on the hot path.
func TestNormalizeIdentifierForCompare(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"email_lower", "foo@x.com", "foo@x.com"},
		{"email_mixed", "Foo@X.COM", "foo@x.com"},
		{"matrix_mxid_preserves_case", "@Alice:matrix.org", "@Alice:matrix.org"},
		{"handle_preserves_case", "AliceHandle", "AliceHandle"},
		{"phone_preserves", "+15551234567", "+15551234567"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeIdentifierForCompare(tc.in); got != tc.want {
				t.Errorf("NormalizeIdentifierForCompare(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEqualIdentifier asserts that the in-memory comparison rule
// matches the SQL-side LOWER() rule encoded by identifierMatch:
// email-shaped tokens compare case-insensitively, everything else
// compares case-sensitively. The CLI uses this to look up prior
// rows in already-loaded identity slices before calling
// AddAccountIdentity, which is what surfaces the "already confirmed"
// UX message correctly when the user re-supplies an email with
// different casing.
func TestEqualIdentifier(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"email_same_case", "foo@x.com", "foo@x.com", true},
		{"email_mixed_case", "Foo@X.COM", "foo@x.com", true},
		{"email_distinct", "alice@x.com", "bob@x.com", false},
		{"non_email_same", "AliceHandle", "AliceHandle", true},
		{"non_email_case_diff", "AliceHandle", "alicehandle", false},
		{"matrix_mxid_case_diff", "@Alice:matrix.org", "@alice:matrix.org", false},
		{"phone_same", "+15551234567", "+15551234567", true},
		{"empty_both", "", "", true},
		{"one_email_one_handle", "foo@x.com", "AliceHandle", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EqualIdentifier(tc.a, tc.b); got != tc.want {
				t.Errorf("EqualIdentifier(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestIdentifierMatch_TableDriven asserts the SQL-composition contract
// of newIdentifierMatch for representative inputs. Email-shaped tokens
// produce a LOWER()-wrapped predicate; everything else produces a
// case-sensitive predicate. BindValue is always the raw input.
//
// The classification rule is "@ not at index 0 AND right side contains
// a dot" — see looksLikeEmail. This test treats that rule as the
// contract; TestLooksLikeEmail tests the predicate directly.
func TestIdentifierMatch_TableDriven(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantWhere string
	}{
		{"email", "foo@x.com", "LOWER(address) = LOWER(?)"},
		{"email_mixed_case", "Foo@X.COM", "LOWER(address) = LOWER(?)"},
		{"matrix_mxid", "@alice:matrix.org", "address = ?"},
		{"bare_handle", "AliceHandle", "address = ?"},
		{"phone", "+15551234567", "address = ?"},
		{"email_no_dot", "alice@localhost", "address = ?"},
		{"empty", "", "address = ?"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newIdentifierMatch(tc.input)
			if got := m.WhereClause("address"); got != tc.wantWhere {
				t.Errorf("WhereClause(%q) = %q, want %q", tc.input, got, tc.wantWhere)
			}
			if got := m.BindValue(); got != tc.input {
				t.Errorf("BindValue() = %q, want %q (raw)", got, tc.input)
			}
		})
	}
}

// TestIdentifierMatch_WhereClauseAcceptsCustomColumn asserts the helper
// is column-name-driven so call sites can specify their own column
// (today every site uses "address", but the contract supports more).
func TestIdentifierMatch_WhereClauseAcceptsCustomColumn(t *testing.T) {
	m := newIdentifierMatch("foo@x.com")
	if got := m.WhereClause("normalized"); got != "LOWER(normalized) = LOWER(?)" {
		t.Errorf("WhereClause(\"normalized\") = %q", got)
	}
	m2 := newIdentifierMatch("AliceHandle")
	if got := m2.WhereClause("col"); got != "col = ?" {
		t.Errorf("WhereClause(\"col\") = %q", got)
	}
}

// TestLooksLikeEmail asserts the email-shape predicate directly. The
// regression cases (iter2→iter3 Matrix MXID misclassification) are
// the load-bearing rows here: a future refactor that loosens the
// predicate to "@ contains" must fail this test.
func TestLooksLikeEmail(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"plain_email", "foo@x.com", true},
		{"mixed_case", "Foo@X.COM", true},
		{"subdomain", "foo@mail.x.com", true},
		{"matrix_mxid", "@alice:matrix.org", false},
		{"matrix_mxid_with_subdomain", "@alice:server.matrix.org", false},
		{"bare_handle", "AliceHandle", false},
		{"phone_e164", "+15551234567", false},
		{"empty", "", false},
		{"email_no_dot", "alice@localhost", false},
		{"trailing_at", "alice@", false},
		{"leading_at_only", "@", false},
		{"single_char", "a", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeEmail(tc.input); got != tc.want {
				t.Errorf("looksLikeEmail(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
