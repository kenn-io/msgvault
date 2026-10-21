package store

// EmailFamilyLookupQueriesForTest returns the two identifier lookups an email
// family walk runs, each with one %s for the placeholders.
func (s *Store) EmailFamilyLookupQueriesForTest(scope, kinds string) (byOwnID, byKey string) {
	return s.emailFamilyLookupQueries(scope, kinds)
}

// EmailFamilyDirectKindsForTest is the kind filter of the direct-link lookup.
const EmailFamilyDirectKindsForTest = emailFamilyDirectKinds

// EmailFamilyConversationQueryForTest is the read of one conversation that seeds a walk.
func EmailFamilyConversationQueryForTest(scope string, limit int) string {
	return emailFamilyConversationQuery(scope, 1, limit)
}

// EmailFamilyPendingQueryForTest is the probe for in-scope emails awaiting linkage.
func EmailFamilyPendingQueryForTest(scope string) string { return emailFamilyPendingQuery(scope) }
