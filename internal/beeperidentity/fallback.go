package beeperidentity

import (
	"strconv"
	"strings"
)

// Prefix namespaces Beeper fallback identities.
const Prefix = "beeper"

// EncodePart preserves delimiters by prefixing the UTF-8 byte length.
func EncodePart(value string) string {
	return strconv.Itoa(len(value)) + ":" + value
}

// Fallback is the one durable namespace for an opaque Beeper user ID. The raw
// ID is only unique inside its account, so callers that persist fallback
// identities must store this value rather than the payload's ID.
func Fallback(accountID, userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ""
	}
	if account := strings.TrimSpace(accountID); account != "" {
		return Prefix + ":" + EncodePart(account) + ":" + EncodePart(userID)
	}
	return Prefix + ":" + EncodePart(userID)
}

// FallbackSQL encodes trusted SQL expressions with the same bytes as Fallback.
func FallbackSQL(driverName, accountExpr, userExpr string) string {
	account := trimSQL(driverName, accountExpr)
	user := trimSQL(driverName, userExpr)
	part := func(value string) string {
		return partSQL(driverName, value)
	}
	return "CASE WHEN " + user + " = '' THEN '' WHEN " + account + " = '' THEN '" + Prefix + ":' || " + part(user) +
		" ELSE '" + Prefix + ":' || " + part(account) + " || ':' || " + part(user) + " END"
}

func trimSQL(driverName, expr string) string {
	// This is the Unicode White_Space set used by strings.TrimSpace.
	const whitespace = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
	trim := "trim"
	if driverName == "pgx" {
		trim = "btrim"
	}
	return trim + "(" + expr + ", '" + whitespace + "')"
}

func partSQL(driverName, value string) string {
	length := "length(CAST(" + value + " AS BLOB))"
	if driverName == "pgx" {
		length = "octet_length(" + value + ")"
	}
	return "CAST(" + length + " AS TEXT) || ':' || " + value
}

// ScopedUserSQL returns the user in a canonical account-scoped key, or NULL.
// All arguments are trusted SQL expressions.
func ScopedUserSQL(driverName, accountExpr, keyExpr string) string {
	account := trimSQL(driverName, accountExpr)
	prefix := "'" + Prefix + ":' || " + partSQL(driverName, account) + " || ':'"
	var rest, user string
	if driverName == "pgx" {
		rest = "substring(" + keyExpr + " from char_length(" + prefix + ") + 1)"
		user = "substring(" + rest + " from position(':' in " + rest + ") + 1)"
	} else {
		rest = "substr(CAST(" + keyExpr + " AS BLOB), length(CAST(" + prefix + " AS BLOB)) + 1)"
		user = "CAST(substr(" + rest + ", instr(" + rest + ", X'3A') + 1) AS TEXT)"
	}
	return "CASE WHEN " + account + " <> '' AND " + keyExpr + " = " + FallbackSQL(driverName, accountExpr, user) + " THEN " + user + " ELSE NULL END"
}
