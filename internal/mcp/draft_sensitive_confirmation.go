package mcp

import (
	"strings"

	"go.kenn.io/msgvault/internal/sensitive"
)

// The existing confirmation challenge binds the exact tool and arguments.
// Display only fixed category labels here: the submitted body and subject may
// contain credentials or instructions and must not be copied into a prompt.
func sensitiveDraftConfirmationMessage(toolName string, findings []sensitive.Finding) string {
	seen := make(map[string]bool)
	for _, finding := range findings {
		seen[finding.Kind] = true
	}
	var labels []string
	for _, kind := range []string{"payment_card", "iban", "us_ssn", "api_key", "password"} {
		if seen[kind] {
			labels = append(labels, sensitiveKindLabel(kind))
		}
	}
	if len(labels) == 0 {
		labels = append(labels, "sensitive information")
	}
	return "Approve " + toolName + "? Submitted draft content may contain " + strings.Join(labels, ", ") + ". Review the draft content before approving. This creates or edits a draft; it does not send it."
}
