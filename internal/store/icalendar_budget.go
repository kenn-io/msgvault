package store

import "go.kenn.io/msgvault/internal/calevent"

// Evidence has its own retained-data limits and shares ingestion's work limit.
func (t *loggedTx) icsParseBudget() *calevent.ICSParseBudget {
	budget := calevent.NewICSParseBudget()
	if t.icsVerificationRemaining != nil {
		budget.VerificationRemaining = t.icsVerificationRemaining
	}
	return budget
}
