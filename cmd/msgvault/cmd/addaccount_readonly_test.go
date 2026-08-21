package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
			// Write scopes msgvault never requests still have to be stripped;
			// a grant is read-only when nothing in it can write.
			name:             "narrowing drops write scopes msgvault never requests",
			hasScopeMetadata: true,
			existing: []string{
				oauth.ScopeGmailReadonly,
				oauth.ScopeGmailSend,
				oauth.ScopeGmailCompose,
				oauth.ScopeGmailInsert,
				oauth.ScopeGmailLabels,
				oauth.ScopeGmailSettingsBasic,
				oauth.ScopeGmailSettingsSharing,
				oauth.ScopeCalendarReadonly,
			},
			readonly: true,
			want:     []string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly},
		},
		{
			// gmail.metadata is read-only and must survive narrowing.
			name:             "narrowing keeps the read-only metadata scope",
			hasScopeMetadata: true,
			existing:         []string{oauth.ScopeGmailMetadata, oauth.ScopeGmailModify},
			readonly:         true,
			want:             []string{oauth.ScopeGmailMetadata, oauth.ScopeGmailReadonly},
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
		freshClient      bool
		wantErr          bool
		wantErrContains  string
		wantWarn         bool
	}{
		{
			name:             "refusal names the revoke-and-re-add procedure",
			hasToken:         true,
			hasScopeMetadata: true,
			granted:          oauth.Scopes,
			readonly:         true,
			wantErr:          true,
			wantErrContains:  "myaccount.google.com/permissions",
		},
		{
			// gmail.send is write-capable but never requested by msgvault, so
			// a set that stops at modify and full would wave it through.
			name:             "readonly over a send-only grant is refused",
			hasToken:         true,
			hasScopeMetadata: true,
			granted:          []string{oauth.ScopeGmailReadonly, oauth.ScopeGmailSend},
			readonly:         true,
			wantErr:          true,
			wantErrContains:  oauth.ScopeGmailSend,
		},
		{
			name:             "readonly over a settings grant is refused",
			hasToken:         true,
			hasScopeMetadata: true,
			granted:          []string{oauth.ScopeGmailReadonly, oauth.ScopeGmailSettingsBasic},
			readonly:         true,
			wantErr:          true,
			wantErrContains:  oauth.ScopeGmailSettingsBasic,
		},
		{
			// Metadata is read-only, so this grant is genuinely narrow and
			// must not be refused.
			name:             "readonly over a metadata grant is a no-op",
			hasToken:         true,
			hasScopeMetadata: true,
			granted:          []string{oauth.ScopeGmailReadonly, oauth.ScopeGmailMetadata},
			readonly:         true,
		},
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
			wantErrContains:  "cannot be narrowed",
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
			// A genuinely different OAuth client has no prior grant, so this
			// is a first authorization rather than narrowing in place. The
			// caller sets this only when the token's recorded client_id
			// differs — unknown provenance does not qualify.
			name:             "readonly proceeds for a genuinely different oauth client",
			hasToken:         true,
			hasScopeMetadata: true,
			granted:          oauth.Scopes,
			readonly:         true,
			freshClient:      true,
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
				email, tt.hasToken, tt.hasScopeMetadata, tt.granted, tt.readonly,
				tt.freshClient, "/tmp/tokens/user@example.com.json")

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
	assert.Contains(err.Error(), "myaccount.google.com/permissions")
	assert.NotContains(out, "Starting browser authorization")

	after, readErr := os.ReadFile(tokenPath)
	require.NoError(readErr)
	assert.Equal(before, after, "refusal must not touch the existing token")
}

// TestAddAccount_ReadonlyForceIsStillRefused is the regression for the change
// that matters most here. --force used to waive the refusal and re-authorize
// narrow, which measured out badly against a live account: msgvault's token
// narrowed, but a refresh token issued beforehand kept returning write-scoped
// access tokens, and revoking it invalidated the replacement too. There is no
// flag combination that narrows access already granted, so --force must not
// look like one.
func TestAddAccount_ReadonlyForceIsStillRefused(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	saveAddAccountFlags(t)
	tokenPath, restore := seedTokenEnv(t, gmailOnlyTokenJSON)
	defer restore()

	before, err := os.ReadFile(tokenPath)
	require.NoError(err)

	out, err := runAddAccountForTest(t,
		scopeEscalationAccount, "--readonly", "--force", "--no-default-identity")

	require.Error(err)
	assert.Contains(err.Error(), "already has Gmail write access")
	assert.Contains(err.Error(), "myaccount.google.com/permissions")
	assert.NotContains(out, "Removing existing token")
	assert.NotContains(out, "Starting browser authorization")

	after, readErr := os.ReadFile(tokenPath)
	require.NoError(readErr)
	assert.Equal(before, after,
		"the refusal must run before --force deletes anything")
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
	assert.Contains(err.Error(), "myaccount.google.com/permissions")
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

func TestAddAccount_AuthenticatedPreflightRegistersWiderGrant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	saveAddAccountFlags(t)
	_, restore := seedTokenEnv(t, gmailOnlyTokenJSON)
	defer restore()
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))

	out, err := runAddAccountForTest(t,
		scopeEscalationAccount, "--readonly", "--grant-decided", "--no-default-identity")

	require.NoError(err)
	assert.Contains(out, "already authorized")
	assert.NotContains(out, "Starting browser authorization")
}

func TestAddAccount_ImplicitDefaultRejectsKnownDifferentClient(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	saveAddAccountFlags(t)
	otherClientToken := strings.Replace(
		gmailReadonlyTokenJSON,
		"test.apps.googleusercontent.com",
		"other.apps.googleusercontent.com",
		1,
	)
	_, restore := seedTokenEnv(t, otherClientToken)
	defer restore()

	out, err := runAddAccountForTest(t,
		scopeEscalationAccount, "--readonly", "--no-default-identity")

	require.ErrorIs(err, context.Canceled)
	assert.Contains(out, "Starting browser authorization")
	assert.NotContains(out, "already authorized")
}

// TestAddAccount_HeadlessAppliesGrantDecision is the regression for a bypass:
// the --headless branch returned before the grant decision ran, so it printed
// instructions for widening a narrowed account with no warning, and accepted
// --readonly against a write-capable account that would be refused anywhere
// else. --headless only prints, but what it prints is what the operator then
// runs on a machine with a browser.
func TestAddAccount_HeadlessAppliesGrantDecision(t *testing.T) {
	t.Run("refuses readonly against a write-capable account", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		saveAddAccountFlags(t)
		_, restore := seedTokenEnv(t, gmailOnlyTokenJSON)
		defer restore()

		out, err := runAddAccountForTest(t,
			scopeEscalationAccount, "--headless", "--readonly", "--no-default-identity")

		require.Error(err)
		assert.Contains(err.Error(), "already has Gmail write access")
		// The remedy is identical on every host: revoke and re-create.
		assert.Contains(err.Error(), "myaccount.google.com/permissions")
		assert.NotContains(out, "Headless Server Setup")
	})

	t.Run("warns before printing instructions that would widen", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		saveAddAccountFlags(t)
		_, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
		defer restore()

		out, err := runAddAccountForTest(t,
			scopeEscalationAccount, "--headless", "--no-default-identity")

		require.NoError(err)
		assert.Contains(out, "currently has read-only Gmail access")
		assert.Contains(out, "Headless Server Setup")
	})

	t.Run("brand new account prints instructions with no warning", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		saveAddAccountFlags(t)
		_, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
		defer restore()

		out, err := runAddAccountForTest(t,
			"fresh@example.com", "--headless", "--readonly", "--no-default-identity")

		require.NoError(err)
		assert.Contains(out, "Headless Server Setup")
		assert.Contains(out, "--readonly")
		assert.NotContains(out, "Warning")
	})
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

// TestReadonlyGrantWarning covers the post-authorization check. Requesting
// read-only does not guarantee receiving it — the authorization server decides
// what to issue — so the recorded grant is read back rather than assumed.
//
// It warns rather than failing: the token is already written, and aborting
// would skip source registration, leaving a token on disk that no command can
// see while the obvious retry is refused for holding write access.
func TestReadonlyGrantWarning(t *testing.T) {
	tests := []struct {
		name      string
		tokenJSON string
		wantWarn  bool
		wantNamed string
	}{
		{
			name:      "a genuinely narrow grant is silent",
			tokenJSON: gmailReadonlyCalendarTokenJSON,
		},
		{
			name:      "a returned modify scope is reported",
			tokenJSON: gmailOnlyTokenJSON,
			wantWarn:  true,
			wantNamed: oauth.ScopeGmailModify,
		},
		{
			name:      "a returned full-access scope is reported",
			tokenJSON: gmailFullOnlyTokenJSON,
			wantWarn:  true,
			wantNamed: oauth.ScopeGmailFull,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			_, restore := seedTokenEnv(t, tt.tokenJSON)
			defer restore()

			mgr, err := oauth.NewManager(cfg.OAuth.ClientSecrets, cfg.TokensDir(), logger)
			require.NoError(err)

			got := readonlyGrantWarning(mgr, scopeEscalationAccount)

			if !tt.wantWarn {
				assert.Empty(got)
				return
			}
			require.NotEmpty(got)
			assert.Contains(got, tt.wantNamed)
			assert.Contains(got, "myaccount.google.com/permissions")
		})
	}
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

// TestAddAccount_ReadonlyRefusesAliasOfStoredToken: authorization accepts
// Gmail alias spellings as the same account, so a --readonly run under a
// dot-variant of a stored token must refuse rather than read as a fresh
// account while the stored spelling's credential keeps its access. When both
// spellings hold tokens, the refusal names the revoke-and-re-add procedure
// instead of bouncing between the two spellings.
func TestAddAccount_ReadonlyRefusesAliasOfStoredToken(t *testing.T) {
	seedAliasToken := func(t *testing.T, tokenJSON string) {
		t.Helper()
		require.NoError(t, os.WriteFile(
			filepath.Join(cfg.TokensDir(), "username@gmail.com.json"),
			[]byte(tokenJSON), 0600))
	}

	t.Run("alias of a stored token points at the stored spelling", func(t *testing.T) {
		saveAddAccountFlags(t)
		_, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
		defer restore()
		seedAliasToken(t, gmailOnlyTokenJSON)

		out, err := runAddAccountForTest(t, "user.name@gmail.com", "--readonly", "--no-default-identity")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "username@gmail.com")
		assert.NotContains(t, out, "Starting browser authorization")
	})

	t.Run("duplicate spellings name the revoke-and-re-add procedure", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		saveAddAccountFlags(t)
		_, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
		defer restore()
		seedAliasToken(t, gmailOnlyTokenJSON)
		require.NoError(os.WriteFile(
			filepath.Join(cfg.TokensDir(), "user.name@gmail.com.json"),
			[]byte(gmailReadonlyTokenJSON), 0600))

		out, err := runAddAccountForTest(t, "user.name@gmail.com", "--readonly", "--no-default-identity")

		require.Error(err)
		assert.Contains(err.Error(), "myaccount.google.com/permissions")
		assert.Contains(err.Error(), "username@gmail.com")
		assert.NotContains(out, "Starting browser authorization")
	})

	t.Run("default run ignores alias spellings", func(t *testing.T) {
		saveAddAccountFlags(t)
		_, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
		defer restore()
		seedAliasToken(t, gmailOnlyTokenJSON)

		_, err := runAddAccountForTest(t, "user.name@gmail.com", "--no-default-identity")

		require.Error(t, err, "browser authorization cannot complete on a cancelled context")
		assert.NotContains(t, err.Error(), "username@gmail.com")
	})

	t.Run("known different OAuth client is an independent grant", func(t *testing.T) {
		saveAddAccountFlags(t)
		_, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
		defer restore()
		differentClientToken := strings.Replace(
			gmailOnlyTokenJSON,
			`"client_id": "test.apps.googleusercontent.com"`,
			`"client_id": "other.apps.googleusercontent.com"`,
			1,
		)
		seedAliasToken(t, differentClientToken)

		out, err := runAddAccountForTest(t, "user.name@gmail.com", "--readonly", "--no-default-identity")

		require.Error(t, err, "browser authorization cannot complete on a cancelled context")
		assert.Contains(t, out, "Starting browser authorization")
		assert.NotContains(t, err.Error(), "username@gmail.com")
	})

	t.Run("unknown OAuth client remains conservative", func(t *testing.T) {
		saveAddAccountFlags(t)
		_, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
		defer restore()
		seedAliasToken(t, legacyTokenJSON)

		out, err := runAddAccountForTest(t, "user.name@gmail.com", "--readonly", "--no-default-identity")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "username@gmail.com")
		assert.NotContains(t, out, "Starting browser authorization")
	})

	t.Run("remediation preserves named OAuth app", func(t *testing.T) {
		saveAddAccountFlags(t)
		_, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
		defer restore()
		seedAliasToken(t, gmailOnlyTokenJSON)
		mgr, err := oauth.NewManager(cfg.OAuth.ClientSecrets, cfg.TokensDir(), logger)
		require.NoError(t, err)

		err = refuseReadonlyUnderAliasSpelling(mgr, "user.name@gmail.com", "workspace app")

		require.Error(t, err)
		assert.Contains(t, err.Error(),
			"msgvault add-account username@gmail.com --oauth-app 'workspace app' --readonly")
	})
}
