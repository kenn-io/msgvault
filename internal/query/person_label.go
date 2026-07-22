package query

// Display-label policy for identity clusters, shared by the ranked
// relationships list (buildRelationshipsSQL) and person search/detail
// (searchPeople). The canonical cluster ID stays the join/dedup key
// everywhere; only the label evaluates all cluster members: the non-empty
// display_name of the smallest-ID member wins, so linking an older unnamed
// participant to a named alias never degrades the label to a bare identifier
// or "Unknown person". The identifier fallback chain (phone → email → stored
// identifier evidence → "Unknown person #id") applies only when no member of
// the cluster is named.

// sqlClusterBestNameExpr renders the deterministic best-name selection: a
// scalar subquery yielding the non-empty display_name of the smallest-ID
// participant matched by memberFilter (a predicate over the participants
// alias "pbn"), or NULL when none of the matched members is named. ORDER BY
// pbn.id pins the tie-break so a cluster with several named members renders
// the same label across cache rebuilds.
func sqlClusterBestNameExpr(memberFilter string) string {
	return "(SELECT NULLIF(TRIM(pbn.display_name), '') FROM participants pbn WHERE " + memberFilter +
		" AND TRIM(COALESCE(pbn.display_name, '')) <> '' ORDER BY pbn.id LIMIT 1)"
}

// sqlPersonIdentifierFallbackExpr renders the identifier fallback chain for
// one participants row (alias): phone → email → best stored identifier
// evidence → "Unknown person #id". It deliberately excludes display_name;
// callers put a best-name expression (the participant's own name, or
// sqlClusterBestNameExpr across its cluster) in front of it.
func sqlPersonIdentifierFallbackExpr(alias string) string {
	return "COALESCE(NULLIF(TRIM(" + alias + ".phone_number), ''), NULLIF(TRIM(" + alias + ".email_address), ''),\n" +
		"        (SELECT COALESCE(NULLIF(TRIM(pi.display_value), ''), pi.identifier_value) FROM participant_identifiers pi\n" +
		"         WHERE pi.participant_id = " + alias + ".id ORDER BY pi.is_primary DESC, pi.identifier_type, pi.identifier_value LIMIT 1),\n" +
		"        'Unknown person #' || CAST(" + alias + ".id AS VARCHAR))"
}

// sqlPersonDisplayLabelExpr composes the full label policy: bestNameExpr
// first, then the identifier fallback chain on the identity's own row.
func sqlPersonDisplayLabelExpr(bestNameExpr, alias string) string {
	return "COALESCE(" + bestNameExpr + ", " + sqlPersonIdentifierFallbackExpr(alias) + ")"
}
