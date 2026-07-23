package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/fileutil"
	"golang.org/x/oauth2"
)

// Microsoft Graph delegated permission scopes for Teams ingestion.
const (
	scopeGraphChatRead       = "https://graph.microsoft.com/Chat.Read"
	scopeGraphChannelMessage = "https://graph.microsoft.com/ChannelMessage.Read.All"
	scopeGraphTeamReadBasic  = "https://graph.microsoft.com/Team.ReadBasic.All"
	scopeGraphChannelBasic   = "https://graph.microsoft.com/Channel.ReadBasic.All"
	scopeGraphUserRead       = "https://graph.microsoft.com/User.Read"
	scopeGraphUserReadBasic  = "https://graph.microsoft.com/User.ReadBasic.All"
)

// GraphScopes returns the OAuth scopes requested for Microsoft Teams ingestion
// via the Graph API. Unlike the IMAP scopes, these are identical for personal
// and organizational accounts.
func GraphScopes() []string {
	return []string{
		scopeGraphChatRead, scopeGraphChannelMessage, scopeGraphTeamReadBasic,
		scopeGraphChannelBasic, scopeGraphUserRead, scopeGraphUserReadBasic,
		scopeOfflineAccess, "openid", scopeEmail,
	}
}

// GraphManager is a sibling of Manager that runs the same interactive browser
// auth-code flow but requests Microsoft Graph scopes and persists tokens under
// a "teams_" filename prefix. It deliberately omits the IMAP scope-validation
// and IMAP-host logic of Manager.
//
// The heavy browser-flow and ID-token verification machinery is reused via an
// internal *Manager delegate; only token storage (filename prefix) and the
// scope set differ. This keeps Manager's external behavior unchanged.
type GraphManager struct {
	clientID    string
	tenantID    string
	redirectURI string
	tokensDir   string
	logger      *slog.Logger

	// Test hooks, mirrored onto the internal delegate. See Manager.
	browserFlowFn   func(ctx context.Context, email string, scopes []string) (*oauth2.Token, string, error)
	verifyIDTokenFn func(ctx context.Context, rawIDToken string) (*idTokenClaims, error)
}

// NewGraphManager constructs a GraphManager. An empty tenantID defaults to the
// multi-tenant "common" endpoint; a nil logger defaults to slog.Default().
func NewGraphManager(clientID, tenantID, redirectURI, tokensDir string, logger *slog.Logger) *GraphManager {
	if tenantID == "" {
		tenantID = DefaultTenant
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &GraphManager{
		clientID:    clientID,
		tenantID:    tenantID,
		redirectURI: redirectURI,
		tokensDir:   tokensDir,
		logger:      logger,
	}
}

// delegate builds an internal *Manager used only for its reusable browser-flow
// and ID-token verification logic. Token storage is handled by GraphManager
// itself (with the teams_ prefix), so the delegate's tokensDir is irrelevant.
func (m *GraphManager) delegate() *Manager {
	return &Manager{
		clientID:        m.clientID,
		tenantID:        m.tenantID,
		redirectURI:     m.redirectURI,
		tokensDir:       m.tokensDir,
		logger:          m.logger,
		browserFlowFn:   m.browserFlowFn,
		verifyIDTokenFn: m.verifyIDTokenFn,
	}
}

// TokenPath returns the on-disk location of the persisted Graph token for an
// account, namespaced with a "teams_" prefix to keep it distinct from the IMAP
// Manager's "microsoft_" tokens.
func (m *GraphManager) TokenPath(email string) string {
	return filepath.Join(m.tokensDir, "teams_"+sanitizeEmail(email)+".json")
}

// Authorize runs the interactive browser auth-code flow requesting Graph
// scopes, verifies the returned ID token matches the expected email, and
// persists the token. Unlike Manager.Authorize there is no IMAP scope
// correction step — Graph scopes are identical across account types.
func (m *GraphManager) Authorize(ctx context.Context, email string) error {
	scopes := GraphScopes()
	d := m.delegate()
	token, nonce, err := d.doBrowserFlow(ctx, email, scopes)
	if err != nil {
		return err
	}
	_, claims, err := d.resolveTokenEmail(ctx, email, token, nonce)
	if err != nil {
		return err
	}
	tenantID := ""
	if claims != nil {
		tenantID = claims.TenantID
	}
	return m.saveToken(email, token, scopes, tenantID)
}

// TokenSource loads the persisted Graph token and returns a function yielding a
// fresh (auto-refreshed) access token. The returned function is safe for
// concurrent use. There is NO IMAP scope validation and NO IMAP-host logic.
//
// Token refresh HTTP requests run against context.Background so they are not
// cancelled if the caller's context expires between calls; each attempt is
// bounded by tokenRefreshTimeout.
func (m *GraphManager) TokenSource(ctx context.Context, email string) (func(context.Context) (string, error), error) {
	tf, err := m.loadTokenFile(email)
	if err != nil {
		return nil, fmt.Errorf("no valid token for %s: %w", email, err)
	}

	scopes := tf.Scopes
	if len(scopes) == 0 {
		scopes = GraphScopes()
	} else if missing := missingGraphScopes(scopes); len(missing) > 0 {
		return nil, fmt.Errorf(
			"token for %s is missing Microsoft Graph scopes %s — run 'msgvault add-teams %s' to re-authorize",
			email, strings.Join(missing, ", "), email,
		)
	}

	refreshTenant := m.tenantID
	if tf.TenantID != "" {
		refreshTenant = tf.TenantID
	}
	oauthCfg := m.delegate().oauthConfigWithTenant(refreshTenant, scopes)
	// context.Background so refreshes outlive the caller's (sync-scoped) ctx.
	ts := oauthCfg.TokenSource(context.Background(), &tf.Token)

	var (
		mu               sync.Mutex
		lastAccessToken  = tf.AccessToken
		lastRefreshToken = tf.RefreshToken
		lastExpiry       = tf.Expiry
	)

	return func(callCtx context.Context) (string, error) {
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
				return "", fmt.Errorf("refresh Microsoft Graph token: %w", res.err)
			}
			tok = res.tok
		case <-timer.C:
			return "", fmt.Errorf("microsoft graph token refresh timed out after %s — check network connectivity", tokenRefreshTimeout)
		case <-callCtx.Done():
			return "", fmt.Errorf("microsoft graph token refresh cancelled: %w", callCtx.Err())
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
				return "", fmt.Errorf("save refreshed microsoft graph token for %s: %w (token refreshed but not persisted — re-run may require re-authorization)", email, saveErr)
			}
		}

		return tok.AccessToken, nil
	}, nil
}

// HasToken reports whether a persisted Graph token exists for the account.
func (m *GraphManager) HasToken(email string) bool {
	_, err := os.Stat(m.TokenPath(email))
	return err == nil
}

// DeleteToken removes the local Graph token file. Missing files are not an
// error. (Graph refresh tokens expire naturally; no remote revocation here.)
func (m *GraphManager) DeleteToken(email string) error {
	err := os.Remove(m.TokenPath(email))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// saveToken atomically persists the token in the same on-disk JSON format as
// the IMAP Manager (tokenFile), under the teams_ filename.
func (m *GraphManager) saveToken(email string, token *oauth2.Token, scopes []string, tenantID string) error {
	if err := fileutil.SecureMkdirAll(m.tokensDir, 0700); err != nil {
		return err
	}

	tf := tokenFile{Token: *token, Scopes: scopes, TenantID: tenantID}
	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}

	path := m.TokenPath(email)
	tmpFile, err := os.CreateTemp(m.tokensDir, ".teams-token-*.tmp")
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

func (m *GraphManager) loadTokenFile(email string) (*tokenFile, error) {
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

func missingGraphScopes(scopes []string) []string {
	have := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		have[scope] = struct{}{}
	}
	var missing []string
	for _, scope := range GraphScopes() {
		if _, ok := have[scope]; !ok {
			missing = append(missing, scope)
		}
	}
	return missing
}
