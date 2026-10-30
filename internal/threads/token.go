package threads

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/fileutil"
)

var (
	ErrReauthorizationRequired = errors.New("Threads reauthorization required")
	ErrTokenNotFound           = errors.New("Threads token not found")
)

// TokenRecord is verified credential metadata with an opaque, pointer-backed
// secret. Only AccessToken exposes the secret; ordinary JSON and fmt do not.
type TokenRecord struct {
	AppID               string    `json:"app_id"`
	OwnerID             string    `json:"owner_id"`
	Username            string    `json:"username,omitempty"`
	Name                string    `json:"name,omitempty"`
	Scopes              []string  `json:"scopes"`
	IssuedAt            time.Time `json:"issued_at"`
	RefreshedAt         time.Time `json:"refreshed_at,omitempty"`
	ExpiresAt           time.Time `json:"expires_at"`
	DataAccessExpiresAt time.Time `json:"data_access_expires_at"`
	RedirectURI         string    `json:"redirect_uri"`
	Generation          uint64    `json:"generation"`
	Invalidated         bool      `json:"invalidated,omitempty"`
	secret              *credentialSecret
}
type credentialSecret struct{ value string }

func (r TokenRecord) AccessToken() string {
	if r.secret == nil {
		return ""
	}
	return r.secret.value
}
func (r TokenRecord) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "Threads credential [redacted]")
}

// tokenMetadata prevents serialization of the formatting methods; the secret
// is added only in the private on-disk representation of a 0600 file.
type tokenMetadata TokenRecord
type threadsTokenFile struct {
	tokenMetadata

	AccessToken string `json:"access_token"`
}

// TokenManager pins the direct directory identity on first use. Replacing or
// symlinking that directory during an operation fails closed, including across
// provider I/O. Separate instances coordinate through the OS file locks.
type TokenManager struct {
	tokensDir string
	rootMu    sync.Mutex
	rootInfo  os.FileInfo
}

func NewTokenManager(tokensDir string) *TokenManager {
	return &TokenManager{tokensDir: filepath.Clean(tokensDir)}
}

// tokenValidID is intentionally private to authentication preparation. It must
// not overlap the read client's ID helpers during coordinator integration.
func tokenValidID(id string) bool {
	if id == "" || len(id) > 128 || id[0] == '0' {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func tokenSourceIDs(source string) (string, string, error) {
	parts := strings.Split(source, ":")
	if len(parts) != 3 || parts[0] != "threads" || !tokenValidID(parts[1]) || !tokenValidID(parts[2]) {
		return "", "", errors.New("invalid Threads source identifier")
	}
	return parts[1], parts[2], nil
}
func tokenFilename(appID, ownerID string) (string, error) {
	if !tokenValidID(appID) || !tokenValidID(ownerID) {
		return "", errors.New("invalid Threads credential identity")
	}
	name := "threads_" + appID + "_" + ownerID + ".json"
	if len(name) > 200 {
		name = fmt.Sprintf("threads_sha256_%x.json", sha256.Sum256([]byte("threads:"+appID+":"+ownerID)))
	}
	return name, nil
}

func (m *TokenManager) inspectRoot(create bool) error {
	m.rootMu.Lock()
	defer m.rootMu.Unlock()
	if create && m.rootInfo == nil {
		if err := fileutil.SecureMkdirAll(m.tokensDir, 0700); err != nil {
			return errors.New("create Threads token directory")
		}
	}
	info, err := os.Lstat(m.tokensDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return errors.New("inspect Threads token directory")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Threads token path is not a direct directory")
	}
	if m.rootInfo != nil && !os.SameFile(m.rootInfo, info) {
		return errors.New("Threads token directory changed")
	}
	m.rootInfo = info
	return nil
}
func tokenRegularPath(path string, missingOK bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && missingOK {
		return nil
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return errors.New("inspect Threads credential file")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Threads credential path is not a regular file")
	}
	return nil
}

func (m *TokenManager) withFileLock(ctx context.Context, name string, fn func() error) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.inspectRoot(true); err != nil {
		return err
	}
	path := filepath.Join(m.tokensDir, name)
	if err := tokenRegularPath(path, true); err != nil {
		return err
	}
	lock := flock.New(path, flock.SetPermissions(0600))
	ok, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("lock Threads credential file")
	}
	if !ok {
		return errors.New("Threads credential lock unavailable")
	}
	defer func() {
		if err := lock.Unlock(); err != nil && retErr == nil {
			retErr = errors.New("unlock Threads credential file")
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.inspectRoot(false); err != nil {
		return err
	}
	if err := tokenRegularPath(path, false); err != nil {
		return err
	}
	return fn()
}

// WithSourceLock is the context-aware cross-process outer lifecycle lock for
// reconnect, removal, sync and send. Callers recheck source existence/generation
// after acquisition. Order: source lock -> execution lease -> short transaction.
// Release both outer owners before cache refresh or any long throttle wait.
func (m *TokenManager) WithSourceLock(ctx context.Context, source string, fn func() error) error {
	appID, ownerID, err := tokenSourceIDs(source)
	if err != nil {
		return err
	}
	if fn == nil {
		return errors.New("missing Threads lifecycle operation")
	}
	name := ".threads_" + appID + "_" + ownerID + ".lock"
	if len(name) > 200 {
		name = fmt.Sprintf(".threads_source_%x.lock", sha256.Sum256([]byte(source)))
	}
	return m.withFileLock(ctx, name, fn)
}

func (m *TokenManager) Load(appID, ownerID string) (TokenRecord, error) {
	name, err := tokenFilename(appID, ownerID)
	if err != nil {
		return TokenRecord{}, err
	}
	if err = m.inspectRoot(false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TokenRecord{}, ErrTokenNotFound
		}
		return TokenRecord{}, err
	}
	path := filepath.Join(m.tokensDir, name)
	if err = tokenRegularPath(path, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TokenRecord{}, ErrTokenNotFound
		}
		return TokenRecord{}, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return TokenRecord{}, errors.New("inspect Threads token")
	}
	file, err := os.Open(path)
	if err != nil {
		return TokenRecord{}, errors.New("open Threads token")
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return TokenRecord{}, errors.New("Threads token file changed")
	}
	const maxFile = 1 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxFile+1))
	if err != nil || len(data) > maxFile {
		return TokenRecord{}, errors.New("read Threads token")
	}
	if err = m.inspectRoot(false); err != nil {
		return TokenRecord{}, err
	}
	var stored threadsTokenFile
	if err = json.Unmarshal(data, &stored); err != nil {
		return TokenRecord{}, errors.New("invalid Threads credential file")
	}
	r := TokenRecord(stored.tokenMetadata)
	r.secret = &credentialSecret{stored.AccessToken}
	if err = validateThreadsToken(r); err != nil {
		return TokenRecord{}, err
	}
	if r.AppID != appID || r.OwnerID != ownerID || r.Generation == 0 {
		return TokenRecord{}, errors.New("Threads credential file binding mismatch")
	}
	return r, nil
}

// Save atomically creates a verified generation. A zero-generation record is a
// fresh exchange; a nonzero record must match the durable, usable generation.
// Call under WithSourceLock whenever source state is involved. This method uses
// only the distinct inner store lock and never reacquires the lifecycle lock.
func (m *TokenManager) Save(record TokenRecord) error {
	if err := validateThreadsToken(record); err != nil {
		return err
	}
	return m.withFileLock(context.Background(), ".threads-token.lock", func() error {
		current, err := m.Load(record.AppID, record.OwnerID)
		if err != nil && !errors.Is(err, ErrTokenNotFound) {
			return err
		}
		if record.Generation != 0 && (err != nil || record.Generation != current.Generation || current.Invalidated) {
			return errors.New("stale Threads credential generation")
		}
		if current.Generation == ^uint64(0) {
			return errors.New("Threads credential generation exhausted")
		}
		record.Generation = current.Generation + 1
		record.Invalidated = false
		return m.writeToken(record)
	})
}
func validateThreadsToken(r TokenRecord) error {
	if _, err := tokenFilename(r.AppID, r.OwnerID); err != nil {
		return err
	}
	if r.AccessToken() == "" || r.IssuedAt.IsZero() || r.ExpiresAt.IsZero() || r.DataAccessExpiresAt.IsZero() {
		return errors.New("incomplete Threads credential")
	}
	if _, err := oauthCallbackURI(r.RedirectURI); err != nil {
		return err
	}
	for _, required := range []string{"threads_basic", "threads_read_replies"} {
		found := false
		for _, scope := range r.Scopes {
			if scope == required {
				found = true
			}
		}
		if !found {
			return errors.New("Threads credential lacks required granted scopes")
		}
	}
	return nil
}
func (m *TokenManager) writeToken(r TokenRecord) error {
	if err := m.inspectRoot(false); err != nil {
		return err
	}
	name, err := tokenFilename(r.AppID, r.OwnerID)
	if err != nil {
		return err
	}
	path := filepath.Join(m.tokensDir, name)
	if err = tokenRegularPath(path, true); err != nil {
		return err
	}
	stored := threadsTokenFile{tokenMetadata: tokenMetadata(r), AccessToken: r.AccessToken()}
	data, err := json.Marshal(stored)
	if err != nil {
		return errors.New("serialize Threads credential")
	}
	if err = fileutil.SecureReplaceFile(path, data, 0600); err != nil {
		return errors.New("persist Threads credential")
	}
	return m.inspectRoot(false)
}

func (m *TokenManager) Delete(appID, ownerID string) error {
	name, err := tokenFilename(appID, ownerID)
	if err != nil {
		return err
	}
	if err = m.inspectRoot(false); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return m.withFileLock(context.Background(), ".threads-token.lock", func() error {
		path := filepath.Join(m.tokensDir, name)
		if err := tokenRegularPath(path, true); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("remove Threads credential")
		}
		return nil
	})
}

// EnsureCurrent reloads durable state under caller-owned WithSourceLock. It
// deliberately never takes that lifecycle lock again. Every network attempt
// uses opts.HTTPClient's existing transport; no fresh budget is constructed.
func (m *TokenManager) EnsureCurrent(ctx context.Context, source string, opts OAuthClientOptions) (TokenRecord, error) {
	appID, ownerID, err := tokenSourceIDs(source)
	if err != nil {
		return TokenRecord{}, err
	}
	if err = ctx.Err(); err != nil {
		return TokenRecord{}, err
	}
	r, err := m.Load(appID, ownerID)
	if err != nil {
		return TokenRecord{}, err
	}
	now := time.Now()
	if opts.Now != nil {
		now = opts.Now()
	}
	if r.Invalidated || !now.Before(r.ExpiresAt) || !now.Before(r.DataAccessExpiresAt) {
		return TokenRecord{}, ErrReauthorizationRequired
	}
	last := r.IssuedAt
	if r.RefreshedAt.After(last) {
		last = r.RefreshedAt
	}
	age := now.Sub(last)
	if age < 24*time.Hour || (age < 7*24*time.Hour && r.ExpiresAt.After(now.Add(7*24*time.Hour))) {
		return r, nil
	}
	c, err := newOAuthHTTP(opts)
	if err != nil {
		return TokenRecord{}, err
	}
	var replacement oauthTokenResponse
	err = c.request(ctx, "GET", "/refresh_access_token", url.Values{"grant_type": {"th_refresh_token"}, "access_token": {r.AccessToken()}}, &replacement)
	if err == nil {
		err = replacement.validate()
	}
	var verified TokenRecord
	if err == nil {
		verified, err = c.verify(ctx, replacement.AccessToken, replacement.AccessToken, appID, ownerID, r.Scopes)
	}
	if err != nil {
		if errors.Is(err, ErrReauthorizationRequired) {
			if saveErr := m.Invalidate(r); saveErr != nil {
				return TokenRecord{}, saveErr
			}
		}
		return TokenRecord{}, err
	}
	verified.Username = r.Username
	verified.Name = r.Name
	verified.RedirectURI = r.RedirectURI
	verified.Generation = r.Generation
	verified.RefreshedAt = now
	verified.ExpiresAt = minTokenExpiry(verified.ExpiresAt, now.Add(time.Duration(replacement.ExpiresIn)*time.Second))
	if err = m.Save(verified); err != nil {
		return TokenRecord{}, err
	}
	// Return only the durable replacement, never an unsaved in-memory token.
	return m.Load(appID, ownerID)
}

// Invalidate disables only the supplied durable generation after a definite
// provider credential failure, including Graph 190 from ordinary media reads.
// Call under WithSourceLock. The distinct inner lock and generation comparison
// prevent a late failure from disabling a newer verified connection.
func (m *TokenManager) Invalidate(r TokenRecord) error {
	if _, err := tokenFilename(r.AppID, r.OwnerID); err != nil {
		return err
	}
	if r.Generation == 0 {
		return errors.New("Threads invalidation requires a durable generation")
	}
	return m.withFileLock(context.Background(), ".threads-token.lock", func() error {
		current, err := m.Load(r.AppID, r.OwnerID)
		if err != nil {
			return err
		}
		if current.Generation != r.Generation {
			return errors.New("Threads credential changed during invalidation")
		}
		current.Invalidated = true
		return m.writeToken(current)
	})
}
