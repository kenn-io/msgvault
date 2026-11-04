package loops

import "testing"

// SetListEvidenceBudgetForTest changes the evidence budget until test cleanup.
func SetListEvidenceBudgetForTest(tb testing.TB, budget int) {
	tb.Helper()
	previousBudget := listEvidenceBudget
	listEvidenceBudget = budget
	tb.Cleanup(func() { listEvidenceBudget = previousBudget })
}
