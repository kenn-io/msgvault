package microsoft

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.kenn.io/msgvault/internal/fileutil"
	"golang.org/x/oauth2"
)

const (
	DefaultTenant = "common"

	// ScopeIMAPOrg is the IMAP scope for organizational (O365) accounts.
	ScopeIMAPOrg = "https://outlook.office365.com/IMAP.AccessAsUser.All"
	// ScopeIMAPPersonal is the IMAP scope for personal Microsoft accounts
	// (hotmail.com, outlook.com, live.com, etc.).
	ScopeIMAPPersonal = "https://outlook.office.com/IMAP.AccessAsUser.All"

	// MicrosoftConsumerTenantID is the fixed tenant ID for all personal
	// Microsoft accounts (outlook.com, hotmail.com, live.com, etc.).
	MicrosoftConsumerTenantID = "9188040d-6c67-4c5b-b112-36a304b66dad"

	redirectPort = "8089"
	callbackPath = "/callback/microsoft"

	// scopeOfflineAccess is the OAuth scope requesting a refresh token.
	scopeOfflineAccess = "offline_access"
	// scopeEmail is the OpenID "email" scope requested alongside the IMAP scope.
	scopeEmail = "email"
	// logKeyEmail is the structured-log field key carrying the account email.
	logKeyEmail = "email"
)

// scopesForEmail returns the OAuth scopes appropriate for the given email.
// Personal Microsoft accounts use a different IMAP resource than org accounts.
func scopesForEmail(email string) []string {
	imapScope := ScopeIMAPOrg
	if isPersonalMicrosoftAccount(email) {
		imapScope = ScopeIMAPPersonal
	}
	return []string{imapScope, scopeOfflineAccess, "openid", scopeEmail}
}

// isPersonalMicrosoftAccount returns true for common consumer Microsoft domains.
func isPersonalMicrosoftAccount(email string) bool {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return false
	}
	domain := strings.ToLower(parts[1])
	switch domain {
	case "hotmail.com", "outlook.com", "live.com", "msn.com",
		"hotmail.co.uk", "hotmail.fr", "hotmail.de", "hotmail.es", "hotmail.it",
		"hotmail.co.jp", "hotmail.com.au", "hotmail.com.br",
		"live.co.uk", "live.fr", "live.de", "live.it",
		"live.com.au", "live.jp",
		"outlook.co.uk", "outlook.fr", "outlook.de", "outlook.es", "outlook.it",
		"outlook.jp", "outlook.kr", "outlook.com.br", "outlook.com.au":
		return true
	}
	return false
}

// idTokenClaims holds the relevant claims extracted from a Microsoft ID token.
type idTokenClaims struct {
	Email             string // "email" claim (SMTP address)
	PreferredUsername string // "preferred_username" claim (may be UPN for org accounts)
	TenantID          string // "tid" claim
}

// imapScopeForTenant returns the correct IMAP scope based on the tenant ID.
// The consumer tenant has a fixed, well-known ID; all others are org tenants.
func imapScopeForTenant(tid string) string {
	if strings.EqualFold(tid, MicrosoftConsumerTenantID) {
		return ScopeIMAPPersonal
	}
	return ScopeIMAPOrg
}

type TokenMismatchError struct {
	Expected string
	Actual   string
}

func (e *TokenMismatchError) Error() string {
	return fmt.Sprintf("token mismatch: expected %s but authorized as %s", e.Expected, e.Actual)
}

type Manager struct {
	clientID  string
	tenantID  string
	tokensDir string
	logger    *slog.Logger

	// browserFlowFn overrides browserFlow for testing. Returns (token, nonce, error).
	browserFlowFn func(ctx context.Context, email string, scopes []string) (*oauth2.Token, string, error)
	// verifyIDTokenFn overrides verifyIDToken for testing (bypasses OIDC validation).
	verifyIDTokenFn func(ctx context.Context, rawIDToken string) (*idTokenClaims, error)
}

func NewManager(clientID, tenantID, tokensDir string, logger *slog.Logger) *Manager {
	if tenantID == "" {
		tenantID = DefaultTenant
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		clientID:  clientID,
		tenantID:  tenantID,
		tokensDir: tokensDir,
		logger:    logger,
	}
}

func (m *Manager) oauthConfig(scopes []string) *oauth2.Config {
	return m.oauthConfigWithTenant(m.tenantID, scopes)
}

func (m *Manager) oauthConfigWithTenant(tenantID string, scopes []string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: m.clientID,
		Endpoint: oauth2.Endpoint{
			AuthURL:  fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/authorize", tenantID),
			TokenURL: fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenantID),
		},
		RedirectURL: "http://localhost:" + redirectPort + callbackPath,
		Scopes:      scopes,
	}
}

func (m *Manager) Authorize(ctx context.Context, email string) error {
	scopes := scopesForEmail(email)
	flow := m.doBrowserFlow
	token, nonce, err := flow(ctx, email, scopes)
	if err != nil {
		return err
	}
	_, claims, err := m.resolveTokenEmail(ctx, email, token, nonce)
	if err != nil {
		return err
	}

	// Correct IMAP scope if the domain-based guess was wrong.
	// The tid claim from the ID token is authoritative for account type.
	// We must restart the browser flow (not just refresh) because consent
	// for a different IMAP resource requires interactive authorization.
	if claims.TenantID != "" {
		correctIMAPScope := imapScopeForTenant(claims.TenantID)
		if scopes[0] != correctIMAPScope {
			m.logger.Info("correcting IMAP scope based on tenant ID, re-authorizing",
				logKeyEmail, email,
				"tid", claims.TenantID,
				"from", scopes[0],
				"to", correctIMAPScope,
			)
			scopes = []string{correctIMAPScope, scopeOfflineAccess, "openid", scopeEmail}
			token, nonce, err = flow(ctx, email, scopes)
			if err != nil {
				return fmt.Errorf("re-authorize with correct IMAP scope: %w", err)
			}
			_, claims, err = m.resolveTokenEmail(ctx, email, token, nonce)
			if err != nil {
				return err
			}
		}
	}

	tenantID := ""
	if claims != nil {
		tenantID = claims.TenantID
	}
	return m.saveToken(email, token, scopes, tenantID)
}

// doBrowserFlow dispatches to browserFlowFn (test hook) or the real browserFlow.
func (m *Manager) doBrowserFlow(ctx context.Context, email string, scopes []string) (*oauth2.Token, string, error) {
	if m.browserFlowFn != nil {
		return m.browserFlowFn(ctx, email, scopes)
	}
	return m.browserFlow(ctx, email, scopes)
}

// tokenRefreshTimeout bounds how long a single token refresh HTTP request
// may take. Prevents indefinite hangs when Microsoft's token endpoint is
// unreachable, while still being generous enough for slow networks.
const tokenRefreshTimeout = 30 * time.Second

// TokenSource returns a function that provides fresh access tokens.
// Suitable for passing to imap.WithTokenSource. The returned function
// is safe for concurrent use.
//
// Token refresh HTTP requests run against context.Background so they are
// not cancelled if the caller's context expires between calls. This
// prevents silent refresh failures during long-running syncs where the
// original context may be narrower than the token source's lifetime.
// Each refresh attempt is bounded by tokenRefreshTimeout to prevent
// indefinite hangs when the token endpoint is unreachable.
func (m *Manager) TokenSource(ctx context.Context, email string) (func(context.Context) (string, error), error) {
	tf, err := m.loadTokenFile(email)
	if err != nil {
		return nil, fmt.Errorf("no valid token for %s: %w", email, err)
	}

	scopes := tf.Scopes
	if len(scopes) == 0 {
		scopes = scopesForEmail(email)
	}

	// Migrate pre-migration tokens: if the token file has no tenant_id but the
	// Manager was constructed with a specific tenant (not "common"), bind it.
	// This allows scope validation to kick in on next load.
	if tf.TenantID == "" && m.tenantID != "" && m.tenantID != DefaultTenant {
		tf.TenantID = m.tenantID
		m.logger.Info("migrating pre-scope-correction token: binding tenant ID",
			logKeyEmail, email, "tenant", m.tenantID)
	}

	// Validate persisted scopes against tenant ID to detect stale tokens
	// from before scope-correction was added. Tokens without a tenant_id
	// are pre-migration and skip this check (backward compatible).
	if tf.TenantID != "" && len(tf.Scopes) > 0 {
		correctScope := imapScopeForTenant(tf.TenantID)
		if tf.Scopes[0] != correctScope {
			m.logger.Debug("stale IMAP scope detected",
				logKeyEmail, email,
				"current_scope", tf.Scopes[0],
				"expected_scope", correctScope,
				"tenant_id", tf.TenantID,
			)
			return nil, fmt.Errorf(
				"token for %s has stale IMAP scope — run 'msgvault add-o365 %s' to re-authorize",
				email, email,
			)
		}
	}

	refreshTenant := m.tenantID
	if tf.TenantID != "" {
		refreshTenant = tf.TenantID
	}
	oauthCfg := m.oauthConfigWithTenant(refreshTenant, scopes)
	// Use context.Background so token refreshes are not cancelled by the
	// caller's (sync-scoped) context. The token source outlives individual
	// operations; tying it to a cancellable context would cause silent
	// failures on the next call after cancellation.
	ts := oauthCfg.TokenSource(context.Background(), &tf.Token)

	var (
		mu               sync.Mutex
		lastAccessToken  = tf.AccessToken
		lastRefreshToken = tf.RefreshToken
		lastExpiry       = tf.Expiry
	)

	return func(callCtx context.Context) (string, error) {
		// Run ts.Token() in a goroutine bounded by a timeout and the
		// caller's context. The oauth2 TokenSource is internally
		// synchronized and caches valid tokens, so most calls return
		// immediately; the timeout only matters when a network refresh
		// is required.
		type tokenResult struct {
			tok *oauth2.Token
			err error
		}
		ch := make(chan tokenResult, 1)
		go func() {
			tok, err := ts.Token()
			ch <- tokenResult{tok, err}
		}()

		timer := time.NewTimer(tokenRefreshTimeout)
		defer timer.Stop()

		var tok *oauth2.Token
		select {
		case res := <-ch:
			if res.err != nil {
				return "", fmt.Errorf("refresh Microsoft token: %w", res.err)
			}
			tok = res.tok
		case <-timer.C:
			return "", fmt.Errorf("microsoft token refresh timed out after %s — check network connectivity", tokenRefreshTimeout)
		case <-callCtx.Done():
			return "", fmt.Errorf("microsoft token refresh cancelled: %w", callCtx.Err())
		}

		mu.Lock()
		changed := tok.AccessToken != lastAccessToken ||
			tok.RefreshToken != lastRefreshToken ||
			!tok.Expiry.Equal(lastExpiry)
		if changed {
			lastAccessToken = tok.AccessToken
			lastRefreshToken = tok.RefreshToken
			lastExpiry = tok.Expiry
		}
		mu.Unlock()

		if changed {
			if saveErr := m.saveToken(email, tok, scopes, tf.TenantID); saveErr != nil {
				return "", fmt.Errorf("save refreshed microsoft token for %s: %w (token refreshed but not persisted — re-run may require re-authorization)", email, saveErr)
			}
		}

		return tok.AccessToken, nil
	}, nil
}

// IMAPHost returns the correct IMAP hostname for the given email based on the
// persisted token's scope. Must be called after Authorize completes.
// Personal accounts use outlook.office.com; org accounts use outlook.office365.com.
func (m *Manager) IMAPHost(email string) (string, error) {
	tf, err := m.loadTokenFile(email)
	if err != nil {
		return "", fmt.Errorf("load token for %s: %w", email, err)
	}
	if len(tf.Scopes) > 0 && tf.Scopes[0] == ScopeIMAPPersonal {
		return "outlook.office.com", nil
	}
	return "outlook.office365.com", nil
}

func (m *Manager) browserFlow(ctx context.Context, email string, scopes []string) (*oauth2.Token, string, error) {
	// Bound the entire browser flow to 5 minutes. This prevents port 8089
	// from staying bound indefinitely if the user abandons authorization.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Bind the listener before constructing the auth URL so that the redirect
	// URI embedded in the authorization request matches exactly. Failing fast
	// here produces a clear error instead of a silent hang.
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", "localhost:"+redirectPort)
	if err != nil {
		return nil, "", fmt.Errorf(
			"port %s is already in use — ensure no other process is using it and retry: %w",
			redirectPort, err,
		)
	}

	cfg := m.oauthConfig(scopes)

	// PKCE (required by Azure AD for public clients)
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	challengeHash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])

	// CSRF state
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, "", fmt.Errorf("generate state: %w", err)
	}
	state := base64.URLEncoding.EncodeToString(stateBytes)

	// Nonce for ID token replay protection
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, "", fmt.Errorf("generate nonce: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)

	codeChan := make(chan string, 1)
	errChan := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Query().Get("state") != state {
			select {
			case errChan <- errors.New("state mismatch: possible CSRF attack"):
			default:
			}
			_, _ = fmt.Fprintf(w, "Error: state mismatch")
			return
		}
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			desc := r.URL.Query().Get("error_description")
			select {
			case errChan <- fmt.Errorf("microsoft OAuth error: %s: %s", errMsg, desc):
			default:
			}
			// Set text/plain so any HTML-looking characters in desc are
			// rendered literally, not interpreted by the browser.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = fmt.Fprintf(w, "Error: %s", desc) //nolint:gosec // text/plain prevents HTML injection
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			select {
			case errChan <- errors.New("no code in callback"):
			default:
			}
			_, _ = fmt.Fprintf(w, "Error: no authorization code received")
			return
		}
		select {
		case codeChan <- code:
		default:
		}
		_, _ = fmt.Fprintf(w, "Authorization successful! You can close this window.")
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(ln); err != http.ErrServerClosed {
			select {
			case errChan <- err:
			default:
			}
		}
	}()
	defer func() {
		// Use a fresh context for shutdown: the caller's ctx may already be
		// cancelled (e.g. Ctrl-C), in which case Shutdown(ctx) returns
		// immediately without waiting for in-flight requests to drain.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	authURL := cfg.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oauth2.SetAuthURLParam("login_hint", email),
		oauth2.SetAuthURLParam("nonce", nonce),
	)

	fmt.Printf("Opening browser for Microsoft authorization...\n")
	fmt.Printf("If browser doesn't open, visit:\n%s\n\n", redactAuthURL(authURL))
	if err := openBrowser(ctx, authURL); err != nil {
		m.logger.Warn("failed to open browser", "error", err)
	}

	select {
	case code := <-codeChan:
		token, err := cfg.Exchange(ctx, code,
			oauth2.SetAuthURLParam("code_verifier", verifier),
		)
		if err != nil {
			return nil, "", fmt.Errorf("exchange authorization code: %w", err)
		}
		return token, nonce, nil
	case err := <-errChan:
		return nil, "", err
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
}

// resolveTokenEmail verifies the ID token and validates the authenticated
// email matches the expected address. Uses OIDC signature/issuer/audience
// validation in production, or verifyIDTokenFn in tests.
func (m *Manager) resolveTokenEmail(ctx context.Context, email string, token *oauth2.Token, nonce string) (string, *idTokenClaims, error) {
	rawIDToken, _ := token.Extra("id_token").(string)
	if rawIDToken == "" {
		return "", nil, errors.New("no id_token in authorization response")
	}

	var claims *idTokenClaims
	var err error
	if m.verifyIDTokenFn != nil {
		claims, err = m.verifyIDTokenFn(ctx, rawIDToken)
	} else {
		claims, err = m.verifyIDToken(ctx, rawIDToken, nonce)
	}
	if err != nil {
		return "", nil, fmt.Errorf("verify ID token: %w", err)
	}

	// Prefer the "email" claim — it is the authoritative SMTP address.
	if claims.Email != "" {
		if !strings.EqualFold(claims.Email, email) {
			return "", claims, &TokenMismatchError{Expected: email, Actual: claims.Email}
		}
		return claims.Email, claims, nil
	}

	// Fall back to "preferred_username". In Entra/O365 setups the UPN
	// (sign-in identifier) can differ from the SMTP mailbox address, so a
	// mismatch is not necessarily an error — trust the user-entered email as
	// the mailbox address and log a warning. If the address is wrong, IMAP
	// authentication will fail with an explicit error pointing back here.
	if claims.PreferredUsername != "" {
		if !strings.EqualFold(claims.PreferredUsername, email) {
			m.logger.Warn("Microsoft sign-in UPN differs from the address you specified — "+
				"proceeding with your address as the IMAP mailbox; "+
				"if sync fails with an authentication error, re-run 'add-o365' "+
				"using the UPN shown below as the email argument",
				"specified", email,
				"upn", claims.PreferredUsername,
			)
		}
		return email, claims, nil
	}

	return "", claims, errors.New("no email or preferred_username claim in ID token")
}

// verifyIDToken validates the ID token using Microsoft's OIDC discovery and
// JWKS endpoints. Checks signature, issuer, audience, expiry, and nonce.
func (m *Manager) verifyIDToken(ctx context.Context, rawIDToken, nonce string) (*idTokenClaims, error) {
	// Peek at the unverified tid to construct the tenant-specific OIDC provider.
	// This is safe because the subsequent OIDC verification will fail if the
	// token was actually issued by a different tenant than the derived issuer.
	tid, err := peekTIDFromJWT(rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("peek tenant ID from JWT: %w", err)
	}

	issuerURL := fmt.Sprintf("https://login.microsoftonline.com/%s/v2.0", tid)
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for tenant %s: %w", tid, err)
	}

	verifier := provider.Verifier(&oidc.Config{
		ClientID: m.clientID,
	})

	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("verify ID token signature/claims: %w", err)
	}

	// Verify nonce to prevent replay attacks.
	if idToken.Nonce != nonce {
		return nil, errors.New("ID token nonce mismatch (possible replay attack)")
	}

	var raw struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		TenantID          string `json:"tid"`
	}
	if err := idToken.Claims(&raw); err != nil {
		return nil, fmt.Errorf("extract ID token claims: %w", err)
	}
	return &idTokenClaims{
		Email:             raw.Email,
		PreferredUsername: raw.PreferredUsername,
		TenantID:          raw.TenantID,
	}, nil
}

// peekTIDFromJWT does a minimal unverified decode of a JWT to extract the
// "tid" (tenant ID) claim. Used only to determine which OIDC provider URL
// to construct for full validation.
func peekTIDFromJWT(rawToken string) (string, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid JWT format (expected 3 parts, got %d)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		TenantID string `json:"tid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parse JWT claims: %w", err)
	}
	if claims.TenantID == "" {
		return "", errors.New("no tid claim in JWT")
	}
	return claims.TenantID, nil
}

// --- Token storage ---

type tokenFile struct {
	oauth2.Token

	Scopes   []string `json:"scopes,omitempty"`
	TenantID string   `json:"tenant_id,omitempty"`
}

func (m *Manager) TokenPath(email string) string {
	safe := sanitizeEmail(email)
	return filepath.Join(m.tokensDir, "microsoft_"+safe+".json")
}

func (m *Manager) saveToken(email string, token *oauth2.Token, scopes []string, tenantID string) error {
	if err := fileutil.SecureMkdirAll(m.tokensDir, 0700); err != nil {
		return err
	}

	tf := tokenFile{Token: *token, Scopes: scopes, TenantID: tenantID}
	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}

	path := m.TokenPath(email)
	tmpFile, err := os.CreateTemp(m.tokensDir, ".ms-token-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp token file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp token file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp token file: %w", err)
	}
	if err := fileutil.SecureChmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp token file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp token file: %w", err)
	}
	return nil
}

func (m *Manager) loadTokenFile(email string) (*tokenFile, error) {
	path := m.TokenPath(email)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return nil, err
	}
	return &tf, nil
}

func (m *Manager) HasToken(email string) bool {
	_, err := os.Stat(m.TokenPath(email))
	return err == nil
}

// DeleteToken revokes the refresh token at Microsoft (best-effort) and
// removes the local token file. Revocation failures are logged but do not
// prevent local cleanup — the user's intent is to remove the account, and
// a stale remote token will expire naturally (≤90 days).
func (m *Manager) DeleteToken(email string) error {
	// Attempt to revoke the refresh token at Microsoft before deleting
	// the local file. We load the token first so we can send the
	// revocation request.
	tf, loadErr := m.loadTokenFile(email)
	if loadErr == nil && tf.RefreshToken != "" {
		if revokeErr := m.revokeToken(tf.RefreshToken, tf.TenantID); revokeErr != nil {
			m.logger.Warn("failed to revoke Microsoft token (will expire naturally)",
				logKeyEmail, email, "error", revokeErr)
		} else {
			m.logger.Info("revoked Microsoft refresh token", logKeyEmail, email)
		}
	}

	err := os.Remove(m.TokenPath(email))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// revokeToken sends a POST to Microsoft's OAuth2 logout/revocation endpoint.
// Microsoft's v2.0 endpoint does not implement RFC 7009 token revocation,
// but the /oauth2/v2.0/logout endpoint with a token hint invalidates the
// refresh token. As a fallback we POST to the token endpoint with
// grant_type=revocation which some tenants support.
func (m *Manager) revokeToken(refreshToken, tenantID string) error {
	if tenantID == "" {
		tenantID = m.tenantID
	}
	if tenantID == "" {
		tenantID = DefaultTenant
	}

	// Microsoft supports token revocation via the /oauth2/v2.0/logout
	// endpoint for confidential clients, but for public clients the
	// recommended approach is to POST to the token endpoint. We use a
	// direct HTTP POST to revoke the refresh token.
	revokeURL := fmt.Sprintf(
		"https://login.microsoftonline.com/%s/oauth2/v2.0/logout", tenantID)

	data := url.Values{
		"client_id": {m.clientID},
		"token":     {refreshToken},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, revokeURL,
		strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("create revocation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("send revocation request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Microsoft's logout endpoint returns 200 on success; treat any 2xx as ok.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("revocation returned HTTP %d", resp.StatusCode)
}

func sanitizeEmail(email string) string {
	safe := strings.ReplaceAll(email, "\x00", "_")
	safe = strings.ReplaceAll(safe, "/", "_")
	safe = strings.ReplaceAll(safe, "\\", "_")
	safe = strings.ReplaceAll(safe, "..", "_.._")
	// filepath.Base strips any residual directory components as a final
	// defense-in-depth against path traversal in crafted email addresses.
	return filepath.Base(safe)
}

// redactAuthURL strips security-sensitive query parameters (state, nonce,
// code_challenge) from the authorization URL before printing to stdout.
// The full URL is still passed to the browser via openBrowser; only the
// fallback text shown in the terminal is redacted to prevent these
// short-lived secrets from persisting in logs.
func redactAuthURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "[invalid URL]"
	}
	q := u.Query()
	for _, param := range []string{"state", "nonce", "code_challenge"} {
		if q.Has(param) {
			q.Set(param, "REDACTED")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func openBrowser(ctx context.Context, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" {
		return fmt.Errorf("refused to open URL with scheme %q (only https is allowed)", parsed.Scheme)
	}

	// rawURL is constrained to https above; pass it directly to the OS opener.
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", rawURL) //nolint:gosec // rawURL is validated as https
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", rawURL) //nolint:gosec // rawURL is validated as https
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", rawURL) //nolint:gosec // rawURL is validated as https
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser command: %w", err)
	}
	return nil
}
