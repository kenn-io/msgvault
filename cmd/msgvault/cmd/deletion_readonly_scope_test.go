package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"google.golang.org/api/drive/v3"
)

// TestDeleteStagedScopeEscalationForSource_ReadonlyWorld covers the deletion
// pre-flight for grants that only exist once accounts can be read-only.
//
// The regression it guards is the full-access case: an account holding
// mail.google.com but not gmail.modify already has more than enough access to
// trash mail, but a check requiring every member of oauth.Scopes concludes it
// does not and prompts for an upgrade it does not need.
func TestDeleteStagedScopeEscalationForSource_ReadonlyWorld(t *testing.T) {
	tests := []struct {
		name        string
		tokenJSON   string
		permanent   bool
		wantPrompt  bool
		description string
	}{
		{
			name:        "full access covers trashing",
			tokenJSON:   gmailFullOnlyTokenJSON,
			permanent:   false,
			wantPrompt:  false,
			description: "mail.google.com is a superset of gmail.modify",
		},
		{
			name:        "full access covers permanent deletion",
			tokenJSON:   gmailFullOnlyTokenJSON,
			permanent:   true,
			wantPrompt:  false,
			description: "mail.google.com is exactly what batchDelete needs",
		},
		{
			name:        "read-only grant cannot trash",
			tokenJSON:   gmailReadonlyTokenJSON,
			permanent:   false,
			wantPrompt:  true,
			description: "escalation must still be offered when access is genuinely missing",
		},
		{
			name:        "read-only grant cannot permanently delete",
			tokenJSON:   gmailReadonlyTokenJSON,
			permanent:   true,
			wantPrompt:  true,
			description: "escalation must still be offered when access is genuinely missing",
		},
		{
			name:        "modify grant covers trashing",
			tokenJSON:   gmailOnlyTokenJSON,
			permanent:   false,
			wantPrompt:  false,
			description: "unchanged from today",
		},
		{
			name:        "modify grant cannot permanently delete",
			tokenJSON:   gmailOnlyTokenJSON,
			permanent:   true,
			wantPrompt:  true,
			description: "gmail.modify does not cover batchDelete",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, restore := seedTokenEnv(t, tt.tokenJSON)
			defer restore()

			src := &store.Source{SourceType: sourceTypeGmail}
			escalation, err := deleteStagedScopeEscalationForSource(
				scopeEscalationAccount, src, tt.permanent, cfg.OAuth.ClientSecrets)

			require.NoError(t, err)
			assert.Equal(t, tt.wantPrompt, escalation.Needed, tt.description)
		})
	}
}

func TestDeletionEscalationScopes_ReadonlyWorld(t *testing.T) {
	tests := []struct {
		name        string
		batchDelete bool
		existing    []string
		want        []string
	}{
		{
			name:        "trash escalation from read-only adds modify and keeps calendar",
			batchDelete: false,
			existing:    []string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly},
			want: []string{
				oauth.ScopeGmailReadonly,
				oauth.ScopeCalendarReadonly,
				oauth.ScopeGmailModify,
			},
		},
		{
			// Full access already permits trashing, so on the pre-flight path
			// grantCoversDeletion returns true and this function is never
			// reached with such a grant. The path that DOES reach it is the
			// 403 insufficient-scope recovery, which runs precisely because
			// the recorded scopes were wrong — there, requesting only what is
			// already recorded makes re-consent a no-op and the next attempt
			// 403s again. Adding modify grants nothing new over full access
			// and keeps the recovery able to change something.
			name:        "trash escalation still requests modify alongside full access",
			batchDelete: false,
			existing:    []string{oauth.ScopeGmailReadonly, oauth.ScopeGmailFull},
			want: []string{
				oauth.ScopeGmailReadonly,
				oauth.ScopeGmailFull,
				oauth.ScopeGmailModify,
			},
		},
		{
			name:        "permanent escalation from read-only adds full access",
			batchDelete: true,
			existing:    []string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly},
			want: []string{
				oauth.ScopeGmailReadonly,
				oauth.ScopeCalendarReadonly,
				oauth.ScopeGmailFull,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, tt.want,
				deletionEscalationScopes(tt.batchDelete, tt.existing))
		})
	}
}

// TestCalendarEscalationScopes_DoesNotRewidenNarrowGrant is the add-calendar
// half of the "read-only stays read-only" rule. Preserving Gmail while adding
// Calendar must preserve the Gmail access the account has, not the full bundle.
func TestCalendarEscalationScopes_DoesNotRewidenNarrowGrant(t *testing.T) {
	tests := []struct {
		name     string
		existing []string
		want     []string
	}{
		{
			name:     "read-only gmail stays read-only",
			existing: []string{oauth.ScopeGmailReadonly},
			want:     []string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly},
		},
		{
			name:     "read-only gmail with drive stays read-only and keeps drive",
			existing: []string{oauth.ScopeGmailReadonly, drive.DriveReadonlyScope},
			want: []string{
				oauth.ScopeGmailReadonly,
				drive.DriveReadonlyScope,
				oauth.ScopeCalendarReadonly,
			},
		},
		{
			name:     "write-capable gmail is unchanged",
			existing: oauth.Scopes,
			want: []string{
				oauth.ScopeGmailReadonly,
				oauth.ScopeGmailModify,
				oauth.ScopeCalendarReadonly,
			},
		},
		{
			// Not a narrowed grant — it holds write access — so the rule does
			// not apply. Existing scopes are carried forward regardless, so
			// full access survives either way.
			name:     "full access only gmail keeps its grant",
			existing: []string{oauth.ScopeGmailFull},
			want:     []string{oauth.ScopeGmailFull, oauth.ScopeCalendarReadonly},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preserveGmail := calendarShouldPreserveGmail(true, true, tt.existing)
			assert.ElementsMatch(t, tt.want,
				calendarEscalationScopes(tt.existing, preserveGmail))
		})
	}
}
