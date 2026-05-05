package store

import (
	"fmt"
	"strings"
)

// EqualIdentifier reports whether two identifiers refer to the same
// row under the comparison rules used by AddAccountIdentity /
// RemoveAccountIdentity / MigrateLegacyIdentityConfig: email-shaped
// tokens compare case-insensitively, everything else compares
// case-sensitively.
//
// Use this when a caller has already loaded identity rows and needs
// to find the row that corresponds to a user-supplied identifier in
// memory — e.g., to read prior signals before calling
// AddAccountIdentity. Routing through this function keeps the CLI's
// in-memory matching consistent with the SQL-side LOWER() compare so
// case-mismatched re-adds do not silently bypass "already confirmed"
// UX.
func EqualIdentifier(a, b string) bool {
	if looksLikeEmail(a) || looksLikeEmail(b) {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// NormalizeIdentifierForCompare returns the comparison-canonical form
// of an identifier under the same rules as EqualIdentifier and the
// SQL-side LOWER() shape: email-shaped tokens are lowercased,
// everything else is returned unchanged.
//
// Use this when building a map keyed by identifier (e.g., the dedup
// engine's per-source identity lookup) so the same value can be used
// to insert and to look up. Calling NormalizeIdentifierForCompare on
// both the stored side and the lookup side keeps Matrix MXIDs and
// other case-sensitive synthetic identifiers intact while still
// matching email-shaped identities case-insensitively.
func NormalizeIdentifierForCompare(s string) string {
	if looksLikeEmail(s) {
		return strings.ToLower(s)
	}
	return s
}

// identifierMatch carries the comparison rule for one identifier in
// account_identities. It exists so the email-vs-other branch lives in
// one place: every call site that compares an identifier against
// stored rows builds an identifierMatch and consumes its SQL fragment
// + bind value.
//
// Email-shaped tokens (per looksLikeEmail) compare case-insensitively
// via LOWER() in SQL; everything else compares case-sensitively.
// Case-folding always happens in SQL, never in Go, so stored rows
// retain whatever casing the user supplied at first write.
//
// Use:
//
//	match := newIdentifierMatch(addr)
//	row := tx.QueryRow(
//	    `SELECT source_signal FROM account_identities
//	     WHERE source_id = ? AND `+match.WhereClause("address"),
//	    sourceID, match.BindValue(),
//	)
type identifierMatch struct {
	isEmailShaped bool
	raw           string
}

// newIdentifierMatch classifies raw via looksLikeEmail and packages
// the classification with the raw input. Empty input is permitted
// (it falls into the non-email branch); the caller is responsible
// for any empty-input gating that has to happen before SQL is run.
func newIdentifierMatch(raw string) identifierMatch {
	return identifierMatch{
		isEmailShaped: looksLikeEmail(raw),
		raw:           raw,
	}
}

// WhereClause renders an equality predicate of the form
// "LOWER(<column>) = LOWER(?)" for email-shaped identifiers and
// "<column> = ?" for everything else. The placeholder always binds
// to BindValue.
//
// SECURITY: column is interpolated into SQL without escaping. Every
// caller in this package supplies a hard-coded column name (today,
// "address" at all three sites). Do NOT pass user input as column
// — there is no SQL-injection guard here.
func (m identifierMatch) WhereClause(column string) string {
	if m.isEmailShaped {
		return fmt.Sprintf("LOWER(%s) = LOWER(?)", column)
	}
	return fmt.Sprintf("%s = ?", column)
}

// BindValue returns the value to bind for the placeholder in
// WhereClause. Equal to the raw input — case-folding happens in SQL
// via LOWER, not in Go, so the stored row keeps original casing.
func (m identifierMatch) BindValue() any {
	return m.raw
}
