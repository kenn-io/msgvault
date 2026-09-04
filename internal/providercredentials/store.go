// Package providercredentials stores browser-managed provider credentials in
// an owner-only file separate from config.toml. Values are write-only at the
// HTTP boundary and bound to the origin that may receive them.
package providercredentials

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	Filename                                 = "provider-credentials.json" // #nosec G101 -- filename, not a credential.
	VectorEmbeddingsID                       = "vector.embeddings"
	VectorMultimodalID                       = "vector.multimodal"
	PeopleSweepID                            = "people.sweep"
	PersonEnrichmentSuppressionID            = "people.enrichment/suppression"
	StoredSuppressionEnvironment             = "MSGVAULT_STORED_PERSON_ENRICHMENT_SUPPRESSION_KEY"
	personEnrichmentCredentialIDPrefix       = "people.enrichment/"
	credentialStoreVersion                   = 1
	maximumCredentialStoreBytes        int64 = 1 << 20
)

var (
	ErrConflict       = errors.New("provider credential store changed")
	ErrUnavailable    = errors.New("provider credential store unavailable")
	ErrOriginMismatch = errors.New("stored provider credential is bound to a different endpoint origin")
	providerNameRE    = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
)

type Source string

const (
	SourceNone        Source = "none"
	SourceStored      Source = "stored"
	SourceEnvironment Source = "environment"
)

type State struct {
	Configured bool   `json:"configured"`
	Source     Source `json:"source"`
}

type record struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Origin   string `json:"origin,omitempty"`
	Revision int64  `json:"revision"`
}

type storeFile struct {
	Version     int               `json:"version"`
	Credentials map[string]record `json:"credentials"`
}

// Snapshot is an immutable credential-store read and its independent strong
// ETag. Credentials remain private to this package.
type Snapshot struct {
	ETag        string
	credentials map[string]record
	loadErr     error
}

type permissionBackend interface {
	secureDirectory(path string) error
	verifyDirectory(path string) error
	secureFile(file *os.File) error
	verifyFile(file *os.File) error
}

func PersonEnrichmentID(name string) string {
	return personEnrichmentCredentialIDPrefix + name
}

func ValidateID(id string) error {
	switch id {
	case VectorEmbeddingsID, VectorMultimodalID, PeopleSweepID:
		return nil
	}
	if !strings.HasPrefix(id, personEnrichmentCredentialIDPrefix) {
		return errors.New("unsupported provider credential ID")
	}
	name := strings.TrimPrefix(id, personEnrichmentCredentialIDPrefix)
	if name == "" || name == "suppression" || !providerNameRE.MatchString(name) {
		return errors.New("invalid person-enrichment provider credential ID")
	}
	return nil
}

// EndpointOrigin returns the only destination identity stored with a secret.
// It rejects URL components commonly abused to smuggle credentials.
func EndpointOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil {
		return "", errors.New("provider endpoint must be an http or https URL with a host")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if parsed.Host == "" || (scheme != "http" && scheme != "https") {
		return "", errors.New("provider endpoint must be an http or https URL with a host")
	}
	if parsed.User != nil {
		return "", errors.New("provider endpoint must not contain credentials")
	}
	if parsed.RawQuery != "" {
		return "", errors.New("provider endpoint must not contain a query")
	}
	if parsed.Fragment != "" {
		return "", errors.New("provider endpoint must not contain a fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host, nil
}

func emptyStore() storeFile {
	return storeFile{Version: credentialStoreVersion, Credentials: map[string]record{}}
}

func Read(tokenDir string) (Snapshot, error) {
	return readWithPermissions(tokenDir, nativePermissions{})
}

func readWithPermissions(tokenDir string, permissions permissionBackend) (Snapshot, error) {
	path := filepath.Join(tokenDir, Filename)
	file, err := openStoreFile(path)
	if errors.Is(err, os.ErrNotExist) {
		empty := emptyStore()
		encoded, marshalErr := json.Marshal(empty)
		if marshalErr != nil {
			return unavailableSnapshot(marshalErr)
		}
		return snapshotFrom(empty, encoded), nil
	}
	if err != nil {
		return unavailableSnapshot(fmt.Errorf("open credential store: %w", err))
	}
	defer file.Close() //nolint:errcheck // read-only file
	if err := permissions.verifyDirectory(tokenDir); err != nil {
		return unavailableSnapshot(fmt.Errorf("verify credential directory: %w", err))
	}
	if err := permissions.verifyFile(file); err != nil {
		return unavailableSnapshot(fmt.Errorf("verify credential store permissions: %w", err))
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximumCredentialStoreBytes+1))
	if err != nil {
		return unavailableSnapshot(fmt.Errorf("read credential store: %w", err))
	}
	if int64(len(raw)) > maximumCredentialStoreBytes {
		return unavailableSnapshot(errors.New("credential store exceeds size limit"))
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var saved storeFile
	if err := decoder.Decode(&saved); err != nil {
		return unavailableSnapshot(fmt.Errorf("decode credential store: %w", err))
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return unavailableSnapshot(errors.New("credential store contains trailing data"))
	}
	if err := validateStore(saved); err != nil {
		return unavailableSnapshot(err)
	}
	return snapshotFrom(saved, raw), nil
}

func unavailableSnapshot(err error) (Snapshot, error) {
	wrapped := fmt.Errorf("%w: %w", ErrUnavailable, err)
	return Snapshot{loadErr: wrapped}, wrapped
}

func validateStore(saved storeFile) error {
	if saved.Version != credentialStoreVersion || saved.Credentials == nil {
		return errors.New("credential store has an unsupported format")
	}
	for id, credential := range saved.Credentials {
		if id != PersonEnrichmentSuppressionID {
			if err := ValidateID(id); err != nil {
				return errors.New("credential store contains an invalid credential ID")
			}
			origin, err := EndpointOrigin(credential.Origin)
			if err != nil || origin != credential.Origin {
				return errors.New("credential store contains an invalid endpoint binding")
			}
		} else if credential.Origin != "" {
			return errors.New("credential store suppression key must not have an endpoint binding")
		}
		if credential.ID != id || credential.Kind != recordKind(id) || credential.Revision <= 0 {
			return errors.New("credential store contains invalid identity metadata")
		}
		if credential.Value == "" {
			return errors.New("credential store contains an empty credential")
		}
	}
	return nil
}

func snapshotFrom(saved storeFile, raw []byte) Snapshot {
	credentials := make(map[string]record, len(saved.Credentials))
	maps.Copy(credentials, saved.Credentials)
	digest := sha256.Sum256(raw)
	return Snapshot{ETag: `"sha256-` + hex.EncodeToString(digest[:]) + `"`, credentials: credentials}
}

func (s Snapshot) Resolve(
	id, endpoint, environmentName string,
	lookup func(string) (string, bool),
) (string, State, error) {
	if s.loadErr != nil {
		return "", State{}, s.loadErr
	}
	if err := ValidateID(id); err != nil {
		return "", State{}, err
	}
	if stored, ok := s.credentials[id]; ok {
		origin, err := EndpointOrigin(endpoint)
		if err != nil || stored.Origin != origin {
			return "", State{Configured: false, Source: SourceNone}, ErrOriginMismatch
		}
		return stored.Value, State{Configured: true, Source: SourceStored}, nil
	}
	if lookup != nil && environmentName != "" {
		if value, ok := lookup(environmentName); ok && value != "" {
			return value, State{Configured: true, Source: SourceEnvironment}, nil
		}
	}
	return "", State{Configured: false, Source: SourceNone}, nil
}

// Stored reports whether the snapshot holds a credential for id, regardless
// of which endpoint origin it is bound to.
func (s Snapshot) Stored(id string) bool {
	_, ok := s.credentials[id]
	return ok
}

// StoredPersonEnrichmentIDs returns the named enrichment credential IDs in a
// stable order without exposing their values.
func (s Snapshot) StoredPersonEnrichmentIDs() []string {
	ids := make([]string, 0)
	for id := range s.credentials {
		if id != PersonEnrichmentSuppressionID && strings.HasPrefix(id, personEnrichmentCredentialIDPrefix) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func (s Snapshot) ResolveSuppression() (string, bool, error) {
	if s.loadErr != nil {
		return "", false, s.loadErr
	}
	credential, ok := s.credentials[PersonEnrichmentSuppressionID]
	if !ok {
		return "", false, nil
	}
	return credential.Value, true, nil
}

func Put(tokenDir, ifMatch, id, endpoint, value string) (Snapshot, error) {
	if err := ValidateID(id); err != nil {
		return Snapshot{}, err
	}
	if value == "" {
		return Snapshot{}, errors.New("provider credential cannot be empty")
	}
	origin, err := EndpointOrigin(endpoint)
	if err != nil {
		return Snapshot{}, err
	}
	return mutate(tokenDir, ifMatch, func(credentials map[string]record) {
		revision := int64(1)
		if current, ok := credentials[id]; ok {
			revision = current.Revision + 1
		}
		credentials[id] = record{ID: id, Kind: recordKind(id), Value: value, Origin: origin, Revision: revision}
	})
}

func PutSuppression(tokenDir, ifMatch, value string) (Snapshot, error) {
	if value == "" {
		return Snapshot{}, errors.New("suppression key cannot be empty")
	}
	return mutate(tokenDir, ifMatch, func(credentials map[string]record) {
		revision := int64(1)
		if current, ok := credentials[PersonEnrichmentSuppressionID]; ok {
			revision = current.Revision + 1
		}
		credentials[PersonEnrichmentSuppressionID] = record{
			ID: PersonEnrichmentSuppressionID, Kind: recordKind(PersonEnrichmentSuppressionID),
			Value: value, Revision: revision,
		}
	})
}

// DeleteSuppressionIfValue removes a just-generated suppression key during a
// failed cross-store settings transaction. It preserves unrelated concurrent
// credential changes and never removes a key another writer replaced.
func DeleteSuppressionIfValue(tokenDir, value string) (Snapshot, error) {
	if value == "" {
		return Snapshot{}, errors.New("suppression key cannot be empty")
	}
	permissions := nativePermissions{}
	if err := permissions.secureDirectory(tokenDir); err != nil {
		return Snapshot{}, fmt.Errorf("secure credential directory: %w", err)
	}
	var result Snapshot
	err := withStoreLock(tokenDir, func() error {
		current, err := readWithPermissions(tokenDir, permissions)
		if err != nil {
			return err
		}
		stored, ok := current.credentials[PersonEnrichmentSuppressionID]
		if !ok {
			result = current
			return nil
		}
		if subtle.ConstantTimeCompare([]byte(stored.Value), []byte(value)) != 1 {
			return ErrConflict
		}
		credentials := make(map[string]record, len(current.credentials))
		maps.Copy(credentials, current.credentials)
		delete(credentials, PersonEnrichmentSuppressionID)
		result, err = persist(tokenDir, permissions, credentials)
		return err
	})
	return result, err
}

func Delete(tokenDir, ifMatch, id string) (Snapshot, error) {
	if err := ValidateID(id); err != nil {
		return Snapshot{}, err
	}
	return mutate(tokenDir, ifMatch, func(credentials map[string]record) {
		delete(credentials, id)
	})
}

func mutate(tokenDir, ifMatch string, mutation func(map[string]record)) (Snapshot, error) {
	permissions := nativePermissions{}
	if err := permissions.secureDirectory(tokenDir); err != nil {
		return Snapshot{}, fmt.Errorf("secure credential directory: %w", err)
	}
	var result Snapshot
	err := withStoreLock(tokenDir, func() error {
		current, err := readWithPermissions(tokenDir, permissions)
		if err != nil {
			return err
		}
		if ifMatch == "" || ifMatch != current.ETag {
			return ErrConflict
		}
		credentials := make(map[string]record, len(current.credentials)+1)
		maps.Copy(credentials, current.credentials)
		mutation(credentials)
		result, err = persist(tokenDir, permissions, credentials)
		if err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func persist(tokenDir string, permissions permissionBackend, credentials map[string]record) (Snapshot, error) {
	saved := storeFile{Version: credentialStoreVersion, Credentials: credentials}
	encoded, err := json.Marshal(saved)
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode credential store: %w", err)
	}
	encoded = append(encoded, '\n')
	published, err := publish(tokenDir, permissions, encoded)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshotFrom(saved, published), nil
}

func publish(tokenDir string, permissions permissionBackend, encoded []byte) ([]byte, error) {
	temporary, err := os.CreateTemp(tokenDir, ".provider-credentials-*.json")
	if err != nil {
		return nil, fmt.Errorf("create credential candidate: %w", err)
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		_ = temporary.Close()
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := permissions.secureFile(temporary); err != nil {
		return nil, fmt.Errorf("secure credential candidate: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return nil, fmt.Errorf("write credential candidate: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return nil, fmt.Errorf("sync credential candidate: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close credential candidate: %w", err)
	}
	if err := replaceStoreFile(temporaryPath, filepath.Join(tokenDir, Filename)); err != nil {
		return nil, fmt.Errorf("publish credential store: %w", err)
	}
	published = true
	if err := syncStoreDirectory(tokenDir); err != nil {
		return nil, fmt.Errorf("sync credential store directory: %w", err)
	}
	return encoded, nil
}

func recordKind(id string) string {
	switch id {
	case VectorEmbeddingsID:
		return "vector_embeddings"
	case VectorMultimodalID:
		return "vector_multimodal"
	case PeopleSweepID:
		return "people_sweep"
	case PersonEnrichmentSuppressionID:
		return "person_enrichment_suppression"
	default:
		return "person_enrichment"
	}
}
