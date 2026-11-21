package store

import "context"

// activityIdentityPopulationSQL supplies the visible identities and authorized
// account ownership used by both tier scoring and activity exclusions.
func activityIdentityPopulationSQL(ctx context.Context) (string, []any) {
	ownerScope, args := sourceScopeClause(ctx, "ai.source_id")
	visibleScope, visibleArgs := sourceScopeClause(ctx, "observed.source_id")
	visible := ""
	if visibleScope != "" {
		visible = ` WHERE EXISTS(SELECT 1 FROM messages observed WHERE ` + activityParticipantMatchSQL("observed", "p.id") + visibleScope + `)`
	}
	args = append(args, visibleArgs...)
	return `SELECT p.*,pp.person_id,CASE WHEN EXISTS (
 SELECT 1 FROM account_identities ai WHERE ((` + ownerEmailMatch("p.email_address", "ai.address") + `) OR EXISTS (
 SELECT 1 FROM participant_identifiers oi WHERE oi.participant_id=p.id AND ` +
		ownerIdentifierMatch("oi.identifier_type", "oi.identifier_value", "ai.address", " AND (p.email_address IS NULL OR TRIM(p.email_address)='')") + `))` + ownerScope + `)
 THEN 1 ELSE 0 END AS is_account_owner
 FROM participants p LEFT JOIN person_participants pp ON pp.participant_id=p.id` + visible, args
}
