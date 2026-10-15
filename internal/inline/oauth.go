package inline

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
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
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/fileutil"
	"golang.org/x/oauth2"
)

const (
	MCPResource        = "https://mcp.inline.chat"
	defaultOAuthIssuer = "https://api.inline.chat"
	inlineCallbackPath = "/callback/inline"
	credentialVersion  = 1
)

var inlineReadScopes = []string{"messages:read", "offline_access"}

// OAuthCredentials is an owner-only handoff document. Never log it or expose it
// through source metadata. The daemon verifies account.me before importing it
// under the canonical account identifier.
type OAuthCredentials struct {
	Version       int          `json:"version"`
	Endpoint      string       `json:"endpoint"`
	Token         oauth2.Token `json:"token"`
	ClientID      string       `json:"client_id"`
	AuthEndpoint  string       `json:"auth_endpoint"`
	TokenEndpoint string       `json:"token_endpoint"`
	Resource      string       `json:"resource"`
	Scopes        []string     `json:"scopes"`
}

// OAuthManager owns delegated credentials, never a first-party Inline token.
// Sync only refreshes stored credentials; browser consent is an explicit setup
// operation. File locks serialize rotation across CLI and daemon processes.
type OAuthManager struct {
	endpoint, tokensDir string
	http                *http.Client
	logger              *slog.Logger
	redirectPort        string
	openBrowserFn       func(context.Context, string) error
}

func NewOAuthManager(endpoint, tokensDir string, logger *slog.Logger) *OAuthManager {
	if endpoint == "" {
		endpoint = DefaultMCPEndpoint
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &OAuthManager{endpoint: endpoint, tokensDir: tokensDir, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, logger: logger, redirectPort: "8091", openBrowserFn: openInlineBrowser}
}

func (m *OAuthManager) Endpoint() string { return m.endpoint }

func (m *OAuthManager) TokenPath(identifier string) string {
	key := sha256.Sum256([]byte(identifier))
	return filepath.Join(m.tokensDir, fmt.Sprintf("inline_%x.json", key))
}

func (m *OAuthManager) HasToken(identifier string) bool {
	_, err := os.Stat(m.TokenPath(identifier))
	return err == nil
}

func (m *OAuthManager) issuer() (string, error) {
	u, err := validateMCPEndpoint(m.endpoint)
	if err != nil {
		return "", err
	}
	if u.Scheme == "https" {
		return defaultOAuthIssuer, nil
	}
	return u.Scheme + "://" + u.Host, nil
}

func (m *OAuthManager) validateCredentials(c *OAuthCredentials) error {
	issuer, err := m.issuer()
	if err != nil {
		return err
	}
	if c.Version != credentialVersion || c.Endpoint != m.endpoint || c.Resource != MCPResource || c.ClientID == "" || len(c.ClientID) > 512 || c.AuthEndpoint != issuer+"/oauth/authorize" || c.TokenEndpoint != issuer+"/oauth/token" {
		return errors.New("inline credentials have an invalid endpoint, client, version, or audience")
	}
	if !slices.Contains(c.Scopes, "messages:read") {
		return errors.New("inline credentials lack messages:read")
	}
	for _, scope := range c.Scopes {
		if !slices.Contains(inlineReadScopes, scope) {
			return errors.New("inline archive credentials must contain only messages:read and offline_access")
		}
	}
	if c.Token.AccessToken == "" || c.Token.RefreshToken == "" || !strings.EqualFold(c.Token.TokenType, "Bearer") || c.Token.Expiry.IsZero() {
		return errors.New("inline credentials lack a bounded bearer token or refresh token")
	}
	return nil
}

func (m *OAuthManager) decodeCredentials(payload []byte) (*OAuthCredentials, error) {
	if len(payload) > 1<<20 {
		return nil, errors.New("inline credential document is too large")
	}
	var c OAuthCredentials
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, errors.New("malformed Inline credential document")
	}
	if err := m.validateCredentials(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// AuthorizePayload performs local loopback PKCE without creating any token
// file. The returned document is transferred to the trusted daemon, where the
// principal is verified before canonical account custody is established.
func (m *OAuthManager) AuthorizePayload(ctx context.Context) ([]byte, error) {
	issuer, err := m.issuer()
	if err != nil {
		return nil, err
	}
	var meta struct {
		Issuer           string   `json:"issuer"`
		Authorization    string   `json:"authorization_endpoint"`
		Token            string   `json:"token_endpoint"`
		Registration     string   `json:"registration_endpoint"`
		Scopes           []string `json:"scopes_supported"`
		ChallengeMethods []string `json:"code_challenge_methods_supported"`
	}
	if err := m.requestJSON(ctx, http.MethodGet, issuer+"/.well-known/oauth-authorization-server", nil, &meta); err != nil {
		return nil, fmt.Errorf("discover Inline OAuth: %w", err)
	}
	if meta.Issuer != issuer || meta.Authorization != issuer+"/oauth/authorize" || meta.Token != issuer+"/oauth/token" || meta.Registration != issuer+"/oauth/register" || !slices.Contains(meta.ChallengeMethods, "S256") {
		return nil, errors.New("inline authorization server metadata has an unsupported issuer, endpoint, or PKCE method")
	}
	for _, scope := range inlineReadScopes {
		if !slices.Contains(meta.Scopes, scope) {
			return nil, errors.New("inline authorization server does not advertise the required read scopes")
		}
	}
	flowCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	ln, err := (&net.ListenConfig{}).Listen(flowCtx, "tcp", "127.0.0.1:"+m.redirectPort)
	if err != nil {
		return nil, fmt.Errorf("listen for Inline OAuth callback: %w", err)
	}
	defer func() { _ = ln.Close() }()
	redirect := "http://" + ln.Addr().String() + inlineCallbackPath
	var reg struct {
		ClientID     string   `json:"client_id"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	body, err := json.Marshal(map[string]any{"redirect_uris": []string{redirect}, "client_name": "msgvault", "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "scope": strings.Join(inlineReadScopes, " ")})
	if err != nil {
		return nil, err
	}
	if err := m.requestJSON(flowCtx, http.MethodPost, meta.Registration, body, &reg); err != nil {
		return nil, fmt.Errorf("register Inline OAuth client: %w", err)
	}
	if reg.ClientID == "" || (reg.AuthMethod != "" && reg.AuthMethod != "none") || !slices.Contains(reg.RedirectURIs, redirect) {
		return nil, errors.New("inline OAuth registration did not confirm the public client callback")
	}
	verifier := oauth2.GenerateVerifier()
	var stateBytes [32]byte
	if _, err := rand.Read(stateBytes[:]); err != nil {
		return nil, fmt.Errorf("generate Inline OAuth state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes[:])
	conf := oauth2.Config{ClientID: reg.ClientID, RedirectURL: redirect, Endpoint: oauth2.Endpoint{AuthURL: meta.Authorization, TokenURL: meta.Token, AuthStyle: oauth2.AuthStyleInParams}, Scopes: inlineReadScopes}
	authURL := conf.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("resource", MCPResource))
	code, err := m.waitForCode(flowCtx, ln, authURL, state)
	if err != nil {
		return nil, err
	}
	token, scopes, err := m.exchange(flowCtx, meta.Token, url.Values{"grant_type": {"authorization_code"}, "client_id": {reg.ClientID}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {verifier}, "resource": {MCPResource}}, "")
	if err != nil {
		return nil, err
	}
	c := &OAuthCredentials{Version: credentialVersion, Endpoint: m.endpoint, Token: *token, ClientID: reg.ClientID, AuthEndpoint: meta.Authorization, TokenEndpoint: meta.Token, Resource: MCPResource, Scopes: scopes}
	if err := m.validateCredentials(c); err != nil {
		return nil, err
	}
	return json.Marshal(c, json.Deterministic(true))
}

func (m *OAuthManager) waitForCode(ctx context.Context, ln net.Listener, authURL, state string) (string, error) {
	type callback struct {
		code string
		err  error
	}
	ch := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(inlineCallbackPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet || r.URL.Query().Get("state") != state {
			http.Error(w, "Invalid authorization callback.", http.StatusBadRequest)
			return
		}
		var response callback
		if r.URL.Query().Get("error") != "" {
			response.err = errors.New("inline authorization was declined")
		} else {
			response.code = r.URL.Query().Get("code")
			if response.code == "" {
				response.err = errors.New("inline authorization callback lacks a code")
			}
		}
		select {
		case ch <- response:
		default:
		}
		_, _ = fmt.Fprint(w, "Authorization received. You can close this window.")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case ch <- callback{err: err}:
			default:
			}
		}
	}()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Printf("Authorize Inline in your browser:\n%s\n", authURL)
	if err := m.openBrowserFn(ctx, authURL); err != nil {
		m.logger.Warn("could not open Inline authorization browser; use the displayed URL")
	}
	select {
	case response := <-ch:
		return response.code, response.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (m *OAuthManager) requestJSON(ctx context.Context, method, endpoint string, body []byte, dest any) error {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("OAuth endpoint returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("OAuth response is too large")
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return errors.New("malformed OAuth response")
	}
	return nil
}

// exchange never includes a provider response body in errors: even failure
// bodies may contain credentials. The resource indicator is sent on both
// authorization-code exchange and refresh rotation.
func (m *OAuthManager) exchange(ctx context.Context, endpoint string, form url.Values, oldRefresh string) (*oauth2.Token, []string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("inline token endpoint returned HTTP %d; reauthorize with add-inline if the grant was revoked", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > 1<<20 {
		return nil, nil, errors.New("inline token response is too large")
	}
	var v struct {
		Access  string  `json:"access_token"`
		Refresh string  `json:"refresh_token"`
		Type    string  `json:"token_type"`
		Expires int64   `json:"expires_in"`
		Scope   *string `json:"scope"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, nil, errors.New("malformed Inline token response")
	}
	if v.Access == "" || !strings.EqualFold(v.Type, "Bearer") || v.Expires <= 0 || v.Expires > 7*24*60*60 {
		return nil, nil, errors.New("inline token response lacks a bounded bearer token")
	}
	if v.Refresh == "" {
		v.Refresh = oldRefresh
	}
	if v.Refresh == "" {
		return nil, nil, errors.New("inline token response lacks a refresh token")
	}
	scopes := slices.Clone(inlineReadScopes)
	if v.Scope != nil {
		scopes = strings.Fields(*v.Scope)
	}
	if !slices.Contains(scopes, "messages:read") {
		return nil, nil, errors.New("inline token response lacks messages:read")
	}
	for _, scope := range scopes {
		if !slices.Contains(inlineReadScopes, scope) {
			return nil, nil, errors.New("inline token response granted an unrequested scope")
		}
	}
	return &oauth2.Token{AccessToken: v.Access, RefreshToken: v.Refresh, TokenType: "Bearer", Expiry: time.Now().Add(time.Duration(v.Expires) * time.Second)}, scopes, nil
}

// HTTPClientFromPayload is for bounded account verification before import. It
// cannot refresh or persist credentials, so a setup exchange never rotates an
// unowned token out from underneath the credential handoff.
func (m *OAuthManager) HTTPClientFromPayload(_ context.Context, payload []byte) (*http.Client, error) {
	c, err := m.decodeCredentials(payload)
	if err != nil {
		return nil, err
	}
	if !c.Token.Valid() {
		return nil, errors.New("inline authorization payload expired; authorize again")
	}
	return m.authenticatedClient(oauth2.StaticTokenSource(&c.Token)), nil
}

func (m *OAuthManager) authenticatedClient(source oauth2.TokenSource) *http.Client {
	endpoint, _ := url.Parse(m.endpoint)
	// oauth2.NewClient wraps Source with ReuseTokenSource, which would hide
	// removal or reauthorization behind its cached valid access token. Our
	// source reloads the authoritative file on every request instead.
	client := &http.Client{Timeout: 30 * time.Second, Transport: &inlineOriginTransport{
		base:   &oauth2.Transport{Base: m.http.Transport, Source: source},
		origin: endpoint.Scheme + "://" + endpoint.Host, path: endpoint.Path,
	}}
	// Preserve no-redirect behavior. A delegated MCP bearer token must never be
	// attached to a redirected CDN/media request or another origin.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

type inlineOriginTransport struct {
	base         http.RoundTripper
	origin, path string
}

func (t *inlineOriginTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme+"://"+req.URL.Host != t.origin || req.URL.Path != t.path || req.URL.User != nil {
		return nil, errors.New("inline bearer credentials may only be sent to their MCP endpoint")
	}
	return t.base.RoundTrip(req)
}

func (m *OAuthManager) HTTPClient(ctx context.Context, identifier string) (*http.Client, error) {
	source, err := m.TokenSource(ctx, identifier)
	if err != nil {
		return nil, err
	}
	return m.authenticatedClient(source), nil
}

func (m *OAuthManager) TokenSource(ctx context.Context, identifier string) (oauth2.TokenSource, error) {
	if identifier == "" {
		return nil, errors.New("inline account identifier is required")
	}
	if err := m.withLock(ctx, identifier, func() error { _, err := m.load(identifier); return err }); err != nil {
		return nil, fmt.Errorf("load Inline credentials; run add-inline to authorize: %w", err)
	}
	return &inlineTokenSource{manager: m, identifier: identifier}, nil
}

type inlineTokenSource struct {
	manager    *OAuthManager
	identifier string
}

func (s *inlineTokenSource) Token() (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var token *oauth2.Token
	err := s.manager.withLock(ctx, s.identifier, func() error {
		c, err := s.manager.load(s.identifier)
		if err != nil {
			return err
		}
		if c.Token.Valid() {
			currentToken := c.Token
			token = &currentToken
			return nil
		}
		refreshed, scopes, err := s.manager.exchange(ctx, c.TokenEndpoint, url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ClientID}, "refresh_token": {c.Token.RefreshToken}, "resource": {c.Resource}}, c.Token.RefreshToken)
		if err != nil {
			return err
		}
		c.Token = *refreshed
		c.Scopes = scopes
		if err := s.manager.save(s.identifier, c); err != nil {
			return fmt.Errorf("persist rotated Inline credential: %w", err)
		}
		token = refreshed
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Reloading under the lock on every call observes removal, reauthorization,
	// and another process's rotation. No stale refresh snapshot is cached.
	if token == nil {
		return nil, errors.New("inline token refresh failed")
	}
	return token, nil
}

func (m *OAuthManager) ImportCredentials(identifier string, payload []byte) error {
	c, err := m.decodeCredentials(payload)
	if err != nil {
		return err
	}
	return m.withLock(context.Background(), identifier, func() error { return m.save(identifier, c) })
}

func (m *OAuthManager) ExportCredentials(identifier string) ([]byte, error) {
	var out []byte
	err := m.withLock(context.Background(), identifier, func() error {
		c, err := m.load(identifier)
		if err != nil {
			return err
		}
		out, err = json.Marshal(c, json.Deterministic(true))
		return err
	})
	return out, err
}

func (m *OAuthManager) DeleteToken(identifier string) error {
	return m.withLock(context.Background(), identifier, func() error {
		err := os.Remove(m.TokenPath(identifier))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

func (m *OAuthManager) withLock(ctx context.Context, identifier string, fn func() error) (err error) {
	if identifier == "" || m.tokensDir == "" {
		return errors.New("inline token directory and account identifier are required")
	}
	if err := fileutil.SecureMkdirAll(m.tokensDir, 0700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lock := flock.New(m.TokenPath(identifier)+".lock", flock.SetPermissions(0600))
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock Inline credential store: %w", err)
	}
	if !locked {
		return errors.New("inline credential store is busy")
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	return fn()
}

func (m *OAuthManager) load(identifier string) (*OAuthCredentials, error) {
	file, err := os.Open(m.TokenPath(identifier))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	return m.decodeCredentials(data)
}

func (m *OAuthManager) save(identifier string, c *OAuthCredentials) error {
	if err := m.validateCredentials(c); err != nil {
		return err
	}
	data, err := json.Marshal(c, json.Deterministic(true))
	if err != nil {
		return errors.New("encode Inline credentials")
	}
	return fileutil.SecureReplaceFile(m.TokenPath(identifier), data, 0600)
}

func openInlineBrowser(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host != "api.inline.chat" {
		return errors.New("refused to open an untrusted Inline authorization URL")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", rawURL) //nolint:gosec // A fixed OS browser program opens the validated Inline HTTPS authorization URL without a shell.
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", rawURL) //nolint:gosec // A fixed OS browser program opens the validated Inline HTTPS authorization URL without a shell.
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", rawURL) //nolint:gosec // A fixed OS browser program opens the validated Inline HTTPS authorization URL without a shell.
	default:
		return errors.New("open the authorization URL in your browser")
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open Inline authorization browser: %w", err)
	}
	return nil
}
