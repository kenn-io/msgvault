package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/oauth"
)

func TestCalendarWriteConsentPreservesNarrowedGmailAndOtherServices(t *testing.T) {
	assertions := assert.New(t)
	existing := []string{oauth.ScopeGmailReadonly, "https://www.googleapis.com/auth/drive.readonly", "openid"}
	scopes := calendarEscalationScopes(existing, true, true)
	assertions.Contains(scopes, oauth.ScopeCalendarEvents)
	assertions.Contains(scopes, oauth.ScopeCalendarReadonly)
	assertions.Contains(scopes, "https://www.googleapis.com/auth/drive.readonly")
	assertions.Contains(scopes, "openid")
	assertions.NotContains(scopes, oauth.ScopeGmailModify)
	assertions.NotContains(scopes, oauth.ScopeGmailCompose)
	assertions.NotContains(calendarEscalationScopes(existing, true), oauth.ScopeCalendarEvents)
}
