package x

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/providercredentials"
	"golang.org/x/oauth2"
)

// ClientType selects the OAuth authentication style without probing a second time.
type ClientType string

const (
	PublicClient       ClientType = "public"
	ConfidentialClient ClientType = "confidential"
)

// TokenRecord keeps secrets behind a pointer, including for fmt's value-%p case.
// A newly authorized record is staged at generation zero. Save installs it with
// the next generation; Load returns that persisted generation.
type TokenRecord struct {
	OwnerID       string     `json:"owner_id"`
	Username      string     `json:"username,omitempty"`
	ClientID      string     `json:"client_id"`
	ClientType    ClientType `json:"client_type"`
	Generation    int64      `json:"generation"`
	GrantedScopes []string   `json:"granted_scopes"`
	Expiry        time.Time  `json:"expiry"`
	consumedStage string
	secret        *tokenSecret
}
type tokenSecret struct{ access, refresh, client string }

func NewTokenRecord(ownerID, clientID string, clientType ClientType, access, refresh, secret string) TokenRecord {
	return TokenRecord{OwnerID: ownerID, ClientID: clientID, ClientType: clientType, secret: &tokenSecret{access, refresh, secret}}
}
func (r TokenRecord) AccessToken() string {
	if r.secret == nil {
		return ""
	}
	return r.secret.access
}
func (r TokenRecord) RefreshToken() string {
	if r.secret == nil {
		return ""
	}
	return r.secret.refresh
}
func (r TokenRecord) ClientSecret() string {
	if r.secret == nil {
		return ""
	}
	return r.secret.client
}
func (r TokenRecord) String() string             { return "X credential (secrets redacted)" }
func (r TokenRecord) GoString() string           { return r.String() }
func (r TokenRecord) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, r.String()) }

// tokenFile is used only at the private credential-file boundary.
type tokenFile struct {
	TokenRecord
	Access        string `json:"access_token"`
	Refresh       string `json:"refresh_token"`
	Secret        string `json:"client_secret,omitempty"`
	ConsumedStage string `json:"consumed_stage,omitempty"`
}

// TokenManagerOptions supplies the token endpoint, HTTP policy and expiry clock.
type TokenManagerOptions struct {
	TokenURL   string
	HTTPClient *http.Client
	Now        func() time.Time
}
type TokenManager struct {
	dir     string
	options TokenManagerOptions
}

func NewTokenManager(dir string, options ...TokenManagerOptions) *TokenManager {
	opts := TokenManagerOptions{TokenURL: DefaultTokenURL, Now: time.Now}
	if len(options) > 0 {
		opts = options[0]
		if opts.TokenURL == "" {
			opts.TokenURL = DefaultTokenURL
		}
		if opts.Now == nil {
			opts.Now = time.Now
		}
	}
	return &TokenManager{dir: dir, options: opts}
}

// WithLifecycleLock is the outer per-owner lock for registration and removal.
// Its callback may call Load, Save or Refresh, which take a separate inner lock.
func (m *TokenManager) WithLifecycleLock(ctx context.Context, userID string, fn func() error) error {
	if err := ValidateID(userID); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("missing X credential lifecycle operation")
	}
	return m.withLock(ctx, ".x-lifecycle-"+userID+".lock", fn)
}
func (m *TokenManager) Load(ctx context.Context, userID string) (record TokenRecord, err error) {
	if err = ValidateID(userID); err != nil {
		return record, err
	}
	err = m.withLock(ctx, ".x-store.lock", func() error { var e error; record, e = m.loadLocked(userID); return e })
	return
}

// Save atomically installs credentials. A nonzero Generation must match the
// current file, preventing a stale read from replacing a newer authorization.
// Generation zero explicitly installs a staged authorization.
func (m *TokenManager) Save(ctx context.Context, record TokenRecord) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	return m.withLock(ctx, ".x-store.lock", func() error {
		current, err := m.loadLocked(record.OwnerID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if record.Generation != 0 && record.Generation != current.Generation {
			return errors.New("stale X credential generation")
		}
		if current.Generation == math.MaxInt64 {
			return errors.New("X credential generation exhausted")
		}
		record.Generation = current.Generation + 1
		return m.writeLocked(record)
	})
}

// InstallVerified installs an explicitly verified replacement above the source's
// durable generation floor. The caller verifies the owner and holds source then
// lifecycle ownership; the manager performs no SQL or provider request. The
// expected file generation is always checked, including zero for an absent file.
// Ordinary refresh does not use or retain this installation-only floor.
func (m *TokenManager) InstallVerified(ctx context.Context, record TokenRecord, expectedFileGeneration, generationFloor int64) (TokenRecord, error) {
	if expectedFileGeneration < 0 || generationFloor < 0 {
		return TokenRecord{}, errors.New("invalid X credential installation generation")
	}
	if err := validateRecord(record); err != nil {
		return TokenRecord{}, err
	}
	var installed TokenRecord
	err := m.withLock(ctx, ".x-store.lock", func() error {
		current, err := m.loadLocked(record.OwnerID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if current.Generation != expectedFileGeneration {
			return errors.New("stale X credential generation")
		}
		generation := max(current.Generation, generationFloor)
		if generation == math.MaxInt64 {
			return errors.New("X credential generation exhausted")
		}
		record.Generation = generation + 1
		if err := m.writeLocked(record); err != nil {
			return err
		}
		installed = record
		return nil
	})
	if err != nil {
		return TokenRecord{}, err
	}
	return installed, nil
}

// Refresh proactively refreshes only expiring credentials, after a locked reread.
func (m *TokenManager) Refresh(ctx context.Context, userID string) (TokenRecord, error) {
	return m.refresh(ctx, userID, 0)
}
func (m *TokenManager) refresh(ctx context.Context, userID string, rejected int64) (record TokenRecord, err error) {
	if err = ValidateID(userID); err != nil {
		return record, err
	}
	err = m.withLock(ctx, ".x-store.lock", func() error {
		current, e := m.loadLocked(userID)
		if e != nil {
			return e
		}
		force := rejected != 0 && rejected == current.Generation
		if !force && current.Expiry.After(m.options.Now().Add(30*time.Second)) {
			record = current
			return nil
		}
		if current.Generation == math.MaxInt64 {
			return errors.New("X credential generation exhausted")
		}
		cfg, e := tokenConfig(current, m.options.TokenURL)
		if e != nil {
			return e
		}
		tokenCtx := context.WithValue(ctx, oauth2.HTTPClient, oauthHTTPClient(m.options.HTTPClient))
		// Expiry is deliberately in the past: this is an explicit refresh operation,
		// never an oauth2.Transport or an implicit retry of a resource request.
		token, e := cfg.TokenSource(tokenCtx, &oauth2.Token{RefreshToken: current.RefreshToken(), Expiry: time.Unix(1, 0)}).Token()
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var rejectedGrant *oauth2.RetrieveError
			if errors.As(e, &rejectedGrant) && rejectedGrant.ErrorCode == "invalid_grant" {
				return &InvalidGrantError{OwnerID: current.OwnerID, Generation: current.Generation}
			}
			return errors.New("X token refresh failed")
		}
		next, e := recordFromToken(current, token, m.options.Now(), false)
		if e != nil {
			return e
		}
		next.Generation = current.Generation + 1
		if e = m.writeLocked(next); e != nil {
			return e
		}
		record = next
		return nil
	})
	return
}

// InvalidGrantError disables only the generation used by the failed exchange.
// Callers persist that generation-bound status; the credential store has no SQL.
type InvalidGrantError struct {
	OwnerID    string
	Generation int64
}

func (e *InvalidGrantError) Error() string {
	return "X credential grant revoked; reconnect this credential generation"
}

func (m *TokenManager) tokenPath(userID string) string {
	return filepath.Join(m.dir, "x_"+userID+".json")
}
func (m *TokenManager) loadLocked(userID string) (TokenRecord, error) {
	raw, err := providercredentials.ReadSecretFile(m.tokenPath(userID))
	if err != nil {
		return TokenRecord{}, fmt.Errorf("read X credential: %w", err)
	}
	var stored tokenFile
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return TokenRecord{}, errors.New("invalid X credential file")
	}
	record := stored.TokenRecord
	record.consumedStage = stored.ConsumedStage
	record.secret = &tokenSecret{stored.Access, stored.Refresh, stored.Secret}
	if err := validateRecord(record); err != nil {
		return TokenRecord{}, err
	}
	if record.OwnerID != userID || record.Generation <= 0 {
		return TokenRecord{}, errors.New("X credential file binding or generation mismatch")
	}
	return record, nil
}
func (m *TokenManager) writeLocked(record TokenRecord) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	if err := privatePath(m.tokenPath(record.OwnerID), true); err != nil {
		return err
	}
	data, err := json.Marshal(tokenFile{TokenRecord: record, Access: record.AccessToken(), Refresh: record.RefreshToken(), Secret: record.ClientSecret(), ConsumedStage: record.consumedStage})
	if err != nil {
		return errors.New("cannot encode X credential")
	}
	if len(data) > 64<<10 {
		return errors.New("X credential exceeds file limit")
	}
	if err := fileutil.SecureReplaceFile(m.tokenPath(record.OwnerID), data, 0600); err != nil {
		return fmt.Errorf("persist X credential: %w", err)
	}
	return nil
}
func validateRecord(r TokenRecord) error {
	if !validConsumedStage(r.consumedStage) {
		return errors.New("invalid X consumed-stage marker")
	}
	if err := ValidateID(r.OwnerID); err != nil {
		return err
	}
	if strings.TrimSpace(r.ClientID) == "" || len(r.ClientID) > 4096 || len(r.Username) > 256 || r.Generation < 0 || r.Expiry.IsZero() {
		return errors.New("invalid X credential metadata")
	}
	if r.ClientType != PublicClient && r.ClientType != ConfidentialClient {
		return errors.New("invalid X OAuth client type")
	}
	if r.AccessToken() == "" || r.RefreshToken() == "" || strings.ContainsAny(r.AccessToken(), "\r\n\t ") {
		return errors.New("missing or invalid X credential tokens")
	}
	if r.ClientType == ConfidentialClient && r.ClientSecret() == "" || r.ClientType == PublicClient && r.ClientSecret() != "" {
		return errors.New("X OAuth client secret does not match client type")
	}
	if len(r.GrantedScopes) == 0 {
		return errors.New("missing X granted scopes")
	}
	return nil
}

func (m *TokenManager) withLock(ctx context.Context, name string, fn func() error) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := m.inspectRoot()
	if err != nil {
		return err
	}
	path := filepath.Join(m.dir, name)
	if err := privatePath(path, true); err != nil {
		return err
	}
	lock := flock.New(path, flock.SetPermissions(0600))
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock X credentials: %w", err)
	}
	if !locked {
		return errors.New("cannot lock X credentials")
	}
	defer func() {
		if e := lock.Close(); e != nil && retErr == nil {
			retErr = fmt.Errorf("unlock X credentials: %w", e)
		}
	}()
	after, err := m.inspectRoot()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return errors.New("X credential directory changed while locking")
	}
	if err := privatePath(path, false); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}
func (m *TokenManager) inspectRoot() (os.FileInfo, error) {
	if m.dir == "" {
		return nil, errors.New("missing X credential directory")
	}
	abs, err := filepath.Abs(m.dir)
	if err != nil {
		return nil, err
	}
	// Inspect existing components before creating anything, including ancestors.
	for path := abs; ; path = filepath.Dir(path) {
		info, e := os.Lstat(path)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
		if e == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
			return nil, errors.New("unsafe X credential directory path")
		}
		if filepath.Dir(path) == path {
			break
		}
	}
	if err := fileutil.SecureMkdirAll(m.dir, 0700); err != nil {
		return nil, fmt.Errorf("create X credential directory: %w", err)
	}
	info, err := os.Lstat(m.dir)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("X credential directory permissions must be private")
	}
	return info, nil
}
func privatePath(path string, missingOK bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if missingOK && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("X credential path must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 && info.Mode().Perm() != 0400 {
		return errors.New("X credential file permissions must be 0600 or 0400")
	}
	return nil
}

// CredentialSnapshot contains only the actual grant and its persisted identity.
type CredentialSnapshot struct {
	OwnerID       string
	Generation    int64
	GrantedScopes []string
}

func (s CredentialSnapshot) HasScope(scope string) bool {
	return slices.Contains(s.GrantedScopes, scope)
}

// Credentials separates explicit preflight/GET rejection handling from bearer
// injection. HTTPClient never refreshes, follows redirects, or replays a POST.
type Credentials interface {
	Prepare(context.Context) (CredentialSnapshot, error)
	Refresh(context.Context, int64) (CredentialSnapshot, error)
	HTTPClient() *http.Client
}
type accountCredentials struct {
	manager *TokenManager
	owner   string
	mu      sync.RWMutex
	record  TokenRecord
	client  *http.Client
}

func (m *TokenManager) Credentials(userID string, base *http.Client) Credentials {
	c := &accountCredentials{manager: m, owner: userID}
	c.client = singleRequestHTTPClient(base)
	transport := c.client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	c.client.Transport = &bearerTransport{credentials: c, base: transport}
	return c
}
func (c *accountCredentials) Prepare(ctx context.Context) (CredentialSnapshot, error) {
	return c.prepare(ctx, 0)
}
func (c *accountCredentials) Refresh(ctx context.Context, generation int64) (CredentialSnapshot, error) {
	if generation <= 0 {
		c.mu.Lock()
		c.record = TokenRecord{}
		c.mu.Unlock()
		return CredentialSnapshot{}, errors.New("missing rejected X credential generation")
	}
	return c.prepare(ctx, generation)
}
func (c *accountCredentials) prepare(ctx context.Context, generation int64) (CredentialSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Clear before refreshing so persistence failure cannot leave a usable bearer.
	c.record = TokenRecord{}
	record, err := c.manager.refresh(ctx, c.owner, generation)
	if err != nil {
		return CredentialSnapshot{}, err
	}
	c.record = record
	return CredentialSnapshot{record.OwnerID, record.Generation, slices.Clone(record.GrantedScopes)}, nil
}
func (c *accountCredentials) HTTPClient() *http.Client { return c.client }

type bearerTransport struct {
	credentials *accountCredentials
	base        http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.credentials.mu.RLock()
	access := t.credentials.record.AccessToken()
	t.credentials.mu.RUnlock()
	if access == "" {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errors.New("X credentials require successful preflight")
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+access)
	return t.base.RoundTrip(clone)
}

// BearerClient uses a staged or loaded record for explicit verification only.
// It never loads, refreshes, redirects or replays a request.
func (r TokenRecord) BearerClient(base *http.Client) *http.Client {
	client := singleRequestHTTPClient(base)
	client.Transport = &bearerTransport{credentials: &accountCredentials{record: r}, base: client.Transport}
	return client
}

// Delete consumes only the exact credential generation the caller verified.
// Callers hold the relevant lifecycle/source ownership; absence is idempotent.
func (m *TokenManager) Delete(ctx context.Context, userID string, expectedGeneration int64) error {
	if err := ValidateID(userID); err != nil {
		return err
	}
	if expectedGeneration <= 0 {
		return errors.New("invalid X credential deletion generation")
	}
	return m.withLock(ctx, ".x-store.lock", func() error {
		current, err := m.loadLocked(userID)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Generation != expectedGeneration {
			return errors.New("stale X credential generation")
		}
		if err := os.Remove(m.tokenPath(userID)); err != nil {
			return fmt.Errorf("remove X credential: %w", err)
		}
		return nil
	})
}
