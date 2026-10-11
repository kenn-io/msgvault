package microsoft

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestTokenPath(t *testing.T) {
	dir := filepath.Join("tmp", "tokens")
	m := &Manager{tokensDir: dir}
	path := m.TokenPath("user@example.com")
	want := filepath.Join(dir, "microsoft_user@example.com.json")
	assert.Equal(t, want, path, "TokenPath")
}

func TestSaveAndLoadToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	m := &Manager{tokensDir: dir}
	token := &oauth2.Token{
		AccessToken:  "access-123",
		RefreshToken: "refresh-456",
		TokenType:    "Bearer",
	}
	scopes := []string{"IMAP.AccessAsUser.All", "offline_access"}

	require.NoError(m.saveToken("user@example.com", token, scopes, ""))

	loaded, err := m.loadTokenFile("user@example.com")
	require.NoError(err)
	assert.Equal("access-123", loaded.AccessToken, "AccessToken")
	assert.Equal("refresh-456", loaded.RefreshToken, "RefreshToken")
	assert.Len(loaded.Scopes, 2, "Scopes len")

	// Verify file permissions (Unix only; Windows ignores POSIX bits).
	if runtime.GOOS != "windows" {
		path := m.TokenPath("user@example.com")
		info, err := os.Stat(path)
		require.NoError(err)
		assert.Equal(os.FileMode(0600), info.Mode().Perm(), "permissions")
	}
}

func TestHasToken(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{tokensDir: dir}

	assert.False(t, m.HasToken("nobody@example.com"), "HasToken should be false for non-existent token")

	token := &oauth2.Token{AccessToken: "test"}
	require.NoError(t, m.saveToken("user@example.com", token, nil, ""))
	assert.True(t, m.HasToken("user@example.com"), "HasToken should be true after save")
}

func TestDeleteToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	m := &Manager{tokensDir: dir}

	token := &oauth2.Token{AccessToken: "test"}
	require.NoError(m.saveToken("user@example.com", token, nil, ""))
	require.NoError(m.DeleteToken("user@example.com"))
	assert.False(m.HasToken("user@example.com"), "HasToken should be false after delete")
	// Delete non-existent should not error
	assert.NoError(m.DeleteToken("nobody@example.com"), "DeleteToken non-existent")
}

func TestIsPersonalMicrosoftAccount(t *testing.T) {
	tests := []struct {
		email    string
		personal bool
	}{
		{"user@hotmail.com", true},
		{"user@outlook.com", true},
		{"user@live.com", true},
		{"user@msn.com", true},
		{"user@hotmail.co.uk", true},
		{"user@hotmail.co.jp", true},
		{"user@hotmail.com.au", true},
		{"user@hotmail.com.br", true},
		{"user@outlook.jp", true},
		{"user@outlook.kr", true},
		{"user@outlook.com.br", true},
		{"user@outlook.com.au", true},
		{"user@live.com.au", true},
		{"user@live.jp", true},
		{"user@company.com", false},
		{"user@5.life", false},
		{"user@gmail.com", false},
	}
	for _, tt := range tests {
		got := isPersonalMicrosoftAccount(tt.email)
		assert.Equal(t, tt.personal, got, "isPersonalMicrosoftAccount(%q)", tt.email)
	}
}

func TestScopesForEmail(t *testing.T) {
	assert := assert.New(t)
	orgScopes := scopesForEmail("user@company.com")
	assert.Equal(ScopeIMAPOrg, orgScopes[0], "org scope")
	personalScopes := scopesForEmail("user@hotmail.com")
	assert.Equal(ScopeIMAPPersonal, personalScopes[0], "personal scope")
	assert.Contains(orgScopes, scopeProfile)
	assert.Contains(personalScopes, scopeProfile)
}

func TestSanitizeEmail(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"user@example.com", "user@example.com"},
		// / replaced first, then .. → _.._; double underscore before "evil"
		// because _.._  ends with _ and the original _evil starts with _.
		{"../evil", "_..__evil"},
		{"a/b", "a_b"},
		{"a\\b", "a_b"},
		// double dot in domain — sanitized in place
		{"user@sub..domain.com", "user@sub_.._domain.com"},
		// null byte replaced before other transforms
		{"user\x00@evil.com", "user_@evil.com"},
	}
	for _, tt := range tests {
		got := sanitizeEmail(tt.input)
		assert.Equal(t, tt.want, got, "sanitizeEmail(%q)", tt.input)
	}
}

func TestSanitizeEmail_NoPathTraversal(t *testing.T) {
	// None of these should produce a string containing a path separator or
	// result in a different filepath.Base (i.e. no directory component).
	inputs := []string{
		"../../etc/passwd",
		"../tokens/admin@example.com",
		"/etc/passwd",
		"C:\\Windows\\system32",
		"user@sub..domain.com",
		"....@example.com",
		"user\x00@evil.com",
	}
	for _, input := range inputs {
		result := sanitizeEmail(input)
		assert.False(t, strings.ContainsAny(result, "/\\"),
			"sanitizeEmail(%q) = %q still contains path separator", input, result)
		assert.Equal(t, filepath.Base(result), result,
			"sanitizeEmail(%q) = %q has directory component (filepath.Base differs)", input, result)
	}
}

// makeIDToken builds a minimal unsigned JWT with the given claims.
// Used in tests with verifyIDTokenFn to bypass OIDC signature validation.
func makeIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	require.NoError(t, err, "marshal claims")
	body := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + body + ".fake-sig"
}

func makeScopedIDToken(t *testing.T, claims map[string]any, scopes []string) string {
	t.Helper()
	requested := make(map[string]any, len(claims))
	for key, value := range claims {
		if key == "preferred_username" && !slices.Contains(scopes, scopeProfile) {
			continue
		}
		if key == "email" && !slices.Contains(scopes, scopeEmail) {
			continue
		}
		requested[key] = value
	}
	return makeIDToken(t, requested)
}

// testVerifyFn decodes an unsigned test JWT, bypassing OIDC validation.
// Only for use in tests via Manager.verifyIDTokenFn.
func testVerifyFn(_ context.Context, rawIDToken string) (*idTokenClaims, error) {
	parts := splitJWT(rawIDToken)
	if len(parts) != 3 {
		return nil, errors.New("invalid test JWT: expected 3 parts")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode JWT payload: %w", err)
	}
	var raw struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		TenantID          string `json:"tid"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, err
	}
	return &idTokenClaims{
		Email:             raw.Email,
		PreferredUsername: raw.PreferredUsername,
		TenantID:          raw.TenantID,
	}, nil
}

// splitJWT is a test helper to avoid importing strings just for Split.
func splitJWT(s string) []string {
	var parts []string
	for {
		idx := -1
		for i := range len(s) {
			if s[i] == '.' {
				idx = i
				break
			}
		}
		if idx < 0 {
			parts = append(parts, s)
			break
		}
		parts = append(parts, s[:idx])
		s = s[idx+1:]
	}
	return parts
}

func TestPeekTIDFromJWT(t *testing.T) {
	idToken := makeIDToken(t, map[string]any{
		"email":              "user@example.com",
		"preferred_username": "user@tenant.onmicrosoft.com",
		"tid":                "some-tenant-id",
	})
	tid, err := peekTIDFromJWT(idToken)
	require.NoError(t, err)
	assert.Equal(t, "some-tenant-id", tid, "tid")
}

func TestImapScopeForTenant(t *testing.T) {
	assert.Equal(t, ScopeIMAPPersonal, imapScopeForTenant(MicrosoftConsumerTenantID), "consumer tenant")
	assert.Equal(t, ScopeIMAPOrg, imapScopeForTenant("some-org-tenant-id"), "org tenant")
}

func TestResolveTokenEmail_Match(t *testing.T) {
	m := &Manager{
		clientID:        "test-client",
		tenantID:        "common",
		tokensDir:       t.TempDir(),
		logger:          slog.Default(),
		verifyIDTokenFn: testVerifyFn,
	}
	idToken := makeIDToken(t, map[string]any{"email": "user@example.com", "tid": "org-tid"})
	token := (&oauth2.Token{AccessToken: "test-token", TokenType: "Bearer"}).
		WithExtra(map[string]any{"id_token": idToken})

	actual, claims, err := m.resolveTokenEmail(t.Context(), "user@example.com", token, "test-nonce")
	require.NoError(t, err)
	assert.Equal(t, "user@example.com", actual, "actual")
	assert.Equal(t, "org-tid", claims.TenantID, "TenantID")
}

func TestResolveTokenEmail_Mismatch(t *testing.T) {
	m := &Manager{
		clientID:        "test-client",
		tenantID:        "common",
		tokensDir:       t.TempDir(),
		verifyIDTokenFn: testVerifyFn,
	}
	idToken := makeIDToken(t, map[string]any{"email": "other@example.com"})
	token := (&oauth2.Token{AccessToken: "test-token", TokenType: "Bearer"}).
		WithExtra(map[string]any{"id_token": idToken})

	_, _, err := m.resolveTokenEmail(t.Context(), "user@example.com", token, "test-nonce")
	require.Error(t, err, "expected error for mismatch")
	tokenMismatchError := &TokenMismatchError{}
	ok := errors.As(err, &tokenMismatchError)
	assert.True(t, ok, "expected *TokenMismatchError, got %T: %v", err, err)
}

func TestResolveTokenEmail_UPNFallback(t *testing.T) {
	// With no "email" claim, the UPN must match the mailbox or the sign-in name.
	for _, tc := range []struct {
		name, upn, signIn, mismatch string
	}{
		{name: "upn equals address", upn: "john@example.com"},
		{name: "upn differs, no sign-in name", upn: "john.doe@example.org", mismatch: "john.doe@example.org"},
		{name: "upn matches sign-in name ignoring case", upn: "JDoe@example.org", signIn: "jdoe@example.org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			m := &Manager{clientID: "test-client", tenantID: "common", tokensDir: t.TempDir(), logger: slog.Default(), verifyIDTokenFn: testVerifyFn}
			m.UseSignInName(tc.signIn)
			idToken := makeIDToken(t, map[string]any{"preferred_username": tc.upn, "tid": "org-tenant-id"})
			token := (&oauth2.Token{AccessToken: "test-token", TokenType: "Bearer"}).
				WithExtra(map[string]any{"id_token": idToken})

			actual, _, err := m.resolveTokenEmail(t.Context(), "john@example.com", token, "test-nonce")
			if tc.mismatch != "" {
				mismatch := &TokenMismatchError{}
				require.ErrorAs(err, &mismatch)
				assert.Equal(tc.mismatch, mismatch.Actual, "Actual")
				return
			}
			require.NoError(err)
			assert.Equal("john@example.com", actual, "actual")
		})
	}
}

// upnBrowserFlow returns a token whose ID token has upn and no email claim,
// recording the login hint it was given when hint is non-nil.
func upnBrowserFlow(t *testing.T, upn string, hint *string) func(context.Context, string, []string) (*oauth2.Token, string, error) {
	t.Helper()
	return func(_ context.Context, h string, scopes []string) (*oauth2.Token, string, error) {
		if hint != nil {
			*hint = h
		}
		idToken := makeScopedIDToken(t, map[string]any{"preferred_username": upn, "tid": "org-tenant-id"}, scopes)
		tok := (&oauth2.Token{AccessToken: "new-access", RefreshToken: "new-refresh", TokenType: "Bearer"}).
			WithExtra(map[string]any{"id_token": idToken})
		return tok, "test-nonce", nil
	}
}

func TestAuthorize_OtherUPNKeepsExistingToken(t *testing.T) {
	for _, tc := range []struct {
		name, signIn, hint string
	}{
		{name: "no sign-in name", hint: "sign in as alice@example.com, or if bob@example.com is your own sign-in name for that mailbox, pass it with --sign-in"},
		{name: "other sign-in name", signIn: "alice@example.org", hint: "that account is not the --sign-in name alice@example.org either"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			m := &Manager{clientID: "test-client", tenantID: "common", tokensDir: t.TempDir(), logger: slog.Default(), verifyIDTokenFn: testVerifyFn}
			m.UseSignInName(tc.signIn)
			old := &oauth2.Token{AccessToken: "old-access", RefreshToken: "old-refresh", TokenType: "Bearer"}
			require.NoError(m.saveToken("alice@example.com", old, scopesForEmail("alice@example.com"), "org-tenant-id"))
			m.browserFlowFn = upnBrowserFlow(t, "bob@example.com", nil)

			err := m.Authorize(t.Context(), "alice@example.com")
			mismatch := &TokenMismatchError{}
			require.ErrorAs(err, &mismatch)
			assert.Contains(err.Error(), tc.hint)
			tf, err := m.loadTokenFile("alice@example.com")
			require.NoError(err)
			assert.Equal("old-access", tf.AccessToken, "existing token untouched")
		})
	}
}

func TestAuthorize_SignInNameAcceptsUPN(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	m := &Manager{clientID: "test-client", tenantID: "common", tokensDir: t.TempDir(), logger: slog.Default(), verifyIDTokenFn: testVerifyFn}
	m.UseSignInName("jdoe@example.org")
	var hint string
	m.browserFlowFn = upnBrowserFlow(t, "jdoe@example.org", &hint)

	require.NoError(m.Authorize(t.Context(), "john@example.com"))
	assert.Equal("jdoe@example.org", hint, "login hint")
	tf, err := m.loadTokenFile("john@example.com")
	require.NoError(err)
	assert.Equal("new-access", tf.AccessToken, "AccessToken")
}

func TestResolveTokenEmail_EmailClaimMismatchStillErrors(t *testing.T) {
	// When the authoritative "email" claim IS present but doesn't match,
	// it should still error (user authenticated the wrong account).
	m := &Manager{
		clientID:        "test-client",
		tenantID:        "common",
		tokensDir:       t.TempDir(),
		verifyIDTokenFn: testVerifyFn,
	}
	idToken := makeIDToken(t, map[string]any{
		"email":              "wrong@other.com",
		"preferred_username": "john@company.com",
	})
	token := (&oauth2.Token{AccessToken: "test-token", TokenType: "Bearer"}).
		WithExtra(map[string]any{"id_token": idToken})

	_, _, err := m.resolveTokenEmail(t.Context(), "john@company.com", token, "test-nonce")
	require.Error(t, err, "expected TokenMismatchError when email claim is wrong")
	tokenMismatchError := &TokenMismatchError{}
	ok := errors.As(err, &tokenMismatchError)
	assert.True(t, ok, "expected *TokenMismatchError, got %T: %v", err, err)
}

func TestAuthorize_ScopeCorrection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// A custom domain is guessed as organizational, but the tenant is personal.
	// The browser flow should be called twice: once with org scope, once with personal.
	dir := t.TempDir()
	m := &Manager{
		clientID:        "test-client",
		tenantID:        "common",
		tokensDir:       dir,
		logger:          slog.Default(),
		verifyIDTokenFn: testVerifyFn,
	}

	consumerTID := MicrosoftConsumerTenantID
	callCount := 0

	m.browserFlowFn = func(ctx context.Context, email string, scopes []string) (*oauth2.Token, string, error) {
		callCount++
		idToken := makeScopedIDToken(t, map[string]any{
			"preferred_username": "user@example.com",
			"tid":                consumerTID,
		}, scopes)
		switch callCount {
		case 1:
			// First call: should have org scope (domain-based guess).
			assert.Equal(ScopeIMAPOrg, scopes[0], "first call scope")
		case 2:
			// Second call: should have personal scope (corrected via tid).
			assert.Equal(ScopeIMAPPersonal, scopes[0], "second call scope")
		}
		tok := (&oauth2.Token{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			TokenType:    "Bearer",
		}).WithExtra(map[string]any{"id_token": idToken})
		return tok, "test-nonce", nil
	}

	require.NoError(m.Authorize(t.Context(), "user@example.com"))

	assert.Equal(2, callCount, "browserFlowFn call count")

	// Verify saved scopes are personal (corrected).
	tf, err := m.loadTokenFile("user@example.com")
	require.NoError(err)
	require.NotEmpty(tf.Scopes, "saved scopes should not be empty")
	assert.Equal(ScopeIMAPPersonal, tf.Scopes[0], "saved scopes[0]")
}

func TestAuthorize_NoScopeCorrection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// When the domain guess matches tid, no correction should happen.
	// user@outlook.com → guessed personal, tid confirms consumer → no correction.
	dir := t.TempDir()
	m := &Manager{
		clientID:        "test-client",
		tenantID:        "common",
		tokensDir:       dir,
		logger:          slog.Default(),
		verifyIDTokenFn: testVerifyFn,
	}

	consumerTID := MicrosoftConsumerTenantID
	callCount := 0

	m.browserFlowFn = func(ctx context.Context, email string, scopes []string) (*oauth2.Token, string, error) {
		callCount++
		// Should already have personal scope.
		assert.Equal(ScopeIMAPPersonal, scopes[0], "initial scope")
		idToken := makeIDToken(t, map[string]any{
			"email": "user@outlook.com",
			"tid":   consumerTID,
		})
		tok := (&oauth2.Token{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			TokenType:    "Bearer",
		}).WithExtra(map[string]any{"id_token": idToken})
		return tok, "test-nonce", nil
	}

	require.NoError(m.Authorize(t.Context(), "user@outlook.com"))

	assert.Equal(1, callCount, "browserFlowFn call count (no correction needed)")

	// Verify saved scopes are personal (no correction needed).
	tf, err := m.loadTokenFile("user@outlook.com")
	require.NoError(err)
	require.NotEmpty(tf.Scopes, "saved scopes should not be empty")
	assert.Equal(ScopeIMAPPersonal, tf.Scopes[0], "saved scopes[0]")
}

func TestAuthorize_PersistsTenantID(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{
		clientID:        "test-client",
		tenantID:        "common",
		tokensDir:       dir,
		logger:          slog.Default(),
		verifyIDTokenFn: testVerifyFn,
	}

	m.browserFlowFn = func(ctx context.Context, email string, scopes []string) (*oauth2.Token, string, error) {
		idToken := makeIDToken(t, map[string]any{
			"email": "user@company.com",
			"tid":   "org-tenant-123",
		})
		tok := (&oauth2.Token{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			TokenType:    "Bearer",
		}).WithExtra(map[string]any{"id_token": idToken})
		return tok, "test-nonce", nil
	}

	require.NoError(t, m.Authorize(t.Context(), "user@company.com"))

	tf, err := m.loadTokenFile("user@company.com")
	require.NoError(t, err)
	assert.Equal(t, "org-tenant-123", tf.TenantID, "TenantID")
}

func TestTokenSource_StaleScopeReturnsError(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{
		clientID:  "test-client",
		tenantID:  "common",
		tokensDir: dir,
		logger:    slog.Default(),
	}

	// Save a token with org IMAP scope but consumer tenant ID (stale).
	token := &oauth2.Token{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
	}
	require.NoError(t, m.saveToken("user@custom.com", token, []string{ScopeIMAPOrg, "offline_access"}, MicrosoftConsumerTenantID))

	_, err := m.TokenSource(t.Context(), "user@custom.com")
	require.Error(t, err, "expected error for stale scope")
	assert.ErrorContains(t, err, "stale IMAP scope")
}

func TestTokenSource_CorrectScopeSucceeds(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{
		clientID:  "test-client",
		tenantID:  "common",
		tokensDir: dir,
		logger:    slog.Default(),
	}

	// Save a token with correct personal IMAP scope and consumer tenant ID.
	token := &oauth2.Token{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
	}
	require.NoError(t, m.saveToken("user@outlook.com", token, []string{ScopeIMAPPersonal, "offline_access"}, MicrosoftConsumerTenantID))

	ts, err := m.TokenSource(t.Context(), "user@outlook.com")
	require.NoError(t, err)
	require.NotNil(t, ts, "TokenSource returned nil")
}

func TestTokenSource_NoTenantIDSkipsValidation(t *testing.T) {
	// Pre-migration tokens without tenant_id should still work.
	dir := t.TempDir()
	m := &Manager{
		clientID:  "test-client",
		tenantID:  "common",
		tokensDir: dir,
		logger:    slog.Default(),
	}

	token := &oauth2.Token{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
	}
	require.NoError(t, m.saveToken("user@custom.com", token, []string{ScopeIMAPOrg, "offline_access"}, ""))

	ts, err := m.TokenSource(t.Context(), "user@custom.com")
	require.NoError(t, err)
	require.NotNil(t, ts, "TokenSource returned nil")
}

func TestOAuthConfigWithTenant(t *testing.T) {
	m := &Manager{
		clientID: "test-client",
		tenantID: "common",
	}
	cfg := m.oauthConfigWithTenant("my-org", []string{"IMAP.AccessAsUser.All"})
	assert.Contains(t, cfg.Endpoint.AuthURL, "my-org", "AuthURL")
	assert.Contains(t, cfg.Endpoint.TokenURL, "my-org", "TokenURL")
	assert.NotContains(t, cfg.Endpoint.AuthURL, "common", "AuthURL should not contain common")
}

func TestTokenSource_PersistedTenantOverridesManager(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{
		clientID:  "test-client",
		tenantID:  "common",
		tokensDir: dir,
		logger:    slog.Default(),
	}

	// Save token with a specific tenant ID (simulating post-authorization state).
	token := &oauth2.Token{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
	}
	require.NoError(t, m.saveToken("user@company.com", token, []string{ScopeIMAPOrg, "offline_access"}, "my-org-tenant"))

	// TokenSource should succeed and use "my-org-tenant", not "common".
	ts, err := m.TokenSource(t.Context(), "user@company.com")
	require.NoError(t, err)
	require.NotNil(t, ts, "TokenSource returned nil")
}

func TestTokenSource_ConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{
		clientID:  "test-client",
		tenantID:  "common",
		tokensDir: dir,
		logger:    slog.Default(),
	}

	token := &oauth2.Token{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
	}
	require.NoError(t, m.saveToken("user@outlook.com", token, []string{ScopeIMAPPersonal, "offline_access"}, MicrosoftConsumerTenantID))

	fn, err := m.TokenSource(t.Context(), "user@outlook.com")
	require.NoError(t, err)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, _ = fn(t.Context())
		})
	}
	wg.Wait()
}

// --- IMAPHost ---

func TestIMAPHost_PersonalAccount(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{tokensDir: dir}
	token := &oauth2.Token{AccessToken: "access", RefreshToken: "refresh"}
	require.NoError(t, m.saveToken("user@hotmail.com", token, []string{ScopeIMAPPersonal, "offline_access"}, MicrosoftConsumerTenantID))
	host, err := m.IMAPHost("user@hotmail.com")
	require.NoError(t, err)
	assert.Equal(t, "outlook.office.com", host, "IMAPHost")
}

func TestIMAPHost_OrgAccount(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{tokensDir: dir}
	token := &oauth2.Token{AccessToken: "access", RefreshToken: "refresh"}
	require.NoError(t, m.saveToken("user@company.com", token, []string{ScopeIMAPOrg, "offline_access"}, "org-tenant"))
	host, err := m.IMAPHost("user@company.com")
	require.NoError(t, err)
	assert.Equal(t, "outlook.office365.com", host, "IMAPHost")
}

func TestIMAPHost_NoToken(t *testing.T) {
	m := &Manager{tokensDir: t.TempDir()}
	_, err := m.IMAPHost("nobody@example.com")
	require.Error(t, err, "expected error for missing token")
}

// IMAPHost with no scopes saved falls back to org host (default).
func TestIMAPHost_NoScopesFallsBackToOrg(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{tokensDir: dir}
	token := &oauth2.Token{AccessToken: "access"}
	require.NoError(t, m.saveToken("user@company.com", token, nil, "org-tenant"))
	host, err := m.IMAPHost("user@company.com")
	require.NoError(t, err)
	assert.Equal(t, "outlook.office365.com", host, "IMAPHost")
}

// --- TokenSource edge cases ---

func TestTokenSource_MissingToken(t *testing.T) {
	m := &Manager{
		clientID:  "test-client",
		tenantID:  "common",
		tokensDir: t.TempDir(),
		logger:    slog.Default(),
	}
	_, err := m.TokenSource(t.Context(), "nobody@example.com")
	require.Error(t, err, "expected error for missing token")
	assert.ErrorContains(t, err, "no valid token")
}

// Pre-migration tokens without scopes fall back to email-based scope detection.
func TestTokenSource_EmptyScopesFallback(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{
		clientID:  "test-client",
		tenantID:  "common",
		tokensDir: dir,
		logger:    slog.Default(),
	}
	token := &oauth2.Token{AccessToken: "access", RefreshToken: "refresh", TokenType: "Bearer"}
	require.NoError(t, m.saveToken("user@outlook.com", token, nil, ""))
	ts, err := m.TokenSource(t.Context(), "user@outlook.com")
	require.NoError(t, err)
	require.NotNil(t, ts, "TokenSource returned nil")
}

// --- Authorize edge cases ---

func TestAuthorize_BrowserFlowError(t *testing.T) {
	m := &Manager{
		clientID:        "test-client",
		tenantID:        "common",
		tokensDir:       t.TempDir(),
		logger:          slog.Default(),
		verifyIDTokenFn: testVerifyFn,
	}
	wantErr := errors.New("user denied access")
	m.browserFlowFn = func(_ context.Context, _ string, _ []string) (*oauth2.Token, string, error) {
		return nil, "", wantErr
	}
	err := m.Authorize(t.Context(), "user@company.com")
	assert.ErrorIs(t, err, wantErr, "Authorize error")
}

func TestAuthorize_ContextCancelled(t *testing.T) {
	m := &Manager{
		clientID:        "test-client",
		tenantID:        "common",
		tokensDir:       t.TempDir(),
		logger:          slog.Default(),
		verifyIDTokenFn: testVerifyFn,
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	m.browserFlowFn = func(ctx context.Context, _ string, _ []string) (*oauth2.Token, string, error) {
		return nil, "", ctx.Err()
	}
	err := m.Authorize(ctx, "user@company.com")
	assert.ErrorIs(t, err, context.Canceled, "Authorize error")
}

// Scope correction triggers a second browser flow; a TokenMismatchError on
// that second flow should propagate rather than be swallowed.
func TestAuthorize_ScopeCorrectionMismatchOnReauth(t *testing.T) {
	m := &Manager{
		clientID:        "test-client",
		tenantID:        "common",
		tokensDir:       t.TempDir(),
		logger:          slog.Default(),
		verifyIDTokenFn: testVerifyFn,
	}
	callCount := 0
	m.browserFlowFn = func(_ context.Context, email string, _ []string) (*oauth2.Token, string, error) {
		callCount++
		var claimsEmail string
		if callCount == 1 {
			claimsEmail = email // first flow succeeds, tid triggers correction
		} else {
			claimsEmail = "someone-else@other.com" // second flow authenticates wrong account
		}
		idToken := makeIDToken(t, map[string]any{"email": claimsEmail, "tid": MicrosoftConsumerTenantID})
		tok := (&oauth2.Token{AccessToken: "tok", TokenType: "Bearer"}).
			WithExtra(map[string]any{"id_token": idToken})
		return tok, "nonce", nil
	}
	err := m.Authorize(t.Context(), "user@custom-domain.com")
	require.Error(t, err, "expected error when re-auth produces wrong email")
	var mismatch *TokenMismatchError
	assert.ErrorAs(t, err, &mismatch, "expected *TokenMismatchError, got %T: %v", err, err)
}

// --- resolveTokenEmail edge cases ---

func TestResolveTokenEmail_MissingIDToken(t *testing.T) {
	m := &Manager{
		clientID:        "test-client",
		tenantID:        "common",
		tokensDir:       t.TempDir(),
		verifyIDTokenFn: testVerifyFn,
	}
	token := &oauth2.Token{AccessToken: "test", TokenType: "Bearer"} // no id_token extra
	_, _, err := m.resolveTokenEmail(t.Context(), "user@example.com", token, "nonce")
	require.Error(t, err, "expected error for missing id_token")
	assert.ErrorContains(t, err, "no id_token")
}

func TestResolveTokenEmail_NeitherEmailNorUPN(t *testing.T) {
	m := &Manager{
		clientID:        "test-client",
		tenantID:        "common",
		tokensDir:       t.TempDir(),
		verifyIDTokenFn: testVerifyFn,
	}
	// ID token has only tid — no email or preferred_username.
	idToken := makeIDToken(t, map[string]any{"tid": "some-tenant"})
	token := (&oauth2.Token{AccessToken: "test", TokenType: "Bearer"}).
		WithExtra(map[string]any{"id_token": idToken})
	_, _, err := m.resolveTokenEmail(t.Context(), "user@example.com", token, "nonce")
	require.Error(t, err, "expected error when neither email nor preferred_username is present")
	assert.ErrorContains(t, err, "preferred_username")
}

// --- TokenSource timeout ---

func TestTokenSource_RespectsCallCtxCancellation(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{
		clientID:  "test-client",
		tenantID:  "common",
		tokensDir: dir,
		logger:    slog.Default(),
	}

	token := &oauth2.Token{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
	}
	require.NoError(t, m.saveToken("user@outlook.com", token, []string{ScopeIMAPPersonal, "offline_access"}, MicrosoftConsumerTenantID))

	fn, err := m.TokenSource(t.Context(), "user@outlook.com")
	require.NoError(t, err)

	// Cancel the context immediately — the token source should return
	// the cached token (not refreshed) or cancel, but must not hang.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// The call may succeed (cached token returned instantly before select
	// sees cancellation) or fail with context.Canceled — either is acceptable.
	// The key property is that it returns promptly, not that it errors.
	_, _ = fn(ctx)
}

// --- redactAuthURL ---

func TestRedactAuthURL(t *testing.T) {
	assert := assert.New(t)
	raw := "https://login.microsoftonline.com/common/oauth2/v2.0/authorize?" +
		"client_id=test&code_challenge=secret-challenge&code_challenge_method=S256&" +
		"login_hint=user%40example.com&nonce=secret-nonce&" +
		"redirect_uri=http%3A%2F%2Flocalhost%3A8089%2Fcallback%2Fmicrosoft&" +
		"response_type=code&scope=IMAP.AccessAsUser.All&state=secret-state"

	redacted := redactAuthURL(raw)

	assert.NotContains(redacted, "secret-challenge", "code_challenge should be redacted")
	assert.NotContains(redacted, "secret-nonce", "nonce should be redacted")
	assert.NotContains(redacted, "secret-state", "state should be redacted")
	// Non-sensitive params should be preserved
	assert.Contains(redacted, "client_id=test", "client_id should be preserved")
	assert.Contains(redacted, "login_hint=", "login_hint should be preserved")
	assert.Contains(redacted, "REDACTED", "should contain REDACTED placeholders")
}

func TestRedactAuthURL_InvalidURL(t *testing.T) {
	result := redactAuthURL("://not-a-url")
	assert.Equal(t, "[invalid URL]", result)
}

// --- DeleteToken with revocation ---

func TestDeleteToken_RevokesBeforeDeleting(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{
		clientID:  "test-client",
		tenantID:  "common",
		tokensDir: dir,
		logger:    slog.Default(),
	}

	token := &oauth2.Token{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
	}
	require.NoError(t, m.saveToken("user@example.com", token, nil, ""))

	// DeleteToken should succeed even when revocation fails (no real
	// Microsoft endpoint in tests). The local file should be removed.
	require.NoError(t, m.DeleteToken("user@example.com"), "DeleteToken")
	assert.False(t, m.HasToken("user@example.com"), "token file should be deleted after DeleteToken")
}

func TestDeleteToken_NoTokenFile(t *testing.T) {
	m := &Manager{
		clientID:  "test-client",
		tenantID:  "common",
		tokensDir: t.TempDir(),
		logger:    slog.Default(),
	}
	// Should not error on non-existent token
	assert.NoError(t, m.DeleteToken("nobody@example.com"), "DeleteToken non-existent")
}

// --- peekTIDFromJWT edge cases ---

func TestTokenSource_PreMigrationTokenGetsTenantBinding(t *testing.T) {
	// Pre-migration tokens have empty TenantID. When the Manager is constructed
	// with a specific (non-"common") tenant, TokenSource should bind that tenant
	// so scope validation can run on the next load.
	dir := t.TempDir()
	m := &Manager{
		clientID:  "test-client",
		tenantID:  "my-org-tenant",
		tokensDir: dir,
		logger:    slog.Default(),
	}

	// Save a token with empty TenantID and org scopes (pre-migration state).
	token := &oauth2.Token{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
	}
	require.NoError(t, m.saveToken("user@company.com", token, []string{ScopeIMAPOrg, "offline_access"}, ""))

	// TokenSource should succeed: the tenant gets bound internally.
	// If scope validation kicked in and found a mismatch it would error.
	ts, err := m.TokenSource(t.Context(), "user@company.com")
	require.NoError(t, err)
	require.NotNil(t, ts, "TokenSource returned nil")
}

func TestTokenSource_SaveFailureReturnsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only directory does not prevent file creation on Windows")
	}
	// Verify saveToken returns an error when the tokens directory is read-only.
	dir := t.TempDir()
	m := &Manager{tokensDir: dir, logger: slog.Default()}
	token := &oauth2.Token{AccessToken: "access", RefreshToken: "refresh", TokenType: "Bearer"}
	require.NoError(t, m.saveToken("user@example.com", token, nil, ""), "initial save")

	// Make tokens directory read-only so subsequent writes fail.
	require.NoError(t, os.Chmod(dir, 0500), "chmod")
	defer func() { _ = os.Chmod(dir, 0700) }()

	err := m.saveToken("user@example.com", token, nil, "")
	require.Error(t, err, "expected error when tokens dir is read-only")
}

func TestPeekTIDFromJWT_TooFewParts(t *testing.T) {
	for _, input := range []string{"onlyone", "header.payload"} {
		_, err := peekTIDFromJWT(input)
		assert.Error(t, err, "peekTIDFromJWT(%q): expected error for malformed JWT", input)
	}
}

func TestPeekTIDFromJWT_TooManyParts(t *testing.T) {
	_, err := peekTIDFromJWT("a.b.c.d")
	assert.Error(t, err, "expected error for JWT with more than 3 parts")
}

func TestPeekTIDFromJWT_InvalidBase64Payload(t *testing.T) {
	_, err := peekTIDFromJWT("header.!!!not-base64!!!.sig")
	require.Error(t, err, "expected error for invalid base64 payload")
}

func TestPeekTIDFromJWT_MissingTIDClaim(t *testing.T) {
	// Valid JWT but payload has no "tid" field.
	idToken := makeIDToken(t, map[string]any{"email": "user@example.com"})
	_, err := peekTIDFromJWT(idToken)
	require.Error(t, err, "expected error for JWT without tid claim")
	assert.ErrorContains(t, err, "no tid claim")
}
