package oauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"

	"go.kenn.io/msgvault/internal/config"
	"golang.org/x/oauth2"
)

// ErrClientConfig marks OAuth client settings a person must fix: invalid
// configuration, a missing client_secrets file, or unparseable client JSON.
var ErrClientConfig = errors.New("OAuth client configuration is unusable")

func NewManagerWithCredentials(ctx context.Context, credentials config.OAuthApp, tokensDir string, commands config.OAuthTokenCommands, logger *slog.Logger, scopes []string) (*Manager, error) {
	var data []byte
	var err error
	if credentials.ClientSecretsCommand != nil {
		data, err = runSecretCommand(ctx, credentials.ClientSecretsCommand, nil, nil)
	} else {
		data, err = os.ReadFile(credentials.ClientSecrets)
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrClientConfig, err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("read client secrets: %w", err)
	}
	parsed, redirects, err := parseClientSecrets(data, scopes)
	if err != nil {
		if credentials.ClientSecretsCommand != nil {
			return nil, fmt.Errorf("parse client secrets: %w: invalid OAuth client JSON from command", ErrClientConfig)
		}
		return nil, fmt.Errorf("parse client secrets: %w: %w", ErrClientConfig, err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{config: parsed, tokensDir: tokensDir, tokenStore: NewTokenStore(tokensDir, commands), logger: logger, webRedirectURIs: redirects}, nil
}

// NewStoredTokenManager allows cleanup/export without reading client credentials.
func NewStoredTokenManager(tokensDir string, commands config.OAuthTokenCommands) *Manager {
	return &Manager{tokensDir: tokensDir, tokenStore: NewTokenStore(tokensDir, commands), config: &oauth2.Config{}, logger: slog.Default()}
}

// WithTokensDir selects another token namespace with the same client credentials.
func (m *Manager) WithTokensDir(tokensDir string) *Manager {
	selected := *m
	selected.tokensDir = tokensDir
	selected.tokenStore = NewTokenStore(tokensDir, m.tokenStore.commands)
	selected.authorizationExpected = nil
	return &selected
}

// TokenOrEquivalentExists checks presence without reading client credentials.
func (m *Manager) TokenOrEquivalentExists(ctx context.Context, email string) (bool, error) {
	if !m.CommandTokens() {
		_, err := os.Lstat(m.TokenPath(email))
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
	} else {
		info, err := m.InspectToken(ctx, email)
		if err != nil {
			return false, err
		}
		if info.Exists {
			return true, nil
		}
	}
	aliases, err := m.EquivalentTokenEmails(ctx, email)
	return len(aliases) > 0, err
}

func (m *Manager) CommandTokens() bool { return m.tokenStore.commands.Enabled() }

// TokenInfo is one checked snapshot for scope and client-identity decisions.
// Missing and malformed/unavailable credentials have distinct results.
type TokenInfo struct {
	snapshot        tokenSnapshot
	ClientMatches   bool
	DifferentClient bool
	Exists          bool
	Scopes          []string
	ClientID        string
}

// ErrInvalidTokenJSON identifies a stored token that cannot be decoded.
var ErrInvalidTokenJSON = errors.New("invalid token JSON")

func (m *Manager) InspectToken(ctx context.Context, email string) (TokenInfo, error) {
	tf, err := m.loadTokenFileContext(ctx, email)
	return m.tokenInfo(tf, err)
}

// SelectedTokenInfo returns the checked snapshot used to choose authorization
// scopes. Authorization still rereads the backend and rejects any change.
func (m *Manager) SelectedTokenInfo(ctx context.Context, email string) (TokenInfo, error) {
	if m.authorizationExpected != nil && m.authorizationExpected.email == email {
		return m.tokenInfo(m.metadataTokenFile(email))
	}
	return m.InspectToken(ctx, email)
}

func (m *Manager) tokenInfo(tf *tokenFile, err error) (TokenInfo, error) {
	if errors.Is(err, os.ErrNotExist) {
		return TokenInfo{}, nil
	}
	if err != nil {
		return TokenInfo{}, err
	}
	return TokenInfo{snapshot: tokenSnapshot{data: tf.snapshot, exists: true}, Exists: true, Scopes: slices.Clone(tf.Scopes), ClientID: tf.ClientID, ClientMatches: tf.ClientID != "" && tf.ClientID == m.config.ClientID, DifferentClient: tf.ClientID != "" && tf.ClientID != m.config.ClientID}, nil
}
func (m *Manager) EquivalentTokenEmails(ctx context.Context, email string) ([]string, error) {
	if !m.CommandTokens() {
		return findEquivalentTokenEmails(m.tokensDir, email), nil
	}
	accounts, err := m.tokenStore.List(ctx)
	if err != nil {
		return nil, err
	}
	var aliases []string
	for _, candidate := range accounts {
		if candidate != email && SameGoogleAccount(email, candidate) {
			aliases = append(aliases, candidate)
		}
	}
	return aliases, nil
}

// ErrTokenUnrefreshable marks a stored token that has expired and has no
// refresh token, so only a new sign-in can replace it.
var ErrTokenUnrefreshable = errors.New("token expired and has no refresh token")

// persistingTokenSource serializes refresh/save and keeps failed saves dirty.
// oauth2 may cache a refreshed token; comparison is against persisted state.
type persistingTokenSource struct {
	mu       sync.Mutex
	manager  *Manager
	source   oauth2.TokenSource
	email    string
	expected *tokenFile
	ctx      context.Context
}

func (s *persistingTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for reloaded := false; ; reloaded = true {
		if s.expected.RefreshToken == "" && !s.expected.Valid() {
			return nil, fmt.Errorf("refresh token: %w", ErrTokenUnrefreshable)
		}
		token, err := s.source.Token()
		if err != nil {
			return nil, fmt.Errorf("refresh token: %w", err)
		}
		prior := s.expected.Token
		if token.AccessToken == prior.AccessToken && token.RefreshToken == prior.RefreshToken && token.TokenType == prior.TokenType && token.Expiry.Equal(prior.Expiry) {
			return token, nil
		}
		scopes := s.expected.Scopes
		if len(scopes) == 0 {
			scopes = s.manager.config.Scopes
		}
		err = s.manager.saveTokenComparedContext(s.ctx, s.email, token, scopes, s.expected)
		if err == nil {
			return token, nil
		}
		if errors.Is(err, ErrTokenChanged) && !reloaded {
			// Another source saved first; continue from its token instead of overwriting a newer grant.
			current, loadErr := s.manager.loadTokenFileContext(s.ctx, s.email)
			// A token from another OAuth client can't be refreshed with this one's credentials.
			if loadErr == nil && current.ClientID != "" && current.ClientID != s.manager.config.ClientID {
				loadErr = ErrTokenChanged
			}
			if loadErr == nil {
				s.expected = current
				token := current.Token // saves rewrite expected in place; the source keeps its own copy
				s.source = s.manager.config.TokenSource(withRefreshHTTPClient(s.ctx), &token)
				continue
			}
			err = loadErr
		}
		if s.manager.CommandTokens() {
			return nil, fmt.Errorf("save refreshed token: %w", err)
		}
		// Preserve the file backend's best-effort refresh behavior.
		if !errors.Is(err, ErrTokenChanged) {
			s.manager.logger.Warn("failed to save refreshed token", "email", s.email, "error", err)
		}
		return token, nil
	}
}

func (i TokenInfo) HasScope(scope string) bool {
	return slices.Contains(i.Scopes, canonicalScope(scope))
}

// EquivalentGrantInUse preserves a shared grant when remaining aliases use it.
// Any unreadable credential aborts the decision, so cleanup keeps the grant.
func (m *Manager) EquivalentGrantInUse(ctx context.Context, email string, remaining []string) (bool, error) {
	removed, err := m.InspectToken(ctx, email)
	if err != nil {
		return false, err
	}
	if !removed.Exists {
		return false, nil
	}
	for _, candidate := range remaining {
		if !SameGoogleAccount(email, candidate) {
			continue
		}
		info, err := m.InspectToken(ctx, candidate)
		if err != nil {
			return false, err
		}
		if info.Exists && (removed.ClientID == "" || info.ClientID == "" || removed.ClientID == info.ClientID) {
			return true, nil
		}
	}
	return false, nil
}

type tokenExpectation struct {
	email    string
	snapshot tokenSnapshot
}

// WithTokenInfo binds the next authorization to the snapshot that selected
// its scopes. A newer grant requires rebuilding the scope decision.
func (m *Manager) WithTokenInfo(email string, info TokenInfo) *Manager {
	scoped := *m
	if m.CommandTokens() {
		scoped.authorizationExpected = &tokenExpectation{email: email, snapshot: info.snapshot}
	}
	return &scoped
}
