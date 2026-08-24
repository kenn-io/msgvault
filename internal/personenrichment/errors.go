package personenrichment

import (
	"errors"
	"fmt"
	"strings"
)

type FailureClass string

const (
	FailurePolicy           FailureClass = "policy"
	FailureSuppressed       FailureClass = "suppressed"
	FailureRateLimited      FailureClass = "rate_limited"
	FailureTransient        FailureClass = "transient"
	FailureInvalidOutput    FailureClass = "invalid_output"
	FailureIdentityRejected FailureClass = "identity_rejected"
	FailureTerminal         FailureClass = "terminal"
	FailureUncertainStart   FailureClass = "uncertain_start"
)

var (
	ErrSuppressed                 = errors.New("person enrichment is suppressed")
	ErrConsentRequired            = errors.New("active person enrichment consent is required")
	ErrCredentialUnavailable      = errors.New("person enrichment credential is unavailable")
	ErrSuppressionKeyMismatch     = errors.New("person enrichment suppression key does not match durable state")
	ErrProgramFingerprintMismatch = errors.New("person enrichment program fingerprint changed")
	ErrRequestBudgetExceeded      = errors.New("person enrichment request budget exceeded")
	ErrCostBudgetExceeded         = errors.New("person enrichment cost budget exceeded")
	ErrAccountingDisabled         = errors.New("person enrichment starts disabled by accounting violation")
)

type ProviderError struct {
	Provider   string
	RequestID  string
	Status     int
	Class      FailureClass
	RetryAfter string
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "person enrichment provider error"
	}
	parts := []string{"person enrichment provider error"}
	if e.Provider != "" {
		parts = append(parts, fmt.Sprintf("provider=%q", e.Provider))
	}
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.Status))
	}
	if e.RequestID != "" {
		parts = append(parts, fmt.Sprintf("request_id=%q", e.RequestID))
	}
	if e.Class != "" {
		parts = append(parts, fmt.Sprintf("class=%q", e.Class))
	}
	return strings.Join(parts, " ")
}

func (e *ProviderError) Retryable() bool {
	return e != nil && (e.Class == FailureRateLimited || e.Class == FailureTransient)
}
