// Package oauth provides OAuth2 authentication flows for Gmail.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/fileutil"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Scopes for normal msgvault operations (sync, search, read).
var Scopes = []string{
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/gmail.modify",
}

// ScopesDeletion includes full access required for batchDelete API.
// gmail.modify supports trash/untrash but NOT batchDelete.
var ScopesDeletion = []string{
	"https://mail.google.com/",
}

const defaultProfileURL = "https://gmail.googleapis.com/gmail/v1/users/me/profile"

// TokenMismatchError is returned when the authorized Google account
// does not match the expected email. Callers can inspect Expected
// and Actual to provide context-appropriate remediation.
type TokenMismatchError struct {
	Expected string // email the user asked to authorize
	Actual   string // email returned by the Gmail profile API
}

func (e *TokenMismatchError) Error() string {
	return fmt.Sprintf(
		"token mismatch: expected %s but authorized as %s",
		e.Expected, e.Actual,
	)
}

// Manager handles OAuth2 token acquisition and storage.
type Manager struct {
	config     *oauth2.Config
	tokensDir  string
	logger     *slog.Logger
	profileURL string // Gmail profile endpoint; overridden in tests

	// browserFlowFn overrides browserFlow in tests to avoid starting
	// a real HTTP server and browser. When nil, the real browserFlow
	// is used.
	browserFlowFn func(ctx context.Context, email string, launchBrowser bool) (*oauth2.Token, error)
}

// NewManager creates an OAuth manager from client secrets.
func NewManager(clientSecretsPath, tokensDir string, logger *slog.Logger) (*Manager, error) {
	return NewManagerWithScopes(clientSecretsPath, tokensDir, logger, Scopes)
}

// TokenSource returns a token source for the given email.
// If a valid token exists, it will be reused and auto-refreshed.
func (m *Manager) TokenSource(ctx context.Context, email string) (oauth2.TokenSource, error) {
	tf, err := m.loadTokenFile(email)
	if err != nil {
		return nil, fmt.Errorf("no valid token for %s: %w", email, err)
	}

	// Create a token source that auto-refreshes
	ts := m.config.TokenSource(ctx, &tf.Token)

	// Save refreshed token if it changed
	newToken, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("refresh token: %w", err)
	}

	if newToken.AccessToken != tf.AccessToken {
		// Preserve the original scopes when saving refreshed token
		scopes := tf.Scopes
		if len(scopes) == 0 {
			scopes = m.config.Scopes // fallback for legacy tokens
		}
		if err := m.saveToken(email, newToken, scopes); err != nil {
			m.logger.Warn("failed to save refreshed token", "email", email, "error", err)
		}
	}

	return ts, nil
}

// HasToken checks if a token exists for the given email.
func (m *Manager) HasToken(email string) bool {
	_, err := m.loadToken(email)
	return err == nil
}

// PrintHeadlessInstructions prints setup instructions for headless servers.
// Google's device flow does not support Gmail scopes, so users must authorize
// on a machine with a browser and copy the token file.
// tokensDir should be the configured tokens directory (e.g., cfg.TokensDir()).
func PrintHeadlessInstructions(email, tokensDir, oauthApp string) {
	// Use same sanitization as tokenPath for consistency
	tokenFile := sanitizeEmail(email) + ".json"
	tokenPath := filepath.Join(tokensDir, tokenFile)

	addCmd := "    msgvault add-account " + email
	if oauthApp != "" {
		addCmd += " --oauth-app " + oauthApp
	}

	fmt.Println()
	fmt.Println("=== Headless Server Setup ===")
	fmt.Println()
	fmt.Println("Google's OAuth device flow does not support Gmail scopes, so --headless")
	fmt.Println("cannot directly authorize. Instead, authorize on a machine with a browser")
	fmt.Println("and copy the token to your server.")
	fmt.Println()
	fmt.Println("Step 1: On a machine with a browser, run:")
	fmt.Println()
	fmt.Println(addCmd)
	fmt.Println()
	fmt.Println("Step 2: Copy the token file to your headless server:")
	fmt.Println()
	fmt.Printf("    ssh user@server mkdir -p %s\n", shellQuote(tokensDir))
	fmt.Printf("    scp %s user@server:%s\n", shellQuote(tokenPath), shellQuote(tokenPath))
	fmt.Println()
	fmt.Println("Step 3: On the headless server, register the account:")
	fmt.Println()
	fmt.Println(addCmd)
	fmt.Println()
	fmt.Println("The token will be detected and the account registered. No browser needed.")
	fmt.Println("All msgvault commands (sync, tui, etc.) will work normally.")
	fmt.Println()
}

// sanitizeEmail sanitizes an email for use in a filename.
func sanitizeEmail(email string) string {
	safe := strings.ReplaceAll(email, "/", "_")
	safe = strings.ReplaceAll(safe, "\\", "_")
	safe = strings.ReplaceAll(safe, "..", "_")
	return safe
}

// shellQuote returns a shell-safe quoted string using single quotes.
// Handles embedded single quotes by ending the quoted string, adding an
// escaped single quote, and starting a new quoted string: ' -> '\”.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// Authorize performs the browser OAuth flow for a new account.
// It opens the default browser and validates that the authorized
// account matches the expected email.
func (m *Manager) Authorize(ctx context.Context, email string) error {
	return m.authorize(ctx, email, true)
}

// AuthorizeManual performs the OAuth flow without opening a browser.
// It prints the authorization URL with clear account context so the
// user knows exactly which account to authorize. Used during sync
// re-auth to prevent accidental account mismatch.
func (m *Manager) AuthorizeManual(ctx context.Context, email string) error {
	return m.authorize(ctx, email, false)
}

func (m *Manager) authorize(
	ctx context.Context, email string, launchBrowser bool,
) error {
	flow := m.browserFlow
	if m.browserFlowFn != nil {
		flow = m.browserFlowFn
	}
	token, err := flow(ctx, email, launchBrowser)
	if err != nil {
		return err
	}

	// Validate the token belongs to the expected account before
	// persisting it. This prevents token pollution where selecting
	// the wrong Google account would overwrite a valid token file.
	// The token is always saved under the original identifier (email)
	// since that's the key used for all lookups elsewhere in the app.
	if _, err := m.resolveTokenEmail(ctx, email, token); err != nil {
		return err
	}

	return m.saveToken(email, token, m.config.Scopes)
}

const (
	redirectPort = "8089"
	callbackPath = "/callback"
)

// newCallbackHandler returns an HTTP handler that processes the OAuth callback.
func (m *Manager) newCallbackHandler(expectedState string, codeChan chan<- string, errChan chan<- error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != expectedState {
			errChan <- errors.New("state mismatch: possible CSRF attack")
			_, _ = fmt.Fprintf(w, "Error: state mismatch")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errChan <- errors.New("no code in callback")
			_, _ = fmt.Fprintf(w, "Error: no authorization code received")
			return
		}
		codeChan <- code
		_, _ = fmt.Fprintf(w, "Authorization successful! You can close this window.")
	}
}

// browserFlow runs the OAuth authorization flow with a local callback server.
// email is used as login_hint to pre-select the Google account.
// If launchBrowser is false, the URL is printed without opening a browser.
func (m *Manager) browserFlow(
	ctx context.Context, email string, launchBrowser bool,
) (*oauth2.Token, error) {
	// Bail early if context is already cancelled — no point starting
	// a server or opening a browser.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Generate random state for CSRF protection
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}
	state := base64.URLEncoding.EncodeToString(stateBytes)

	// Start local server for callback
	codeChan := make(chan string, 1)
	errChan := make(chan error, 1)

	mux := http.NewServeMux()
	mux.Handle(callbackPath, m.newCallbackHandler(state, codeChan, errChan))
	server := &http.Server{
		Addr:              "localhost:" + redirectPort,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			errChan <- err
		}
	}()

	defer func() { _ = server.Shutdown(ctx) }()

	// Generate auth URL with login_hint to pre-select account
	m.config.RedirectURL = "http://localhost:" + redirectPort + callbackPath
	authOpts := []oauth2.AuthCodeOption{
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
	}
	if email != "" {
		authOpts = append(authOpts,
			oauth2.SetAuthURLParam("login_hint", email))
	}
	authURL := m.config.AuthCodeURL(state, authOpts...)

	if launchBrowser {
		fmt.Printf("Opening browser for authorization...\n")
		fmt.Printf("If browser doesn't open, visit:\n%s\n\n", authURL)
		if err := openBrowser(ctx, authURL); err != nil {
			m.logger.Warn("failed to open browser", "error", err)
		}
	} else {
		fmt.Printf("\n=== Re-authorization required for %s ===\n\n", email)
		fmt.Printf("Open this URL in your browser and select the account %s:\n\n", email)
		fmt.Printf("  %s\n\n", authURL)
		fmt.Printf("Waiting for authorization...\n")
	}

	// Wait for callback
	select {
	case code := <-codeChan:
		token, err := m.config.Exchange(ctx, code)
		if err != nil {
			return nil, fmt.Errorf("exchange authorization code: %w", err)
		}
		return token, nil
	case err := <-errChan:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

const resolveTimeout = 10 * time.Second

// resolveTokenEmail calls the Gmail profile API to confirm that
// the token belongs to an account matching the expected email.
// Returns the canonical (primary) Gmail address for the account,
// which may differ from the input when the user supplies an alias
// or secondary login address. The token is never persisted by this
// function — the caller decides what to do on success or failure.
func (m *Manager) resolveTokenEmail(
	ctx context.Context, email string, token *oauth2.Token,
) (string, error) {
	profileURL := m.profileURL
	if profileURL == "" {
		profileURL = defaultProfileURL
	}
	ts := m.config.TokenSource(ctx, token)
	return fetchTokenProfileEmail(ctx, ts, profileURL, email, tokenProfileErrorOAuth)
}

// sameGoogleAccount returns true if two email addresses belong to the
// same Google account. This covers the common alias cases:
//   - exact match (case-insensitive)
//   - gmail.com dot-insensitive (first.last@gmail.com == firstlast@gmail.com)
//   - gmail.com plus-address (user+tag@gmail.com == user@gmail.com)
//   - googlemail.com ↔ gmail.com equivalence
//
// For Google Workspace domains we cannot verify aliases without an
// admin API call, so we fall back to exact-match only.
func sameGoogleAccount(expected, canonical string) bool {
	if strings.EqualFold(expected, canonical) {
		return true
	}

	// Normalize gmail.com / googlemail.com addresses for comparison
	expectedNorm := normalizeGmailAddress(expected)
	canonicalNorm := normalizeGmailAddress(canonical)

	return expectedNorm != "" && expectedNorm == canonicalNorm
}

// normalizeGmailAddress returns a canonical form of a gmail.com or
// googlemail.com address by lowercasing, stripping +suffixes and dots
// from the local part, and mapping googlemail.com → gmail.com.
// Returns "" for non-Gmail addresses.
func normalizeGmailAddress(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return ""
	}
	local := strings.ToLower(email[:at])
	domain := strings.ToLower(email[at+1:])

	if domain != "gmail.com" && domain != "googlemail.com" {
		return ""
	}

	// Gmail ignores dots and +suffixes in the local part
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	local = strings.ReplaceAll(local, ".", "")
	return local + "@gmail.com"
}

// tokenFile wraps an OAuth2 token with metadata about the scopes and
// client it was authorized with. This enables proactive scope checking
// (e.g., detecting that deletion requires re-authorization) and client
// identity verification (detecting that an OAuth app switch requires
// re-authorization) without making an API call first.
type tokenFile struct {
	oauth2.Token

	Scopes   []string `json:"scopes,omitempty"`
	ClientID string   `json:"client_id,omitempty"`
}

// loadToken loads a saved token for the given email.
func (m *Manager) loadToken(email string) (*oauth2.Token, error) {
	path := m.tokenPath(email)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return nil, err
	}

	return &tf.Token, nil
}

// loadTokenFile loads the full token file including scope metadata.
func (m *Manager) loadTokenFile(email string) (*tokenFile, error) {
	path := m.tokenPath(email)
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

// TokenMatchesClient returns true if the stored token for the given email
// was minted by this manager's OAuth client. Returns false if the token
// doesn't exist, has no client_id metadata (legacy token), or was minted
// by a different client.
func (m *Manager) TokenMatchesClient(email string) bool {
	tf, err := m.loadTokenFile(email)
	if err != nil {
		return false
	}
	if tf.ClientID == "" {
		return false // legacy token without client_id metadata
	}
	return tf.ClientID == m.config.ClientID
}

// HasScopeMetadata returns true if the token file for this account has any
// scope metadata stored. Legacy tokens (saved before scope tracking) return false.
func (m *Manager) HasScopeMetadata(email string) bool {
	tf, err := m.loadTokenFile(email)
	if err != nil {
		return false
	}
	return len(tf.Scopes) > 0
}

// HasScope checks if the stored token for the given email was authorized
// with the specified scope. Returns false if the token doesn't exist or
// doesn't have scope metadata (legacy tokens saved before scope tracking).
func (m *Manager) HasScope(email string, scope string) bool {
	tf, err := m.loadTokenFile(email)
	if err != nil {
		return false
	}
	return slices.Contains(tf.Scopes, scope)
}

// saveToken saves a token for the given email with the specified scopes.
func (m *Manager) saveToken(email string, token *oauth2.Token, scopes []string) error {
	if err := fileutil.SecureMkdirAll(m.tokensDir, 0700); err != nil {
		return err
	}

	tf := tokenFile{
		Token:    *token,
		Scopes:   scopes,
		ClientID: m.config.ClientID,
	}

	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}

	path := m.tokenPath(email)

	// Atomic write via temp file + rename to avoid TOCTOU symlink races.
	// If an attacker creates a symlink between tokenPath() returning and
	// the write, os.Rename replaces the symlink itself rather than following it.
	tmpFile, err := os.CreateTemp(m.tokensDir, ".token-*.tmp")
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

// tokenPath returns the path to the token file for an email.
// The email is sanitized to prevent path traversal attacks.
func (m *Manager) tokenPath(email string) string {
	safe := sanitizeEmail(email)

	// Ensure the final path is within tokensDir
	path := filepath.Join(m.tokensDir, safe+".json")
	cleanPath := filepath.Clean(path)
	cleanTokensDir := filepath.Clean(m.tokensDir)

	// Verify the path is still within tokensDir (using proper directory check
	// to avoid prefix attacks like tokensDir-evil matching tokensDir)
	if !hasPathPrefix(cleanPath, cleanTokensDir) {
		// If path escapes tokensDir, use a hash-based fallback
		return filepath.Join(m.tokensDir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(email))))
	}

	// Check if path is a symlink that could escape tokensDir.
	// Note: There is an inherent TOCTOU (time-of-check to time-of-use) race between
	// this check and when the token is actually written. An attacker could create a
	// symlink after this check passes but before the write occurs. However, exploiting
	// this would require the attacker to have write access to the tokens directory and
	// precise timing, making it difficult to exploit in practice.
	if info, err := os.Lstat(cleanPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		// Path exists and is a symlink - resolve it and verify it stays within tokensDir
		resolved, err := filepath.EvalSymlinks(cleanPath)
		if err != nil || !isPathWithinDir(resolved, cleanTokensDir) {
			// Symlink resolution failed or escapes tokensDir - use hash-based fallback
			return filepath.Join(m.tokensDir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(email))))
		}
	}

	return cleanPath
}

// hasPathPrefix checks if path is equal to or a child of dir.
// This prevents prefix attacks like tokensDir-evil matching tokensDir,
// and correctly handles filesystem roots (/, C:\).
// Does not resolve symlinks - use isPathWithinDir when symlink resolution is needed.
func hasPathPrefix(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	// rel must not escape via ".." and must not be absolute
	if rel == "." {
		return true
	}
	if filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// isPathWithinDir checks if path is within dir, resolving symlinks in dir.
// Use this when checking resolved symlink targets.
func isPathWithinDir(path, dir string) bool {
	// Resolve symlinks in dir to get the real base directory
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resolvedDir = dir // fallback to original if dir doesn't exist yet
	}
	return hasPathPrefix(filepath.Clean(path), filepath.Clean(resolvedDir))
}

// scopesToString joins scopes with spaces.
func scopesToString(scopes []string) string {
	return strings.Join(scopes, " ")
}

// validateBrowserURL checks that rawURL is a valid http or https URL.
// Returns an error for invalid URLs or disallowed schemes.
func validateBrowserURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("refused to open URL with scheme %q: only http and https are allowed", parsed.Scheme)
	}
	return nil
}

// openBrowser opens the default browser to the given URL.
// Only http and https URLs are allowed to prevent command injection
// via dangerous URL schemes (e.g., file://, custom protocol handlers).
func openBrowser(ctx context.Context, rawURL string) error {
	if err := validateBrowserURL(rawURL); err != nil {
		return err
	}

	// rawURL is validated above; pass it directly to the OS opener.
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", rawURL) //nolint:gosec // rawURL passed validateBrowserURL above
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", rawURL) //nolint:gosec // rawURL passed validateBrowserURL above
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", rawURL) //nolint:gosec // rawURL passed validateBrowserURL above
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser command: %w", err)
	}
	return nil
}

// NewManagerWithScopes creates an OAuth manager with custom scopes.
func NewManagerWithScopes(clientSecretsPath, tokensDir string, logger *slog.Logger, scopes []string) (*Manager, error) {
	data, err := os.ReadFile(clientSecretsPath)
	if err != nil {
		return nil, fmt.Errorf("read client secrets: %w", err)
	}

	config, err := parseClientSecrets(data, scopes)
	if err != nil {
		return nil, fmt.Errorf("parse client secrets: %w", err)
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &Manager{
		config:    config,
		tokensDir: tokensDir,
		logger:    logger,
	}, nil
}

// parseClientSecrets parses Google OAuth client secrets JSON.
// Requires credentials with redirect_uris (Desktop app or Web app).
// TV/device clients are not supported (device flow doesn't work with Gmail).
func parseClientSecrets(data []byte, scopes []string) (*oauth2.Config, error) {
	config, err := google.ConfigFromJSON(data, scopes...)
	if err != nil {
		// Check if it's a client missing redirect_uris (TV/device or misconfigured)
		var secrets struct {
			Installed *struct {
				RedirectURIs []string `json:"redirect_uris"`
			} `json:"installed"`
			Web *struct {
				RedirectURIs []string `json:"redirect_uris"`
			} `json:"web"`
		}
		if json.Unmarshal(data, &secrets) == nil {
			missingRedirects := (secrets.Installed != nil && len(secrets.Installed.RedirectURIs) == 0) ||
				(secrets.Web != nil && len(secrets.Web.RedirectURIs) == 0)
			if missingRedirects {
				return nil, errors.New("OAuth client is missing redirect_uris (TV/device clients are not supported - Gmail doesn't work with device flow). Please create a 'Desktop application' or 'Web application' OAuth client in Google Cloud Console")
			}
		}
		return nil, fmt.Errorf("parse OAuth client secrets: %w", err)
	}
	return config, nil
}

// TokenFilePath returns the token file path for an email within the
// given tokens directory. Use this when you need the path without a
// full Manager instance (e.g., cleanup during account removal).
func TokenFilePath(tokensDir, email string) string {
	safe := sanitizeEmail(email)
	return filepath.Join(tokensDir, safe+".json")
}

type tokenProfileErrorMode int

const (
	tokenProfileErrorOAuth tokenProfileErrorMode = iota
	tokenProfileErrorServiceAccount
)

func fetchTokenProfileEmail(
	ctx context.Context,
	ts oauth2.TokenSource,
	profileURL string,
	email string,
	mode tokenProfileErrorMode,
) (string, error) {
	valCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	client := oauth2.NewClient(valCtx, ts)
	req, err := http.NewRequestWithContext(valCtx, http.MethodGet, profileURL, nil)
	if err != nil {
		return "", fmt.Errorf("create profile request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		if mode == tokenProfileErrorOAuth {
			return "", fmt.Errorf(
				"could not verify token belongs to %s: %w "+
					"(re-run the command to try again)", email, err)
		}
		return "", fmt.Errorf("verify access to %s: %w", email, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if mode == tokenProfileErrorOAuth {
			return "", fmt.Errorf(
				"could not verify token belongs to %s: "+
					"Gmail API returned HTTP %d: %s "+
					"(re-run the command to try again)",
				email, resp.StatusCode, string(body))
		}
		return "", fmt.Errorf("gmail API returned HTTP %d for %s: %s", resp.StatusCode, email, string(body))
	}

	var profile struct {
		EmailAddress string `json:"emailAddress"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		if mode == tokenProfileErrorOAuth {
			return "", fmt.Errorf(
				"could not verify token belongs to %s: "+
					"failed to parse profile response: %w "+
					"(re-run the command to try again)", email, err)
		}
		return "", fmt.Errorf("parse profile for %s: %w", email, err)
	}

	if !sameGoogleAccount(email, profile.EmailAddress) {
		return "", &TokenMismatchError{Expected: email, Actual: profile.EmailAddress}
	}

	return profile.EmailAddress, nil
}

// ValidateTokenEmail calls the Gmail profile API to confirm that the token
// source can access the given email account. Used by service account flows
// where no Manager is available.
func ValidateTokenEmail(ctx context.Context, ts oauth2.TokenSource, email string) error {
	_, err := fetchTokenProfileEmail(ctx, ts, defaultProfileURL, email, tokenProfileErrorServiceAccount)
	return err
}

// DeleteToken removes the token file for the given email.
func (m *Manager) DeleteToken(email string) error {
	path := m.tokenPath(email)
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil // Already gone
	}
	return err
}

// TokenPath returns the path to the token file for an email (for external use).
func (m *Manager) TokenPath(email string) string {
	return m.tokenPath(email)
}
