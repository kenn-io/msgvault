package peoplesweep

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"os"
	"sync"
)

var ErrCredentialRevisionConflict = errors.New("people provider credential revision changed")

var credentialRevisionKey struct {
	once sync.Once
	key  [32]byte
	err  error
}

// Revision returns an opaque token for one exact stored credential state.
// It is a keyed digest, so it cannot serve as an offline verifier for the
// credential even if the underlying value has unexpectedly low entropy.
func (s *FileCredentialStore) Revision(profileName string) (string, bool, error) {
	if err := validateCredentialProfileName(profileName); err != nil {
		return "", false, err
	}
	var data []byte
	present := false
	err := s.withCredentialRoot("load", func(root credentialStoreRoot) error {
		loaded, loadErr := root.load(profileName)
		if errors.Is(loadErr, ErrCredentialNotFound) || errors.Is(loadErr, os.ErrNotExist) {
			return nil
		}
		if loadErr != nil {
			return loadErr
		}
		data, present = loaded, true
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		err = nil // The private namespace has not been created yet.
	}
	if err != nil {
		return "", false, err
	}
	revision, err := credentialStateRevision(profileName, data, present)
	return revision, present, err
}

// SaveIfRevision atomically replaces one credential only while its exact
// previously observed state is current. The private namespace lock covers
// both the comparison and the existing secure publication path.
func (s *FileCredentialStore) SaveIfRevision(
	profileName string, credential Credential, expected string,
) (string, error) {
	if err := validateCredentialProfileName(profileName); err != nil {
		return "", err
	}
	if err := validateStoredCredential(credential); err != nil {
		return "", err
	}
	newData, err := json.Marshal(credentialFile{Scheme: credential.Scheme, Value: credential.Value()}, json.Deterministic(true))
	if err != nil {
		return "", errors.New("serialize people provider credential")
	}
	var newRevision string
	err = s.withCredentialRoot("save-if-revision", func(root credentialStoreRoot) error {
		oldData, loadErr := root.load(profileName)
		present := loadErr == nil
		if loadErr != nil && !errors.Is(loadErr, ErrCredentialNotFound) && !errors.Is(loadErr, os.ErrNotExist) {
			return loadErr
		}
		actual, revisionErr := credentialStateRevision(profileName, oldData, present)
		if revisionErr != nil {
			return revisionErr
		}
		if !hmac.Equal([]byte(actual), []byte(expected)) {
			return ErrCredentialRevisionConflict
		}
		if err := root.save(profileName, newData); err != nil {
			return err
		}
		newRevision, revisionErr = credentialStateRevision(profileName, newData, true)
		return revisionErr
	})
	return newRevision, err
}

// DeleteIfRevision retires only the exact credential pinned by the existing
// guarded deletion path. The guard prevents a concurrent replacement from
// being deleted after the revision check.
func (s *FileCredentialStore) DeleteIfRevision(profileName, expected string) (string, error) {
	guard, err := s.PreflightDelete(profileName)
	if err != nil {
		return "", err
	}
	reader, ok := guard.(interface{ credentialRevisionData() ([]byte, error) })
	if !ok {
		return "", errors.Join(errors.New("people provider credential revision guard is unsupported"), guard.Close())
	}
	data, err := reader.credentialRevisionData()
	if err != nil {
		return "", errors.Join(err, guard.Close())
	}
	actual, err := credentialStateRevision(profileName, data, true)
	if err != nil {
		return "", errors.Join(err, guard.Close())
	}
	if !hmac.Equal([]byte(actual), []byte(expected)) {
		return "", errors.Join(ErrCredentialRevisionConflict, guard.Close())
	}
	if err := errors.Join(s.Delete(profileName, guard), guard.Close()); err != nil {
		return "", err
	}
	return credentialStateRevision(profileName, nil, false)
}

func credentialStateRevision(profileName string, data []byte, present bool) (string, error) {
	credentialRevisionKey.once.Do(func() {
		_, credentialRevisionKey.err = rand.Read(credentialRevisionKey.key[:])
	})
	if credentialRevisionKey.err != nil {
		return "", credentialRevisionKey.err
	}
	mac := hmac.New(sha256.New, credentialRevisionKey.key[:])
	_, _ = mac.Write([]byte("msgvault people credential revision v1\x00"))
	_, _ = mac.Write([]byte(profileName))
	if present {
		_, _ = mac.Write([]byte{0, 1})
		_, _ = mac.Write(data)
	} else {
		_, _ = mac.Write([]byte{0, 0})
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
