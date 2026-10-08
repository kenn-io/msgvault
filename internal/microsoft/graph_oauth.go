package microsoft

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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
	// Channel imports read the team roster (GET /teams/{id}/members) to
	// evaluate the participant threshold; Team.ReadBasic.All does not cover it.
	scopeGraphTeamMemberRead = "https://graph.microsoft.com/TeamMember.Read.All"
	// Private and shared channels carry their own membership, read via
	// GET /teams/{id}/channels/{id}/members.
	scopeGraphChannelMemberRead = "https://graph.microsoft.com/ChannelMember.Read.All"
	scopeGraphMailRead          = "https://graph.microsoft.com/Mail.Read"
	scopeGraphMailReadWrite     = "https://graph.microsoft.com/Mail.ReadWrite"
	scopeGraphContactsReadWrite = "https://graph.microsoft.com/Contacts.ReadWrite"
)

// GraphScopes returns the OAuth scopes requested for Microsoft Teams ingestion
// via the Graph API. Unlike the IMAP scopes, these are identical for personal
// and organizational accounts.
func GraphScopes() []string {
	return []string{
		scopeGraphChatRead, scopeGraphChannelMessage, scopeGraphTeamReadBasic,
		scopeGraphChannelBasic, scopeGraphUserRead, scopeGraphUserReadBasic,
		scopeGraphTeamMemberRead, scopeGraphChannelMemberRead,
		scopeOfflineAccess, "openid", scopeEmail,
	}
}

// GraphMailScopes returns the OAuth scopes requested for mailbox ingestion via
// the Graph API.
func GraphMailScopes() []string {
	return []string{scopeGraphMailRead, scopeGraphUserRead, scopeOfflineAccess, "openid", scopeEmail}
}

// GraphMailWriteScopes returns the mail scopes plus Mail.ReadWrite, which
// deletion needs. It keeps Mail.Read, so the sync manager still accepts a
// token granted with these scopes.
func GraphMailWriteScopes() []string {
	return append(GraphMailScopes(), scopeGraphMailReadWrite)
}

// GraphContactsScopes returns the OAuth scopes requested for two-way contact
// sync via the Graph API. CardDAV sync can publish at any time, so the write
// scope is requested at sign-in.
func GraphContactsScopes() []string {
	return []string{scopeGraphContactsReadWrite, scopeGraphUserRead, scopeOfflineAccess, "openid", scopeEmail}
}

// GraphManager is a sibling of Manager that runs the same interactive browser
// auth-code flow but requests Microsoft Graph scopes and persists tokens under
// a "teams_" or "msmail_" filename prefix. It deliberately omits the IMAP scope-validation
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
	deviceCode  bool

	// scopes, tokenPrefix and reauthCmd differ per capability: Teams or mail.
	// reauthCmd is a format string that takes the account email.
	scopes      []string
	tokenPrefix string
	reauthCmd   string

	// Test hooks, mirrored onto the internal delegate. See Manager.
	authorityURL    string
	browserFlowFn   func(ctx context.Context, email string, scopes []string) (*oauth2.Token, string, error)
	verifyIDTokenFn func(ctx context.Context, rawIDToken string) (*idTokenClaims, error)
}

// NewGraphManager constructs a GraphManager for Teams. An empty tenantID
// defaults to the multi-tenant "common" endpoint; a nil logger defaults to
// slog.Default().
func NewGraphManager(clientID, tenantID, redirectURI, tokensDir string, logger *slog.Logger) *GraphManager {
	m := newGraphManager(clientID, tenantID, redirectURI, tokensDir, logger)
	m.scopes, m.tokenPrefix, m.reauthCmd = GraphScopes(), "teams_", "msgvault add-teams %s"
	return m
}

// NewGraphMailManager constructs a GraphManager for mailbox ingestion. Its
// tokens are saved under an "msmail_" prefix, apart from the Teams tokens.
func NewGraphMailManager(clientID, tenantID, redirectURI, tokensDir string, logger *slog.Logger) *GraphManager {
	m := newGraphManager(clientID, tenantID, redirectURI, tokensDir, logger)
	m.scopes, m.tokenPrefix, m.reauthCmd = GraphMailScopes(), "msmail_", "msgvault add-o365 %s --graph"
	return m
}

// NewGraphContactsManager constructs a GraphManager for contact sync. Its
// tokens are saved under an "mscontacts_" prefix.
func NewGraphContactsManager(clientID, tenantID, redirectURI, tokensDir string, logger *slog.Logger) *GraphManager {
	m := newGraphManager(clientID, tenantID, redirectURI, tokensDir, logger)
	m.scopes, m.tokenPrefix, m.reauthCmd = GraphContactsScopes(), "mscontacts_", "msgvault carddav authorize-microsoft %s"
	return m
}

// NewGraphMailWriteManager is NewGraphMailManager with GraphMailWriteScopes.
// It shares the "msmail_" token, so Authorize replaces the read-only grant.
func NewGraphMailWriteManager(clientID, tenantID, redirectURI, tokensDir string, logger *slog.Logger) *GraphManager {
	m := NewGraphMailManager(clientID, tenantID, redirectURI, tokensDir, logger)
	m.scopes = GraphMailWriteScopes()
	return m
}

func newGraphManager(clientID, tenantID, redirectURI, tokensDir string, logger *slog.Logger) *GraphManager {
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

// UseDeviceCode makes Authorize sign in with a device code. See
// Manager.UseDeviceCode.
func (m *GraphManager) UseDeviceCode() {
	m.deviceCode = true
}

// delegate builds an internal *Manager used only for its reusable browser-flow
// and ID-token verification logic. Token storage is handled by GraphManager
// itself (with its own prefix), so the delegate's tokensDir is irrelevant.
func (m *GraphManager) delegate() *Manager {
	return &Manager{
		clientID:        m.clientID,
		tenantID:        m.tenantID,
		redirectURI:     m.redirectURI,
		tokensDir:       m.tokensDir,
		logger:          m.logger,
		deviceCode:      m.deviceCode,
		authorityURL:    m.authorityURL,
		browserFlowFn:   m.browserFlowFn,
		verifyIDTokenFn: m.verifyIDTokenFn,
	}
}

// TokenPath returns the on-disk location of the persisted Graph token for an
// account, namespaced with a "teams_" or "msmail_" prefix to keep it distinct
// from the IMAP Manager's "microsoft_" tokens.
func (m *GraphManager) TokenPath(email string) string {
	return filepath.Join(m.tokensDir, m.tokenPrefix+sanitizeEmail(email)+".json")
}

// Authorize runs the interactive browser auth-code flow requesting Graph
// scopes, verifies the returned ID token matches the expected email, and
// persists the token. Unlike Manager.Authorize there is no IMAP scope
// correction step — Graph scopes are identical across account types.
func (m *GraphManager) Authorize(ctx context.Context, email string) error {
	scopes := m.scopes
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
		scopes = m.scopes
	} else if missing := missingScopes(scopes, m.scopes); len(missing) > 0 {
		return nil, fmt.Errorf(
			"token for %s is missing Microsoft Graph scopes %s — run '%s' to re-authorize",
			email, strings.Join(missing, ", "), fmt.Sprintf(m.reauthCmd, email),
		)
	}

	refreshTenant := m.tenantID
	if tf.TenantID != "" {
		refreshTenant = tf.TenantID
	}
	oauthCfg := m.delegate().oauthConfigWithTenant(refreshTenant, scopes)
	// context.Background so refreshes outlive the caller's (sync-scoped) ctx.
	ts := oauthCfg.TokenSource(context.Background(), &tf.Token)
	return refreshingAccessToken(ts, tf, email, "Microsoft Graph", func(tok *oauth2.Token) error {
		return m.saveToken(email, tok, scopes, tf.TenantID)
	}), nil
}

// HasScopes reports whether the saved token was granted every scope this
// manager requests.
func (m *GraphManager) HasScopes(email string) (bool, error) {
	tf, err := m.loadTokenFile(email)
	if err != nil {
		return false, err
	}
	return len(missingScopes(tf.Scopes, m.scopes)) == 0, nil
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
// the IMAP Manager (tokenFile), under the capability's filename prefix.
func (m *GraphManager) saveToken(email string, token *oauth2.Token, scopes []string, tenantID string) error {
	return saveTokenFile(m.tokensDir, m.TokenPath(email), token, scopes, tenantID)
}

func (m *GraphManager) loadTokenFile(email string) (*tokenFile, error) {
	return readTokenFile(m.TokenPath(email))
}

func missingScopes(scopes, want []string) []string {
	have := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		have[scope] = struct{}{}
	}
	var missing []string
	for _, scope := range want {
		if _, ok := have[scope]; !ok {
			missing = append(missing, scope)
		}
	}
	return missing
}
