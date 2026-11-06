package keepintouch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/taskclient"
)

// failureSummary deliberately uses fixed operational text. Scheduled logs must
// not copy model output, source text or credentials from arbitrary error text.
func failureSummary(err error) string {
	switch {
	case errors.Is(err, taskclient.ErrNotificationOccupied):
		return "existing Kata attention request must be cleared before retrying delivery"
	case errors.Is(err, peoplesweep.ErrPersonSweepConsentRevoked):
		return "inference consent is missing or revoked; check person provider status"
	case errors.Is(err, peoplesweep.ErrInvalidStructuredOutput):
		return "inference provider returned an invalid draft"
	case errors.Is(err, taskclient.ErrAuthenticationRequired), errors.Is(err, taskclient.ErrNotFound):
		return "Kata requires complete issue access; check the project and credentials"
	case errors.Is(err, taskclient.ErrUnreachable), errors.Is(err, context.DeadlineExceeded):
		return "outreach service request timed out or is unreachable"
	case errors.Is(err, taskclient.ErrConflict):
		return "Kata issue changed; review it before retrying"
	case errors.Is(err, personagenda.ErrPersonIdentity):
		return "person has no stable UID for Kata linking"
	case errors.Is(err, context.Canceled):
		return "outreach operation was cancelled"
	default:
		return "outreach preparation failed; inspect a manual JSON check for details"
	}
}

// FailureSummary is bounded operational status for scheduled jobs. Full owner
// diagnostics remain in Failures, separate from text safe for daemon logs.
func (r *Result) FailureSummary() string {
	parts := make([]string, 0, min(len(r.Failures), 5))
	for _, failure := range r.Failures[:min(len(r.Failures), 5)] {
		summary := failure.Summary
		if summary == "" {
			summary = "outreach preparation failed; inspect a manual JSON check for details"
		}
		parts = append(parts, fmt.Sprintf("person %d: %s", failure.PersonID, summary))
	}
	if len(r.Failures) > 5 {
		parts = append(parts, fmt.Sprintf("and %d more failures", len(r.Failures)-5))
	}
	return strings.Join(parts, "; ")
}
