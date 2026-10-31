package store

// personTierOwnerMatch reuses the activity loader's per-type identity matching:
// primary email is authoritative over email aliases, and native IDs are exact.
// An account_identities row is confirmed by its presence; candidates live in a
// separate table. Empty sourceExpr checks identities in every source.
func personTierOwnerMatch(participantExpr, sourceExpr string) string {
	source := ""
	if sourceExpr != "" {
		source = "ai.source_id=" + sourceExpr + " AND "
	}
	return `EXISTS (SELECT 1 FROM participants op JOIN account_identities ai ON ` + source + `(
 (` + ownerEmailMatch("op.email_address", "ai.address") + `) OR EXISTS (
 SELECT 1 FROM participant_identifiers oi WHERE oi.participant_id=op.id AND ` +
		ownerIdentifierMatch("oi.identifier_type", "oi.identifier_value", "ai.address", " AND (op.email_address IS NULL OR TRIM(op.email_address)='')") + `)) WHERE op.id=` + participantExpr + `)`
}
