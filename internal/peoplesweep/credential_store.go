package peoplesweep

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"

	"go.kenn.io/msgvault/internal/providercredentials"
)

var (
	credentialProfileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	ErrCredentialNotFound        = errors.New("people provider credential not found")
)

const (
	legacyCredentialNamespace = "people-providers"
	// Older releases took keys of any size, so read up to the shared store's own limit.
	maxLegacyCredentialBytes = 1 << 20
)

// Credential carries an authentication scheme and an opaque secret. The
// pointer-backed secret prevents fmt's value-%p special case from inspecting
// the underlying string when it bypasses Formatter.
type Credential struct {
	Scheme AuthScheme
	secret *credentialSecret
}

type credentialSecret struct {
	value string
}

// NewCredential constructs an opaque provider credential.
func NewCredential(scheme AuthScheme, value string) Credential {
	return Credential{Scheme: scheme, secret: &credentialSecret{value: value}}
}

// Value returns the secret only for explicit use at the provider boundary.
func (c Credential) Value() string {
	if c.secret == nil {
		return ""
	}
	return c.secret.value
}

func (c Credential) hasValue() bool {
	return c.secret != nil && c.secret.value != ""
}

func (c Credential) String() string {
	return fmt.Sprintf("people provider credential (%s)", c.Scheme)
}

func (c Credential) GoString() string {
	return c.String()
}

// Format prevents supported fmt verbs from inspecting credential internals.
func (c Credential) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

// CredentialStore maps people provider profiles onto the shared provider
// credential store. Revisions cover one credential, absent included.
type CredentialStore interface {
	Revision(profileName, endpoint string) (string, bool, error)
	Load(profileName, endpoint string) (string, error)
	SaveIfRevision(profileName, endpoint, value, expected string) (string, error)
	DeleteIfRevision(profileName, endpoint, expected string) (string, error)
}

var ErrCredentialRevisionConflict = providercredentials.ErrConflict

// CredentialResolver resolves only the source fingerprinted into a profile.
type CredentialResolver interface {
	Resolve(profileName string, profile ProviderProfile) (Credential, error)
}

// credentialFile is the format of legacy <tokens>/people-providers files.
type credentialFile struct {
	Scheme AuthScheme `json:"scheme"`
	Value  string     `json:"value"`
}

// StoredCredentials keeps people provider keys in provider-credentials.json
// under people.provider/<name>, bound to the profile endpoint's origin.
type StoredCredentials struct {
	tokensDir string
}

func NewStoredCredentials(tokensDir string) StoredCredentials {
	return StoredCredentials{tokensDir: tokensDir}
}

func (s StoredCredentials) read(profileName, endpoint string) (providercredentials.Snapshot, string, error) {
	if err := validateCredentialProfileName(profileName); err != nil {
		return providercredentials.Snapshot{}, "", err
	}
	if err := s.importLegacy(profileName, endpoint); err != nil {
		return providercredentials.Snapshot{}, "", err
	}
	snapshot, err := providercredentials.Read(s.tokensDir)
	return snapshot, providercredentials.PeopleProviderID(profileName), err
}

func (s StoredCredentials) Revision(profileName, endpoint string) (string, bool, error) {
	snapshot, id, err := s.read(profileName, endpoint)
	if err != nil {
		return "", false, err
	}
	revision, err := snapshot.Revision(id)
	return revision, snapshot.Stored(id), err
}

func (s StoredCredentials) Load(profileName, endpoint string) (string, error) {
	snapshot, id, err := s.read(profileName, endpoint)
	if err != nil {
		return "", err
	}
	if !snapshot.Stored(id) {
		return "", fmt.Errorf("%w for profile %q", ErrCredentialNotFound, profileName)
	}
	value, _, err := snapshot.Resolve(id, endpoint, "", nil)
	return value, err
}

func (s StoredCredentials) SaveIfRevision(profileName, endpoint, value, expected string) (string, error) {
	_, id, err := s.read(profileName, endpoint)
	if err != nil {
		return "", err
	}
	saved, err := providercredentials.PutIfRevision(s.tokensDir, expected, id, endpoint, value)
	if err != nil {
		return "", err
	}
	return saved.Revision(id)
}

func (s StoredCredentials) DeleteIfRevision(profileName, endpoint, expected string) (string, error) {
	snapshot, id, err := s.read(profileName, endpoint)
	if err != nil {
		return "", err
	}
	if !snapshot.Stored(id) {
		return "", fmt.Errorf("%w for profile %q", ErrCredentialNotFound, profileName)
	}
	// A legacy file that could not be removed would be imported again after the delete.
	if path := s.legacyPath(profileName); fileExists(path) {
		return "", fmt.Errorf("delete people provider credential for profile %q: remove the older key file %s first", profileName, path)
	}
	deleted, err := providercredentials.DeleteIfRevision(s.tokensDir, expected, id)
	if err != nil {
		return "", err
	}
	return deleted.Revision(id)
}

// importLegacy moves a key written by an older release into the shared store,
// then removes the old file. A key already stored wins over the old file, and
// an empty file is how older releases recorded a deleted key.
func (s StoredCredentials) importLegacy(profileName, endpoint string) error {
	path := s.legacyPath(profileName)
	if !fileExists(path) {
		return nil
	}
	load := func() (string, bool, error) {
		raw, err := readLegacyCredential(path)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && len(bytes.TrimSpace(raw)) == 0) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		var stored credentialFile
		decoder := jsontext.NewDecoder(bytes.NewReader(raw), json.RejectUnknownMembers(true))
		if json.UnmarshalDecode(decoder, &stored) != nil || requireCredentialJSONEnd(decoder) != nil {
			return "", false, errors.New("malformed JSON")
		}
		return stored.Value, true, nil
	}
	var retireErr error
	retire := func() error {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			retireErr = err
		}
		return retireErr
	}
	id := providercredentials.PeopleProviderID(profileName)
	_, err := providercredentials.ImportIfAbsent(s.tokensDir, id, endpoint, load, retire)
	if retireErr != nil && errors.Is(err, retireErr) {
		// The shared store already holds the outcome, so the key stays usable.
		slog.Warn("could not remove imported legacy people provider key file", "path", path, "error", retireErr)
		return nil
	}
	if err != nil {
		return fmt.Errorf("import legacy people provider credential %s (left in place): %w", path, err)
	}
	return nil
}

func (s StoredCredentials) legacyPath(profileName string) string {
	return filepath.Join(s.tokensDir, legacyCredentialNamespace, profileName+".json")
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

func readLegacyCredential(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("legacy key file is not a regular file")
	}
	file, err := os.OpenFile(path, legacyCredentialOpenFlags, 0) // #nosec G304 -- validated profile name under the tokens directory.
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // read-only file
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("legacy key file changed while it was read")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxLegacyCredentialBytes+1))
	if err == nil && len(raw) > maxLegacyCredentialBytes {
		return nil, errors.New("legacy key file is too large")
	}
	return raw, err
}

// ValidateProviderProfileName applies the single grammar used by provider
// config, commands, and the private credential namespace.
func ValidateProviderProfileName(profileName string) error {
	if !credentialProfileNamePattern.MatchString(profileName) {
		return errors.New("invalid people provider profile name")
	}
	return nil
}

func validateCredentialProfileName(profileName string) error {
	if err := ValidateProviderProfileName(profileName); err != nil {
		return fmt.Errorf("invalid people provider credential profile name: %w", err)
	}
	return nil
}

func requireCredentialJSONEnd(decoder *jsontext.Decoder) error {
	var extra any
	if err := json.UnmarshalDecode(decoder, &extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("credential file contains multiple JSON values")
		}
		return err
	}
	return nil
}

type credentialResolver struct {
	store  CredentialStore
	lookup CredentialLookup
}

// NewCredentialResolver constructs the sole credential-source dispatcher.
func NewCredentialResolver(store CredentialStore, lookup CredentialLookup) CredentialResolver {
	return &credentialResolver{store: store, lookup: lookup}
}

func (r *credentialResolver) Resolve(profileName string, profile ProviderProfile) (Credential, error) {
	if err := profile.Validate(); err != nil {
		return Credential{}, fmt.Errorf("validate people provider profile before credential resolution: %w", err)
	}
	switch profile.Credential {
	case CredentialStored:
		if profileName != profile.CredentialRef {
			return Credential{}, errors.New("stored people provider credential name does not match the fingerprinted profile")
		}
		if r.store == nil {
			return Credential{}, errors.New("people provider credential store is unavailable")
		}
		value, err := r.store.Load(profileName, profile.Endpoint)
		if err != nil {
			return Credential{}, err
		}
		return NewCredential(profile.Auth, value), nil
	case CredentialEnv:
		if r.lookup == nil {
			return Credential{}, errors.New("people provider credential environment lookup is unavailable")
		}
		value, ok := r.lookup(profile.CredentialRef)
		if !ok || value == "" {
			return Credential{}, fmt.Errorf("people provider credential environment variable %s is not set", profile.CredentialRef)
		}
		return NewCredential(profile.Auth, value), nil
	case CredentialNone:
		return NewCredential(AuthNone, ""), nil
	default:
		return Credential{}, errors.New("people provider profile has an invalid credential source")
	}
}
