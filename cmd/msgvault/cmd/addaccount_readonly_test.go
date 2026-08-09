package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/oauth"
)

// Token fixtures for the grants that only become reachable once read-only
// accounts exist. No real credentials are present.

const gmailReadonlyTokenJSON = `{
  "access_token": "fake-access-token",
  "token_type": "Bearer",
  "refresh_token": "fake-refresh-token",
  "expiry": "2099-01-01T00:00:00Z",
  "scopes": [
    "https://www.googleapis.com/auth/gmail.readonly"
  ],
  "client_id": "test.apps.googleusercontent.com"
}`

const gmailReadonlyCalendarTokenJSON = `{
  "access_token": "fake-access-token",
  "token_type": "Bearer",
  "refresh_token": "fake-refresh-token",
  "expiry": "2099-01-01T00:00:00Z",
  "scopes": [
    "https://www.googleapis.com/auth/gmail.readonly",
    "https://www.googleapis.com/auth/calendar.readonly"
  ],
  "client_id": "test.apps.googleusercontent.com"
}`

// gmailFullOnlyTokenJSON holds the broad full-access scope and NOT
// gmail.modify. Produced by narrowing an account to read-only and later
// escalating it for permanent deletion. Any write check that looks only at
// gmail.modify gets this account wrong.
const gmailFullOnlyTokenJSON = `{
  "access_token": "fake-access-token",
  "token_type": "Bearer",
  "refresh_token": "fake-refresh-token",
  "expiry": "2099-01-01T00:00:00Z",
  "scopes": [
    "https://www.googleapis.com/auth/gmail.readonly",
    "https://mail.google.com/"
  ],
  "client_id": "test.apps.googleusercontent.com"
}`

// saveAddAccountFlags snapshots the package-level add-account flag globals so
// a test can set them without leaking into the rest of the package.
func saveAddAccountFlags(t *testing.T) {
	t.Helper()
	savedHeadless, savedForce, savedReadonly := headless, forceReauth, readonlyGrant
	savedApp, savedName, savedNoDefault := oauthAppName, accountDisplayName, noDefaultIdentityAddAccount
	t.Cleanup(func() {
		headless, forceReauth, readonlyGrant = savedHeadless, savedForce, savedReadonly
		oauthAppName, accountDisplayName, noDefaultIdentityAddAccount = savedApp, savedName, savedNoDefault
	})
}

// runAddAccountForTest drives the real add-account command against the seeded
// environment and returns everything it printed. os.Stdout is captured rather
// than a cobra buffer because the command reports progress with fmt.Printf, so
// a buffer alone would miss most of what an operator actually sees.
//
// The context is pre-cancelled, so a run that reaches the browser flow fails
// immediately instead of hanging. Tests that expect no authorization assert on
// the absence of that failure.
func runAddAccountForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	testCmd := &cobra.Command{
		Use:  addAccountUse,
		Args: cobra.ExactArgs(1),
		RunE: runAddAccountLocal,
	}
	registerAddAccountFlags(testCmd)

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs(append([]string{"add-account"}, args...))

	reader, writer, err := os.Pipe()
	require.NoError(t, err, "create stdout pipe")
	savedStdout := os.Stdout
	os.Stdout = writer

	captured := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, reader)
		captured <- buf.String()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runErr := root.ExecuteContext(ctx)

	os.Stdout = savedStdout
	require.NoError(t, writer.Close(), "close stdout pipe")
	out := <-captured
	require.NoError(t, reader.Close(), "close stdout reader")

	t.Log(out)
	if runErr != nil {
		return out, fmt.Errorf("run add-account: %w", runErr)
	}
	return out, nil
}

func TestAddAccountOAuthScopesForToken_Readonly(t *testing.T) {
	tests := []struct {
		name             string
		hasScopeMetadata bool
		existing         []string
		readonly         bool
		want             []string
	}{
		{
			name:     "fresh account requests read and write by default",
			readonly: false,
			want:     oauth.Scopes,
		},
		{
			name:     "fresh account with readonly requests read only",
			readonly: true,
			want:     []string{oauth.ScopeGmailReadonly},
		},
		{
			name:             "narrowing drops every write scope and keeps calendar",
			hasScopeMetadata: true,
			existing: []string{
				oauth.ScopeGmailReadonly,
				oauth.ScopeGmailModify,
				oauth.ScopeGmailFull,
				oauth.ScopeCalendarReadonly,
			},
			readonly: true,
			want:     []string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly},
		},
		{
			name:             "narrowing an account that only holds full access still yields read only",
			hasScopeMetadata: true,
			existing:         []string{oauth.ScopeGmailFull},
			readonly:         true,
			want:             []string{oauth.ScopeGmailReadonly},
		},
		{
			name:             "default request over an existing grant is unchanged",
			hasScopeMetadata: true,
			existing:         []string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly},
			readonly:         false,
			want: []string{
				oauth.ScopeGmailReadonly,
				oauth.ScopeCalendarReadonly,
				oauth.ScopeGmailModify,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := addAccountOAuthScopesForToken(tt.hasScopeMetadata, tt.existing, tt.readonly)
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

func TestDecideAddAccountGrant(t *testing.T) {
	const email = "user@example.com"

	tests := []struct {
		name             string
		hasToken         bool
		hasScopeMetadata bool
		granted          []string
		readonly         bool
		force            bool
		wantErr          bool
		wantErrContains  string
		wantWarn         bool
	}{
		{
			name:     "brand new account never warns",
			readonly: false,
		},
		{
			name:     "brand new account accepts readonly",
			readonly: true,
		},
		{
			// A token predating scope recording holds an unverifiable grant,
			// and tokens that old were minted with read + modify. Treating
			// "unknown" as "already narrow" would report success over a still
			// write-capable account.
			name:             "legacy token without scope metadata is refused under readonly",
			hasToken:         true,
			hasScopeMetadata: false,
			readonly:         true,
			wantErr:          true,
			wantErrContains:  "predates scope recording",
		},
		{
			name:             "legacy token with force proceeds",
			hasToken:         true,
			hasScopeMetadata: false,
			readonly:         true,
			force:            true,
		},
		{
			name:             "legacy token is left alone on a default run",
			hasToken:         true,
			hasScopeMetadata: false,
			readonly:         false,
		},
		{
			name:             "readonly over a modify grant is refused",
			hasToken:         true,
			hasScopeMetadata: true,
			granted:          oauth.Scopes,
			readonly:         true,
			wantErr:          true,
			wantErrContains:  "--readonly --force",
		},
		{
			// The full-access scope is the write scope a check that only
			// knows gmail.modify would miss.
			name:             "readonly over a full access grant is refused",
			hasToken:         true,
			hasScopeMetadata: true,
			granted:          []string{oauth.ScopeGmailReadonly, oauth.ScopeGmailFull},
			readonly:         true,
			wantErr:          true,
			wantErrContains:  oauth.ScopeGmailFull,
		},
		{
			name:             "readonly with force proceeds",
			hasToken:         true,
			hasScopeMetadata: true,
			granted:          oauth.Scopes,
			readonly:         true,
			force:            true,
		},
		{
			name:             "readonly over an already narrow grant is a silent no-op",
			hasToken:         true,
			hasScopeMetadata: true,
			granted:          []string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly},
			readonly:         true,
		},
		{
			name:             "default run over a narrow grant warns before widening",
			hasToken:         true,
			hasScopeMetadata: true,
			granted:          []string{oauth.ScopeGmailReadonly},
			readonly:         false,
			wantWarn:         true,
		},
		{
			name:             "default run over a write grant does not warn",
			hasToken:         true,
			hasScopeMetadata: true,
			granted:          oauth.Scopes,
			readonly:         false,
		},
		{
			// Adding Gmail to a Calendar-only token is a first Gmail grant,
			// not a widening of a narrowed one.
			name:             "calendar only token does not warn",
			hasToken:         true,
			hasScopeMetadata: true,
			granted:          []string{oauth.ScopeCalendarReadonly},
			readonly:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			got := decideAddAccountGrant(
				email, tt.hasToken, tt.hasScopeMetadata, tt.granted, tt.readonly, tt.force)

			if tt.wantErr {
				require.Error(got.Err)
				assert.Contains(got.Err.Error(), tt.wantErrContains)
			} else {
				require.NoError(got.Err)
			}
			if tt.wantWarn {
				assert.Contains(got.Warning, "read-only Gmail access")
			} else {
				assert.Empty(got.Warning)
			}
		})
	}
}

func TestAddAccountTokenHasGmailScopes_Readonly(t *testing.T) {
	tests := []struct {
		name      string
		tokenJSON string
		readonly  bool
		want      bool
	}{
		{
			name:      "readonly run accepts a read-only token",
			tokenJSON: gmailReadonlyTokenJSON,
			readonly:  true,
			want:      true,
		},
		{
			name:      "default run rejects a read-only token",
			tokenJSON: gmailReadonlyTokenJSON,
			readonly:  false,
			want:      false,
		},
		{
			name:      "default run accepts a full gmail token",
			tokenJSON: gmailOnlyTokenJSON,
			readonly:  false,
			want:      true,
		},
		{
			name:      "readonly run accepts a write token as sufficient for reading",
			tokenJSON: gmailOnlyTokenJSON,
			readonly:  true,
			want:      true,
		},
		{
			name:      "default run tolerates a legacy token with no recorded scopes",
			tokenJSON: legacyTokenJSON,
			readonly:  false,
			want:      true,
		},
		{
			// Never reuse an unverifiable grant as though it were narrow.
			name:      "readonly run rejects a legacy token with no recorded scopes",
			tokenJSON: legacyTokenJSON,
			readonly:  true,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, restore := seedTokenEnv(t, tt.tokenJSON)
			defer restore()

			mgr, err := oauth.NewManager(cfg.OAuth.ClientSecrets, cfg.TokensDir(), logger)
			require.NoError(t, err)

			assert.Equal(t, tt.want,
				addAccountTokenHasGmailScopes(mgr, scopeEscalationAccount, tt.readonly))
		})
	}
}

// TestAddAccount_ReadonlyRefusesWriteCapableAccount covers requirement 4: the
// run must fail rather than report success while leaving write access in
// place, and it must leave the token untouched so nothing is lost.
func TestAddAccount_ReadonlyRefusesWriteCapableAccount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	saveAddAccountFlags(t)
	tokenPath, restore := seedTokenEnv(t, gmailCalendarTokenJSON)
	defer restore()

	before, err := os.ReadFile(tokenPath)
	require.NoError(err)

	out, err := runAddAccountForTest(t, scopeEscalationAccount, "--readonly", "--no-default-identity")

	require.Error(err)
	assert.Contains(err.Error(), "already has Gmail write access")
	assert.Contains(err.Error(), oauth.ScopeGmailModify)
	assert.Contains(err.Error(), "--readonly --force")
	assert.NotContains(out, "Starting browser authorization")

	after, readErr := os.ReadFile(tokenPath)
	require.NoError(readErr)
	assert.Equal(before, after, "refusal must not touch the existing token")
}

// TestAddAccount_ReadonlyRefusesFullAccessAccount is the same refusal for an
// account whose only write scope is the broad full-access one. A check written
// against gmail.modify alone would silently downgrade nothing here and report
// success.
func TestAddAccount_ReadonlyRefusesFullAccessAccount(t *testing.T) {
	saveAddAccountFlags(t)
	_, restore := seedTokenEnv(t, gmailFullOnlyTokenJSON)
	defer restore()

	_, err := runAddAccountForTest(t, scopeEscalationAccount, "--readonly", "--no-default-identity")

	require.Error(t, err)
	assert.Contains(t, err.Error(), oauth.ScopeGmailFull)
}

// TestAddAccount_ReadonlyRefusesLegacyTokenWithoutScopeMetadata is the
// regression for a hole in the refusal contract: a token predating scope
// recording has no scopes array, so the write-access check found nothing to
// object to, the token was reused, and the command reported success over an
// account that still held gmail.modify.
//
// Tokens that old were minted when read + modify was the only scope set, so
// "no recorded scopes" must be treated as "possibly write-capable", not as
// "already narrow".
func TestAddAccount_ReadonlyRefusesLegacyTokenWithoutScopeMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	saveAddAccountFlags(t)
	tokenPath, restore := seedTokenEnv(t, legacyTokenJSON)
	defer restore()

	before, err := os.ReadFile(tokenPath)
	require.NoError(err)

	out, err := runAddAccountForTest(t, scopeEscalationAccount, "--readonly", "--no-default-identity")

	require.Error(err)
	assert.Contains(err.Error(), "predates scope recording")
	assert.Contains(err.Error(), "--readonly --force")
	assert.NotContains(out, "already authorized")
	assert.NotContains(out, "Starting browser authorization")

	after, readErr := os.ReadFile(tokenPath)
	require.NoError(readErr)
	assert.Equal(before, after, "refusal must not touch the existing token")
}

// TestAddAccount_LegacyTokenStillReusableWithoutReadonly pins the other half:
// only --readonly is affected. A default run keeps its existing tolerance for
// tokens with no recorded scopes.
func TestAddAccount_LegacyTokenStillReusableWithoutReadonly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	saveAddAccountFlags(t)
	_, restore := seedTokenEnv(t, legacyTokenJSON)
	defer restore()

	out, err := runAddAccountForTest(t, scopeEscalationAccount, "--no-default-identity")

	require.NoError(err)
	assert.Contains(out, "already authorized")
	assert.NotContains(out, "Warning")
}

// TestAddAccount_ReadonlyReusesAlreadyNarrowToken covers requirement 6: no
// refusal, no warning, and no pointless re-authorization.
func TestAddAccount_ReadonlyReusesAlreadyNarrowToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	saveAddAccountFlags(t)
	tokenPath, restore := seedTokenEnv(t, gmailReadonlyCalendarTokenJSON)
	defer restore()

	before, err := os.ReadFile(tokenPath)
	require.NoError(err)

	out, err := runAddAccountForTest(t, scopeEscalationAccount, "--readonly", "--no-default-identity")

	require.NoError(err)
	assert.Contains(out, "already authorized")
	assert.NotContains(out, "Warning")
	assert.NotContains(out, "Starting browser authorization")

	after, readErr := os.ReadFile(tokenPath)
	require.NoError(readErr)
	assert.Equal(before, after, "no-op must not rewrite the token")
}

// TestAddAccount_WarnsBeforeRewideningNarrowGrant covers requirement 7. The run
// then fails on the cancelled context at the browser flow, which is expected —
// what matters is that the warning was printed first.
func TestAddAccount_WarnsBeforeRewideningNarrowGrant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	saveAddAccountFlags(t)
	_, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
	defer restore()

	out, err := runAddAccountForTest(t, scopeEscalationAccount, "--no-default-identity")

	require.Error(err, "browser authorization cannot complete on a cancelled context")
	assert.Contains(out, "currently has read-only Gmail access")
	assert.Contains(out, oauth.ScopeGmailModify)
	assert.Contains(out, "--readonly")
}

// TestAddAccount_NoWarningForBrandNewAccount is the negative half of
// requirement 7: the most common case must stay quiet.
func TestAddAccount_NoWarningForBrandNewAccount(t *testing.T) {
	saveAddAccountFlags(t)
	_, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
	defer restore()

	// A different address than the seeded token, so this account has no
	// prior grant at all.
	out, err := runAddAccountForTest(t, "fresh@example.com", "--no-default-identity")

	require.Error(t, err, "browser authorization cannot complete on a cancelled context")
	assert.NotContains(t, out, "read-only Gmail access")
	assert.NotContains(t, out, "Warning")
}

// TestAddAccount_HeadlessWarnsBeforeRewideningNarrowGrant covers the headless
// half of the widening warning: without it, a plain --headless run against a
// narrowed account prints instructions that silently restore write access
// when followed on the browser machine.
func TestAddAccount_HeadlessWarnsBeforeRewideningNarrowGrant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	saveAddAccountFlags(t)
	_, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
	defer restore()

	out, err := runAddAccountForTest(t, scopeEscalationAccount, "--headless", "--no-default-identity")

	require.NoError(err, "headless run only prints instructions")
	assert.Contains(out, "currently has read-only Gmail access")
	assert.Contains(out, "Headless Server Setup",
		"warning must not replace the instructions")
}

// TestAddAccount_HeadlessReadonlyRefusesWriteCapableAccount is the headless
// half of the narrowing refusal: printing copy-a-token instructions for a
// still-write-capable account would report a narrowing path that skips
// revocation entirely.
func TestAddAccount_HeadlessReadonlyRefusesWriteCapableAccount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	saveAddAccountFlags(t)
	_, restore := seedTokenEnv(t, gmailCalendarTokenJSON)
	defer restore()

	out, err := runAddAccountForTest(t, scopeEscalationAccount, "--headless", "--readonly", "--no-default-identity")

	require.Error(err)
	assert.Contains(err.Error(), "already has Gmail write access")
	assert.NotContains(out, "Headless Server Setup",
		"refusal must stop before instructions are printed")
}

// TestAddAccount_HeadlessFreshAccountPrintsReadonlyInstructions pins that the
// grant decision stays quiet for an account with no token and that the
// instructions echo --readonly.
func TestAddAccount_HeadlessFreshAccountPrintsReadonlyInstructions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	saveAddAccountFlags(t)
	_, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
	defer restore()

	out, err := runAddAccountForTest(t, "fresh@example.com", "--headless", "--readonly", "--no-default-identity")

	require.NoError(err)
	assert.NotContains(out, "Warning")
	assert.Contains(out, "msgvault add-account fresh@example.com --readonly")
}

// TestAddAccountAuthorizeError_PreservesReadonly pins that the re-add hint
// for a consent-screen account mismatch carries the run's grant mode: a hint
// without --readonly would have the operator silently request write access.
func TestAddAccountAuthorizeError_PreservesReadonly(t *testing.T) {
	mismatch := &oauth.TokenMismatchError{
		Expected: "alias@example.com",
		Actual:   "primary@example.com",
	}

	err := addAccountAuthorizeError(mismatch, false, true)
	require.ErrorContains(t, err, "msgvault add-account primary@example.com --readonly")

	err = addAccountAuthorizeError(mismatch, false, false)
	require.ErrorContains(t, err, "msgvault add-account primary@example.com")
	assert.NotContains(t, err.Error(), "--readonly")
}

// fakeTokenRemover records the order of revoke/delete calls so the tests can
// pin the narrowing contract: revocation happens first and its failure stops
// the deletion.
type fakeTokenRemover struct {
	hasToken  bool
	revokeErr error
	calls     []string
}

func (f *fakeTokenRemover) HasToken(string) bool { return f.hasToken }

func (f *fakeTokenRemover) RevokeToken(context.Context, string) error {
	f.calls = append(f.calls, "revoke")
	return f.revokeErr
}

func (f *fakeTokenRemover) DeleteToken(string) error {
	f.calls = append(f.calls, "delete")
	return nil
}

// TestRemoveAddAccountTokenForReauth pins why narrowing must revoke at
// Google and not merely delete the local file: the refresh token inside that
// file stays valid server-side, so any copy of it keeps write access while
// the command reports a successful narrowing.
func TestRemoveAddAccountTokenForReauth(t *testing.T) {
	tests := []struct {
		name        string
		remover     *fakeTokenRemover
		readonly    bool
		wantErr     string
		wantCalls   []string
		description string
	}{
		{
			name:        "narrowing revokes the grant before deleting the token",
			remover:     &fakeTokenRemover{hasToken: true},
			readonly:    true,
			wantCalls:   []string{"revoke", "delete"},
			description: "revocation must precede deletion — a deleted file can no longer be revoked",
		},
		{
			name:        "narrowing fails closed when revocation fails",
			remover:     &fakeTokenRemover{hasToken: true, revokeErr: errors.New("endpoint unreachable")},
			readonly:    true,
			wantErr:     "cannot narrow",
			wantCalls:   []string{"revoke"},
			description: "the token must survive so the run can be retried",
		},
		{
			name:        "default force run deletes without revoking",
			remover:     &fakeTokenRemover{hasToken: true},
			readonly:    false,
			wantCalls:   []string{"delete"},
			description: "plain --force keeps its existing delete-only behavior",
		},
		{
			name:        "no token means nothing to revoke or delete",
			remover:     &fakeTokenRemover{hasToken: false},
			readonly:    true,
			wantCalls:   nil,
			description: "a fresh --readonly --force run proceeds straight to authorization",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			err := removeAddAccountTokenForReauth(
				context.Background(), tt.remover, "user@example.com", tt.readonly)

			if tt.wantErr != "" {
				require.Error(err)
				assert.Contains(err.Error(), tt.wantErr)
			} else {
				require.NoError(err)
			}
			assert.Equal(tt.wantCalls, tt.remover.calls, tt.description)
		})
	}
}
