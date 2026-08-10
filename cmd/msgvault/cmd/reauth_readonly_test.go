package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extOAuth2 "golang.org/x/oauth2"

	"go.kenn.io/msgvault/internal/oauth"
)

// scopedMockReauthorizer adds recorded grant scopes to mockReauthorizer, so
// remediation guidance can be exercised against an account that was
// deliberately narrowed to read-only.
type scopedMockReauthorizer struct {
	*mockReauthorizer

	scopes []string
}

func (m *scopedMockReauthorizer) GrantedScopes(string) []string {
	return append([]string(nil), m.scopes...)
}

func newScopedReauthorizer(scopes []string, authorizeErr error) *scopedMockReauthorizer {
	return &scopedMockReauthorizer{
		mockReauthorizer: &mockReauthorizer{
			tokenSourceFn: func(_ context.Context, _ string) (extOAuth2.TokenSource, error) {
				return nil, &extOAuth2.RetrieveError{ErrorCode: "invalid_grant"}
			},
			hasTokenVal: true,
			authorizeFn: func(_ context.Context, _ string) error { return authorizeErr },
		},
		scopes: scopes,
	}
}

// TestReauthHintPreservesReadonlyGrant is the regression for remediation
// guidance that silently widened an account. A read-only account whose token
// expired was told to run `add-account <addr> --force`, which requests write
// access — undoing the narrowing at the moment the operator is least likely to
// notice, since they are following instructions to fix an unrelated failure.
func TestReauthHintPreservesReadonlyGrant(t *testing.T) {
	tests := []struct {
		name          string
		scopes        []string
		wantReadonly  bool
		wantInCommand string
	}{
		{
			name:          "narrowed account keeps --readonly",
			scopes:        []string{oauth.ScopeGmailReadonly},
			wantReadonly:  true,
			wantInCommand: "add-account x@gmail.com --readonly --force",
		},
		{
			name:          "narrowed account with calendar keeps --readonly",
			scopes:        []string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly},
			wantReadonly:  true,
			wantInCommand: "add-account x@gmail.com --readonly --force",
		},
		{
			name:          "write-capable account is unchanged",
			scopes:        oauth.Scopes,
			wantReadonly:  false,
			wantInCommand: "add-account x@gmail.com --force",
		},
		{
			// No Gmail scopes at all is not a narrowed grant, so guidance
			// stays exactly as it was before --readonly existed.
			name:          "calendar-only account is unchanged",
			scopes:        []string{oauth.ScopeCalendarReadonly},
			wantReadonly:  false,
			wantInCommand: "add-account x@gmail.com --force",
		},
		{
			name:          "legacy token with no recorded scopes is unchanged",
			scopes:        nil,
			wantReadonly:  false,
			wantInCommand: "add-account x@gmail.com --force",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			mock := newScopedReauthorizer(tt.scopes, nil)

			// Non-interactive: the out-of-band remedy is printed rather than
			// a browser being opened.
			_, err := getTokenSourceWithReauth(
				context.Background(), mock, "x@gmail.com", false, gmailReauthHint)

			require.Error(err)
			assert.Contains(err.Error(), tt.wantInCommand)
			if !tt.wantReadonly {
				assert.NotContains(err.Error(), "--readonly")
			}
		})
	}
}

// TestReauthHintCommandIsNotItselfRefused closes the loop between the two
// halves of this change. add-account refuses `--readonly --force`, and the
// expired-token hint suggests exactly that string for a narrowed account —
// which reads like a contradiction, and would be one if the refusal keyed on
// the flags rather than on the grant.
//
// It keys on the grant: a narrowed account holds no write scope, so the
// suggested command is accepted. Asserting that here means the two rules cannot
// drift into genuinely contradicting each other without a test failing.
func TestReauthHintCommandIsNotItselfRefused(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	narrowed := []string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly}

	hint := gmailReauthHint("x@gmail.com", true)
	assert.Contains(hint, "--readonly --force",
		"precondition: the hint suggests the combination under test")

	// The same grant the hint was generated for, run through the decision the
	// suggested command would hit.
	decision := decideAddAccountGrant(
		"x@gmail.com", true, true, narrowed, true, false,
		"/tmp/tokens/x@gmail.com.json")

	require.NoError(decision.Err,
		"the hint must not recommend a command add-account would refuse")
	assert.Empty(decision.Warning,
		"re-authorizing a narrowed account at its own scope is not a widening")
}

// TestReauthAliasMismatchPreservesReadonlyGrant covers the other remediation
// site: the alias-mismatch hint printed after an interactive reauth resolves to
// a different Google account. Its remove-and-re-add recipe must not drop the
// grant mode either.
func TestReauthAliasMismatchPreservesReadonlyGrant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	mismatch := &oauth.TokenMismatchError{
		Expected: "alias@gmail.com",
		Actual:   "primary@gmail.com",
	}
	mock := newScopedReauthorizer([]string{oauth.ScopeGmailReadonly}, mismatch)

	_, err := getTokenSourceWithReauth(
		context.Background(), mock, "alias@gmail.com", true, gmailReauthHint)

	require.Error(err)
	var got *oauth.TokenMismatchError
	require.ErrorAs(err, &got, "mismatch error must survive wrapping")
	assert.Contains(err.Error(), "msgvault add-account primary@gmail.com --readonly")
}

// TestReauthAliasMismatchUnchangedForWriteGrant pins that the alias recipe is
// untouched for an ordinary write-capable account.
func TestReauthAliasMismatchUnchangedForWriteGrant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	mismatch := &oauth.TokenMismatchError{
		Expected: "alias@gmail.com",
		Actual:   "primary@gmail.com",
	}
	mock := newScopedReauthorizer(oauth.Scopes, mismatch)

	_, err := getTokenSourceWithReauth(
		context.Background(), mock, "alias@gmail.com", true, gmailReauthHint)

	require.Error(err)
	assert.Contains(err.Error(), "msgvault add-account primary@gmail.com")
	assert.NotContains(err.Error(), "--readonly")
}

// TestAddAccountGrantFlagSuffix covers the add-account mismatch hint's flag
// rendering, which repeats the current run's grant mode rather than the
// account's recorded one — no token exists yet at that point.
func TestAddAccountGrantFlagSuffix(t *testing.T) {
	saveAddAccountFlags(t)

	readonlyGrant = false
	assert.Empty(t, addAccountGrantFlagSuffix())

	readonlyGrant = true
	assert.Equal(t, " --readonly", addAccountGrantFlagSuffix())
}

// TestAddAccountAuthorizeErrorRepeatsReadonly is the behavioural half: a
// --readonly run that hits an account mismatch must not print a re-add command
// that would request write access.
func TestAddAccountAuthorizeErrorRepeatsReadonly(t *testing.T) {
	assert := assert.New(t)
	saveAddAccountFlags(t)

	mismatch := &oauth.TokenMismatchError{
		Expected: "alias@gmail.com",
		Actual:   "primary@gmail.com",
	}

	readonlyGrant = true
	err := addAccountAuthorizeError(mismatch, false)
	assert.Contains(err.Error(), "msgvault add-account primary@gmail.com --readonly")

	readonlyGrant = false
	err = addAccountAuthorizeError(mismatch, false)
	assert.Contains(err.Error(), "msgvault add-account primary@gmail.com")
	assert.NotContains(err.Error(), "--readonly")
}
