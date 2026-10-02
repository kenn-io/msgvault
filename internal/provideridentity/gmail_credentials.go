package provideridentity

import (
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/gmail"
	"golang.org/x/oauth2"
)

// GmailCredentialIssue identifies a credential problem with safe remediation.
type GmailCredentialIssue uint8

const (
	GmailOAuthConfiguration GmailCredentialIssue = iota + 1
	GmailTokenMissing
	GmailAuthorizationRevoked
	GmailServiceAccountConfiguration
)

// GmailCredentialError preserves the diagnostic cause for daemon logs. HTTP
// callers must use Remediation rather than expose the cause's arbitrary text.
type GmailCredentialError struct {
	issue GmailCredentialIssue
	cause error
}

// NewGmailCredentialError marks a known setup failure without exposing its cause.
func NewGmailCredentialError(issue GmailCredentialIssue, cause error) error {
	return &GmailCredentialError{issue: issue, cause: cause}
}

func (e *GmailCredentialError) Error() string {
	if e.cause == nil {
		return e.Remediation()
	}
	return e.cause.Error()
}
func (e *GmailCredentialError) Unwrap() error { return e.cause }

// Remediation contains only fixed public text. An unknown issue stays internal.
func (e *GmailCredentialError) Remediation() string {
	switch e.issue {
	case GmailOAuthConfiguration:
		return "authenticated Gmail credentials are unavailable; configure the selected source's OAuth app and client-secrets file (see https://msgvault.io/guides/oauth-setup/)"
	case GmailTokenMissing:
		return "authenticated Gmail credentials are missing; run 'msgvault add-account' for the selected account using its existing OAuth app"
	case GmailAuthorizationRevoked:
		return "authenticated Gmail authorization has expired or been revoked; run 'msgvault add-account' for the selected account using its existing OAuth app and permissions"
	case GmailServiceAccountConfiguration:
		return "authenticated Gmail service-account credentials are unavailable; check the selected source's service_account_key, key permissions, and domain-wide delegation"
	default:
		return ""
	}
}

// ClassifyGmailProfileError recognizes explicit provider authorization failures.
// Network, quota, permission, and unknown errors retain their original kind.
func ClassifyGmailProfileError(err error, serviceAccount bool) error {
	revoked := false
	if retrieved, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
		revoked = retrieved.ErrorCode == "invalid_grant"
	}
	if status, ok := errors.AsType[*gmail.StatusError](err); ok {
		revoked = revoked || status.StatusCode == http.StatusUnauthorized
	}
	if !revoked {
		return err
	}
	issue := GmailAuthorizationRevoked
	if serviceAccount {
		issue = GmailServiceAccountConfiguration
	}
	return NewGmailCredentialError(issue, err)
}
