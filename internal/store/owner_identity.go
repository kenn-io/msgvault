package store

// ownerEmailMatch matches a nonblank email column to address case-insensitively.
func ownerEmailMatch(column, address string) string {
	return column + " IS NOT NULL AND TRIM(" + column + ") <> '' AND LOWER(" + column + ") = LOWER(" + address + ")"
}

// ownerIdentifierMatch matches an identifier to address: email identifiers
// case-insensitively when emailGuard (empty or " AND ...") holds, others
// byte-exact. Placeholder expressions bind in order of appearance.
func ownerIdentifierMatch(typeExpr, valueExpr, address, emailGuard string) string {
	return "((" + typeExpr + " = 'email'" + emailGuard + " AND LOWER(" + valueExpr + ") = LOWER(" + address + ")) OR (" +
		typeExpr + " <> 'email' AND " + valueExpr + " = " + address + "))"
}
