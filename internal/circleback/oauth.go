// Package circleback imports meeting notes and transcripts from Circleback
// into the msgvault store. Circleback exposes no REST API; data is pulled
// through its MCP server (Streamable HTTP + OAuth). Each meeting becomes one
// conversation of type "meeting" holding a single "meeting_transcript"
// message.
package circleback

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"go.kenn.io/msgvault/internal/fileutil"
	"golang.org/x/oauth2"
)

const (
	// DefaultEndpoint is Circleback's production MCP server (per their MCP
	// support article; app.circleback.ai/api/mcp serves the same API).
	DefaultEndpoint = "https://circleback.ai/api/mcp"

	// redirectPort deliberately differs from the Microsoft (8089) flow so
	// concurrent authorizations don't collide.
	defaultRedirectPort = "8090"
	callbackPath        = "/callback/circleback"

	// tokenRefreshTimeout bounds a single refresh-grant HTTP request.
	tokenRefreshTimeout = 30 * time.Second
)

// tokenFile is the persisted OAuth state for one Circleback account: the
// token plus everything needed to refresh it later without re-running
// discovery (endpoints, dynamically-registered client credentials, and the
// RFC 8707 resource indicator).
type tokenFile struct {
	Token         oauth2.Token `json:"token"`
	ClientID      string       `json:"client_id"`
	ClientSecret  string       `json:"client_secret,omitempty"`
	AuthEndpoint  string       `json:"auth_endpoint"`
	TokenEndpoint string       `json:"token_endpoint"`
	Resource      string       `json:"resource"`
	Scopes        []string     `json:"scopes,omitempty"`
}

// Manager owns Circleback OAuth token custody: discovery, dynamic client
// registration, the interactive PKCE browser flow, and per-account token
// files under tokensDir (mirroring microsoft.GraphManager).
type Manager struct {
	tokensDir string
	endpoint  string
	http      *http.Client
	logger    *slog.Logger

	// Test hooks.
	redirectPort  string
	openBrowserFn func(ctx context.Context, rawURL string) error
}

// NewManager constructs a Manager. An empty endpoint defaults to production;
// a nil logger defaults to slog.Default().
func NewManager(endpoint, tokensDir string, logger *slog.Logger) *Manager {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		tokensDir:     tokensDir,
		endpoint:      endpoint,
		http:          &http.Client{Timeout: 30 * time.Second},
		logger:        logger,
		redirectPort:  defaultRedirectPort,
		openBrowserFn: openBrowser,
	}
}

// Endpoint returns the MCP endpoint this manager authorizes against.
func (m *Manager) Endpoint() string { return m.endpoint }

// TokenPath returns the on-disk location of the persisted token for an
// account, namespaced with a "circleback_" prefix.
func (m *Manager) TokenPath(identifier string) string {
	return filepath.Join(m.tokensDir, "circleback_"+sanitizeIdentifier(identifier)+".json")
}

// HasToken reports whether a persisted token exists for the account.
func (m *Manager) HasToken(identifier string) bool {
	_, err := os.Stat(m.TokenPath(identifier))
	return err == nil
}

// DeleteToken removes the local Circleback OAuth token file. Missing files
// are not an error. Circleback does not publish a token-revocation endpoint,
// so the refresh token expires according to the authorization server policy.
func (m *Manager) DeleteToken(identifier string) error {
	err := os.Remove(m.TokenPath(identifier))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Authorize runs the full interactive flow: protected-resource discovery,
// auth-server metadata, dynamic client registration (reusing a previously
// registered client when possible), a PKCE auth-code browser flow with a
// localhost callback, and token persistence.
func (m *Manager) Authorize(ctx context.Context, identifier string) error {
	prm, err := m.discoverProtectedResource(ctx)
	if err != nil {
		return err
	}
	if len(prm.AuthorizationServers) == 0 {
		return errors.New("circleback resource metadata lists no authorization servers")
	}
	meta, err := auth.GetAuthServerMetadata(ctx, prm.AuthorizationServers[0], m.http)
	if err != nil {
		return fmt.Errorf("fetch authorization server metadata for %s: %w", prm.AuthorizationServers[0], err)
	}
	if meta == nil {
		return fmt.Errorf("authorization server %s exposes no metadata endpoint", prm.AuthorizationServers[0])
	}

	redirectURL := "http://localhost:" + m.redirectPort + callbackPath
	clientID, clientSecret, err := m.resolveClient(ctx, identifier, meta, prm, redirectURL)
	if err != nil {
		return err
	}

	conf := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:   meta.AuthorizationEndpoint,
			TokenURL:  meta.TokenEndpoint,
			AuthStyle: oauth2.AuthStyleInParams,
		},
		RedirectURL: redirectURL,
		Scopes:      prm.ScopesSupported,
	}

	token, err := m.browserFlow(ctx, conf)
	if err != nil {
		return err
	}

	return m.saveToken(identifier, &tokenFile{
		Token:         *token,
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		AuthEndpoint:  meta.AuthorizationEndpoint,
		TokenEndpoint: meta.TokenEndpoint,
		Resource:      m.endpoint,
		Scopes:        prm.ScopesSupported,
	})
}

// discoverProtectedResource finds the RFC 9728 protected-resource metadata:
// first from the WWW-Authenticate challenge of an unauthenticated request,
// then from the spec's well-known fallback locations.
func (m *Manager) discoverProtectedResource(ctx context.Context) (*oauthex.ProtectedResourceMetadata, error) {
	var candidates []string
	if metaURL := m.challengeMetadataURL(ctx); metaURL != "" {
		candidates = append(candidates, metaURL)
	}
	eu, err := url.Parse(m.endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse circleback endpoint %q: %w", m.endpoint, err)
	}
	// RFC 9728 path-inserted and root well-known locations.
	pathInserted := *eu
	pathInserted.Path = "/.well-known/oauth-protected-resource/" + strings.TrimLeft(eu.Path, "/")
	pathInserted.RawQuery = ""
	root := *eu
	root.Path = "/.well-known/oauth-protected-resource"
	root.RawQuery = ""
	candidates = append(candidates, pathInserted.String(), root.String())

	var lastErr error
	for _, c := range candidates {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, c, m.endpoint, m.http)
		if err == nil {
			return prm, nil
		}
		lastErr = err
		m.logger.Debug("protected resource metadata candidate failed", "url", c, "error", err)
	}
	return nil, fmt.Errorf("discover circleback protected resource metadata: %w", lastErr)
}

// challengeMetadataURL POSTs an unauthenticated request to the MCP endpoint
// and extracts the resource_metadata parameter from the 401 challenge, if
// the server provides one. Best-effort: returns "" when unavailable.
func (m *Manager) challengeMetadataURL(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, strings.NewReader("{}"))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := m.http.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	challenges, err := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	if err != nil {
		return ""
	}
	for _, c := range challenges {
		if u := c.Params["resource_metadata"]; u != "" {
			return u
		}
	}
	return ""
}

// resolveClient reuses the client credentials from a previous registration
// when the token file has them (re-auth shouldn't mint a new client), and
// otherwise performs RFC 7591 dynamic client registration.
func (m *Manager) resolveClient(ctx context.Context, identifier string, meta *oauthex.AuthServerMeta, prm *oauthex.ProtectedResourceMetadata, redirectURL string) (clientID, clientSecret string, err error) {
	if tf, loadErr := m.loadTokenFile(identifier); loadErr == nil && tf.ClientID != "" && tf.TokenEndpoint == meta.TokenEndpoint {
		return tf.ClientID, tf.ClientSecret, nil
	}
	if meta.RegistrationEndpoint == "" {
		return "", "", fmt.Errorf("authorization server %s does not support dynamic client registration", meta.Issuer)
	}
	reg, err := oauthex.RegisterClient(ctx, meta.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
		RedirectURIs:            []string{redirectURL},
		ClientName:              "msgvault",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scope:                   strings.Join(prm.ScopesSupported, " "),
	}, m.http)
	if err != nil {
		return "", "", fmt.Errorf("register msgvault with circleback authorization server: %w", err)
	}
	return reg.ClientID, reg.ClientSecret, nil
}

// browserFlow binds the localhost callback, opens the authorization URL in a
// browser, and exchanges the returned code (PKCE + RFC 8707 resource).
func (m *Manager) browserFlow(ctx context.Context, conf *oauth2.Config) (*oauth2.Token, error) {
	// Bound the whole flow so the callback port doesn't stay bound if the
	// user abandons authorization.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Bind before building the auth URL so the redirect URI is guaranteed
	// to be answerable.
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", "localhost:"+m.redirectPort)
	if err != nil {
		return nil, fmt.Errorf("port %s is already in use — ensure no other process is using it and retry: %w", m.redirectPort, err)
	}

	verifier := oauth2.GenerateVerifier()
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}
	state := base64.URLEncoding.EncodeToString(stateBytes)

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
			_, _ = fmt.Fprint(w, "Error: state mismatch")
			return
		}
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			desc := r.URL.Query().Get("error_description")
			select {
			case errChan <- fmt.Errorf("circleback OAuth error: %s: %s", errMsg, desc):
			default:
			}
			_, _ = fmt.Fprintf(w, "Error: %s", desc) //nolint:gosec // text/plain prevents HTML injection
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			select {
			case errChan <- errors.New("no code in callback"):
			default:
			}
			_, _ = fmt.Fprint(w, "Error: no authorization code received")
			return
		}
		select {
		case codeChan <- code:
		default:
		}
		_, _ = fmt.Fprint(w, "Authorization successful! You can close this window.")
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	authURL := conf.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("resource", m.endpoint),
	)
	fmt.Printf("Opening browser for Circleback authorization...\n")
	fmt.Printf("If the browser doesn't open, visit:\n%s\n\n", authURL)
	if err := m.openBrowserFn(ctx, authURL); err != nil {
		m.logger.Warn("failed to open browser", "error", err)
	}

	select {
	case code := <-codeChan:
		token, err := conf.Exchange(ctx, code,
			oauth2.VerifierOption(verifier),
			oauth2.SetAuthURLParam("resource", m.endpoint),
		)
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

// TokenSource loads the persisted token and returns an auto-refreshing,
// persist-on-refresh oauth2.TokenSource for it. Safe for concurrent use.
func (m *Manager) TokenSource(_ context.Context, identifier string) (oauth2.TokenSource, error) {
	tf, err := m.loadTokenFile(identifier)
	if err != nil {
		return nil, fmt.Errorf("no circleback token for %s (run 'msgvault add-circleback %s' first): %w", identifier, identifier, err)
	}
	return &refreshingTokenSource{mgr: m, identifier: identifier, tf: tf}, nil
}

// refreshingTokenSource returns the stored token while valid and otherwise
// runs the refresh grant, persisting the (possibly rotated) result. The
// refresh grant is implemented manually rather than via oauth2.Config so the
// RFC 8707 resource indicator can be sent — the MCP spec requires it and
// x/oauth2 offers no way to add parameters to refresh requests.
type refreshingTokenSource struct {
	mgr        *Manager
	identifier string

	mu sync.Mutex
	tf *tokenFile
}

func (r *refreshingTokenSource) Token() (*oauth2.Token, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tf.Token.Valid() {
		tok := r.tf.Token
		return &tok, nil
	}
	if r.tf.Token.RefreshToken == "" {
		return nil, fmt.Errorf("circleback token for %s expired and has no refresh token — run 'msgvault add-circleback %s' to re-authorize", r.identifier, r.identifier)
	}
	// context.Background so refreshes are not tied to any one request's ctx;
	// each attempt is bounded by tokenRefreshTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), tokenRefreshTimeout)
	defer cancel()
	tok, err := r.mgr.refreshGrant(ctx, r.tf)
	if err != nil {
		return nil, fmt.Errorf("refresh circleback token for %s: %w (run 'msgvault add-circleback %s' to re-authorize)", r.identifier, err, r.identifier)
	}
	r.tf.Token = *tok
	if err := r.mgr.saveToken(r.identifier, r.tf); err != nil {
		return nil, fmt.Errorf("save refreshed circleback token for %s: %w", r.identifier, err)
	}
	tokCopy := *tok
	return &tokCopy, nil
}

// refreshGrant runs one refresh_token grant against the persisted token
// endpoint, keeping the old refresh token when the server doesn't rotate it.
func (m *Manager) refreshGrant(ctx context.Context, tf *tokenFile) (*oauth2.Token, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tf.Token.RefreshToken},
		"client_id":     {tf.ClientID},
		"resource":      {tf.Resource},
	}
	if tf.ClientSecret != "" {
		form.Set("client_secret", tf.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tf.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if payload.AccessToken == "" {
		return nil, errors.New("token response has no access_token")
	}
	tok := &oauth2.Token{
		AccessToken:  payload.AccessToken,
		TokenType:    payload.TokenType,
		RefreshToken: payload.RefreshToken,
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = tf.Token.RefreshToken
	}
	if payload.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	}
	return tok, nil
}

// Handler returns the auth.OAuthHandler glue for the MCP transport. Its
// Authorize deliberately does NOT start an interactive flow — sync must stay
// daemon-safe — and instead directs the user to add-circleback.
func (m *Manager) Handler(identifier string) auth.OAuthHandler {
	return &oauthHandler{mgr: m, identifier: identifier}
}

type oauthHandler struct {
	mgr        *Manager
	identifier string
}

func (h *oauthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return h.mgr.TokenSource(ctx, h.identifier)
}

func (h *oauthHandler) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	status := "unauthorized"
	if resp != nil {
		status = resp.Status
	}
	return fmt.Errorf("circleback rejected the stored credentials (%s): run 'msgvault add-circleback %s' to re-authorize", status, h.identifier)
}

// saveToken atomically persists the token file with 0600 permissions
// (mirroring microsoft.GraphManager.saveToken).
func (m *Manager) saveToken(identifier string, tf *tokenFile) error {
	if err := fileutil.SecureMkdirAll(m.tokensDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tf, "", "  ") //nolint:gosec // the token file IS the credential store; written 0600
	if err != nil {
		return err
	}
	path := m.TokenPath(identifier)
	tmpFile, err := os.CreateTemp(m.tokensDir, ".circleback-token-*.tmp")
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

func (m *Manager) loadTokenFile(identifier string) (*tokenFile, error) {
	data, err := os.ReadFile(m.TokenPath(identifier))
	if err != nil {
		return nil, err
	}
	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return nil, err
	}
	return &tf, nil
}

// sanitizeIdentifier encodes an account identifier into a filesystem-safe
// filename component. Common identifiers (emails, plain labels) pass through
// unchanged; anything else is percent-encoded so the mapping stays injective
// — a lossy replacement would let identifiers like "team/a" and "team_a"
// share one token file, and one account's authorization would silently
// overwrite another's credentials.
func sanitizeIdentifier(identifier string) string {
	var b strings.Builder
	b.Grow(len(identifier))
	for i := range len(identifier) {
		c := identifier[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '@', c == '_', c == '-':
			b.WriteByte(c)
		default:
			// Percent-encode everything else, including '%' itself, so the
			// mapping is injective: distinct identifiers can never collide
			// on one token file, and no path separator survives.
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// openBrowser opens rawURL in the OS default browser. https-only.
func openBrowser(ctx context.Context, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("refused to open URL with scheme %q (only https is allowed)", parsed.Scheme)
	}
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
		return fmt.Errorf("open browser: %w", err)
	}
	return nil
}
