package discord

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/fileutil"
)

var (
	// ErrTokenNotFound indicates that no Discord bot credential matches a
	// requested binding or bot user ID.
	ErrTokenNotFound = errors.New("discord bot token not found")
	// ErrAmbiguousBinding indicates that an unnamed/default binding cannot be
	// resolved without choosing between multiple credentials.
	ErrAmbiguousBinding = errors.New("discord bot token binding is ambiguous")
	// ErrDuplicateBinding indicates that two bot credentials claim the same
	// explicit binding label.
	ErrDuplicateBinding = errors.New("duplicate Discord bot token binding")
)

// TokenRecord is a Discord bot credential and its optional source-binding
// label. AccessToken is intentionally excluded from general JSON encoding;
// TokenManager uses a private on-disk representation for credential storage.
type TokenRecord struct {
	BotUserID   string `json:"bot_user_id"`
	BotUsername string `json:"bot_username"`
	AccessToken string `json:"-"`
	Binding     string `json:"binding,omitempty"`
}

// String returns safe credential metadata without the access token.
func (r TokenRecord) String() string {
	return fmt.Sprintf("Discord bot %s (%s), binding %q", r.BotUsername, r.BotUserID, r.Binding)
}

// GoString protects Go-syntax formatting (%#v) from reflecting the exported
// AccessToken field. A value receiver covers both TokenRecord and
// *TokenRecord formatting.
func (r TokenRecord) GoString() string {
	return r.String()
}

type tokenFile struct {
	BotUserID   string `json:"bot_user_id"`
	BotUsername string `json:"bot_username"`
	AccessToken string `json:"access_token"`
	Binding     string `json:"binding,omitempty"`
}

func (f tokenFile) record() TokenRecord {
	return TokenRecord(f)
}

// TokenManager stores and resolves Discord bot credentials independently of
// the generic OAuth application configuration.
type TokenManager struct {
	tokensDir string
}

// NewTokenManager constructs a Discord token manager rooted at tokensDir.
func NewTokenManager(tokensDir string) *TokenManager {
	return &TokenManager{tokensDir: tokensDir}
}

// TokenPath returns the namespaced credential path for a Discord bot user ID.
func (m *TokenManager) TokenPath(botUserID string) string {
	if value, err := ParseSnowflake(botUserID); err == nil && value != 0 {
		return filepath.Join(m.tokensDir, "discord_"+botUserID+".json")
	}
	// Invalid IDs are never persisted, but a hash fallback keeps this path
	// helper from producing an escaping path if it is used for diagnostics.
	digest := sha256.Sum256([]byte(botUserID))
	return filepath.Join(m.tokensDir, fmt.Sprintf("discord_%x.json", digest))
}

// Save validates and atomically stores a Discord bot credential. Re-saving the
// same bot rotates its token, and changing its empty binding to a named binding
// performs the supported unnamed-to-named promotion.
func (m *TokenManager) Save(record TokenRecord) (retErr error) {
	if err := validateTokenRecord(record); err != nil {
		return err
	}
	rootInfo, err := m.inspectTokenRoot(true)
	if err != nil {
		return err
	}

	lockPath := filepath.Join(m.tokensDir, ".discord-token.lock")
	if err := validateTokenLockPath(lockPath); err != nil {
		return err
	}
	saveLock := flock.New(lockPath, flock.SetPermissions(0600))
	if err := saveLock.Lock(); err != nil {
		return fmt.Errorf("lock Discord token store: %w", err)
	}
	defer func() {
		if err := saveLock.Unlock(); err != nil && retErr == nil {
			retErr = fmt.Errorf("unlock Discord token store: %w", err)
		}
	}()
	if err := m.revalidateTokenRoot(rootInfo); err != nil {
		return err
	}
	if err := validateTokenLockPath(lockPath); err != nil {
		return err
	}

	records, err := m.List()
	if err != nil {
		return err
	}
	foundSameBot := false
	for _, existing := range records {
		if existing.BotUserID == record.BotUserID {
			foundSameBot = true
			if existing.Binding == record.Binding || existing.Binding == "" && record.Binding != "" {
				continue
			}
			return errors.New("discord bot credential binding cannot be changed from one named binding to another")
		}
		if record.Binding != "" && existing.Binding == record.Binding {
			return fmt.Errorf("%w: %q", ErrDuplicateBinding, record.Binding)
		}
	}

	if !foundSameBot && len(records) > 0 {
		if record.Binding == "" {
			return fmt.Errorf("%w: name each credential before adding another bot", ErrAmbiguousBinding)
		}
		for _, existing := range records {
			if existing.Binding == "" {
				return fmt.Errorf("%w: promote the existing default credential before adding another bot", ErrAmbiguousBinding)
			}
		}
	}

	return m.writeLocked(record)
}

// List loads all Discord bot credentials, validates their contents and
// filenames, and returns them ordered by bot user ID.
func (m *TokenManager) List() ([]TokenRecord, error) {
	entries, err := os.ReadDir(m.tokensDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read Discord tokens directory: %w", err)
	}

	records := make([]TokenRecord, 0)
	bindings := make(map[string]string)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "discord_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil, fmt.Errorf("invalid Discord token file %s", entry.Name())
		}

		path := filepath.Join(m.tokensDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read Discord token file %s: %w", entry.Name(), err)
		}
		var stored tokenFile
		if err := json.Unmarshal(data, &stored); err != nil {
			return nil, fmt.Errorf("parse Discord token file %s: %w", entry.Name(), err)
		}
		record := stored.record()
		if err := validateTokenRecord(record); err != nil {
			return nil, fmt.Errorf("validate Discord token file %s: %w", entry.Name(), err)
		}
		if filepath.Base(m.TokenPath(record.BotUserID)) != entry.Name() {
			return nil, fmt.Errorf("discord token file %s does not match its bot user ID", entry.Name())
		}
		if record.Binding != "" {
			if _, exists := bindings[record.Binding]; exists {
				return nil, fmt.Errorf("%w: %q", ErrDuplicateBinding, record.Binding)
			}
			bindings[record.Binding] = record.BotUserID
		}
		records = append(records, record)
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].BotUserID < records[j].BotUserID
	})
	return records, nil
}

// Resolve returns the credential for an exact explicit binding. An empty
// binding resolves only when exactly one credential exists.
func (m *TokenManager) Resolve(binding string) (TokenRecord, error) {
	records, err := m.List()
	if err != nil {
		return TokenRecord{}, err
	}
	if binding == "" {
		switch len(records) {
		case 0:
			return TokenRecord{}, ErrTokenNotFound
		case 1:
			return records[0], nil
		default:
			return TokenRecord{}, fmt.Errorf("%w: source has no binding but %d credentials exist", ErrAmbiguousBinding, len(records))
		}
	}

	for _, record := range records {
		if record.Binding == binding {
			return record, nil
		}
	}
	return TokenRecord{}, fmt.Errorf("%w for binding %q", ErrTokenNotFound, binding)
}

// Delete removes the credential for botUserID while holding the same
// cross-process lock used by Save. Invalid IDs and malformed token stores fail
// closed so account removal cannot delete an unintended path or guess through
// ambiguous credential state.
func (m *TokenManager) Delete(botUserID string) (retErr error) {
	value, err := ParseSnowflake(botUserID)
	if err != nil || value == 0 {
		return errors.New("discord bot credential has an invalid bot user ID")
	}
	rootInfo, err := m.inspectTokenRoot(false)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	lockPath := filepath.Join(m.tokensDir, ".discord-token.lock")
	if err := validateTokenLockPath(lockPath); err != nil {
		return err
	}
	storeLock := flock.New(lockPath, flock.SetPermissions(0600))
	if err := storeLock.Lock(); err != nil {
		return fmt.Errorf("lock Discord token store: %w", err)
	}
	defer func() {
		if err := storeLock.Unlock(); err != nil && retErr == nil {
			retErr = fmt.Errorf("unlock Discord token store: %w", err)
		}
	}()
	if err := m.revalidateTokenRoot(rootInfo); err != nil {
		return err
	}
	if err := validateTokenLockPath(lockPath); err != nil {
		return err
	}

	records, err := m.List()
	if err != nil {
		return err
	}
	found := false
	for _, record := range records {
		if record.BotUserID == botUserID {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	path := m.TokenPath(botUserID)
	if filepath.Dir(path) != filepath.Clean(m.tokensDir) || filepath.Base(path) != "discord_"+botUserID+".json" {
		return errors.New("discord bot credential path failed validation")
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove Discord bot credential: %w", err)
	}
	return nil
}

// WithLifecycleLock serializes source registration/removal decisions that
// span both the Discord credential store and source rows. Callers must acquire
// this outer lock before TokenManager methods, which take the inner store lock.
func (m *TokenManager) WithLifecycleLock(fn func() error) (retErr error) {
	if fn == nil {
		return errors.New("discord credential lifecycle operation is missing")
	}
	rootInfo, err := m.inspectTokenRoot(true)
	if err != nil {
		return err
	}
	lockPath := filepath.Join(m.tokensDir, ".discord-lifecycle.lock")
	if err := validateTokenLockPath(lockPath); err != nil {
		return err
	}
	lifecycleLock := flock.New(lockPath, flock.SetPermissions(0600))
	if err := lifecycleLock.Lock(); err != nil {
		return fmt.Errorf("lock Discord credential lifecycle: %w", err)
	}
	defer func() {
		if err := lifecycleLock.Unlock(); err != nil && retErr == nil {
			retErr = fmt.Errorf("unlock Discord credential lifecycle: %w", err)
		}
	}()
	if err := m.revalidateTokenRoot(rootInfo); err != nil {
		return err
	}
	if err := validateTokenLockPath(lockPath); err != nil {
		return err
	}
	return fn()
}

func (m *TokenManager) inspectTokenRoot(create bool) (os.FileInfo, error) {
	if create {
		if err := fileutil.SecureMkdirAll(m.tokensDir, 0o700); err != nil {
			return nil, fmt.Errorf("create Discord tokens directory: %w", err)
		}
	}
	info, err := os.Lstat(m.tokensDir)
	if err != nil {
		return nil, fmt.Errorf("inspect Discord tokens directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("discord tokens path is not a direct directory")
	}
	return info, nil
}

func (m *TokenManager) revalidateTokenRoot(before os.FileInfo) error {
	after, err := m.inspectTokenRoot(false)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return errors.New("discord tokens directory changed while locking")
	}
	return nil
}

func validateTokenLockPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect Discord credential lock: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("discord credential lock is not a regular file")
	}
	return nil
}

// Promote assigns a label to an unnamed credential. Promotion is idempotent
// when the same bot already has the requested label.
func (m *TokenManager) Promote(botUserID, binding string) (TokenRecord, error) {
	if binding == "" {
		return TokenRecord{}, errors.New("discord bot token promotion requires a binding")
	}
	records, err := m.List()
	if err != nil {
		return TokenRecord{}, err
	}
	for _, record := range records {
		if record.BotUserID != botUserID {
			continue
		}
		if record.Binding == binding {
			return record, nil
		}
		if record.Binding != "" {
			return TokenRecord{}, errors.New("discord bot credential already has a named binding")
		}
		record.Binding = binding
		if err := m.Save(record); err != nil {
			return TokenRecord{}, err
		}
		return record, nil
	}
	return TokenRecord{}, ErrTokenNotFound
}

// writeLocked persists record while Save holds the cross-process store lock.
func (m *TokenManager) writeLocked(record TokenRecord) error {
	stored := tokenFile(record)
	data, err := json.MarshalIndent(stored, "", "  ") //nolint:gosec // the token file is the 0600 credential store
	if err != nil {
		return fmt.Errorf("serialize Discord bot credential: %w", err)
	}

	tmpFile, err := os.CreateTemp(m.tokensDir, ".discord-token-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary Discord token file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temporary Discord token file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temporary Discord token file: %w", err)
	}
	if err := fileutil.SecureChmod(tmpPath, 0600); err != nil {
		return fmt.Errorf("protect temporary Discord token file: %w", err)
	}
	if err := os.Rename(tmpPath, m.TokenPath(record.BotUserID)); err != nil {
		return fmt.Errorf("replace Discord token file: %w", err)
	}
	return nil
}

func validateTokenRecord(record TokenRecord) error {
	value, err := ParseSnowflake(record.BotUserID)
	if err != nil || value == 0 {
		return errors.New("discord bot credential has an invalid bot user ID")
	}
	if record.BotUsername == "" {
		return errors.New("discord bot credential is missing its bot username")
	}
	if record.AccessToken == "" {
		return errors.New("discord bot credential is missing its access token")
	}
	return nil
}
