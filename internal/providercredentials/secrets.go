package providercredentials

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/atomicfile"
)

const ServerKeyFilename = "server-api-key" // #nosec G101 -- filename, not a credential.
const maximumSecretBytes = 64 << 10

// ResolveSecret selects an inline value, a mounted file, or a named environment.
func ResolveSecret(inline, file, environment string) (string, error) {
	if inline != "" {
		value := strings.TrimSpace(inline)
		if value == "" {
			return "", errors.New("inline credential is empty")
		}
		return value, nil
	}
	if file != "" {
		return ReadSecretFile(file)
	}
	if environment != "" {
		value, ok := os.LookupEnv(environment)
		value = strings.TrimSpace(value)
		if !ok || value == "" {
			return "", fmt.Errorf("credential environment %q is missing or empty", environment)
		}
		return value, nil
	}
	return "", nil
}

// ReadSecretFile reads an owner-only mounted credential without changing it.
func ReadSecretFile(path string) (string, error) {
	file, err := openSecretFile(path)
	if err != nil {
		return "", fmt.Errorf("open credential file: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only input
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat credential file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("credential file must be a regular file")
	}
	if err := verifySecretFile(file); err != nil {
		return "", fmt.Errorf("verify credential file: %w", err)
	}
	value, err := ReadSecret(file)
	if err != nil {
		return "", fmt.Errorf("read credential file: %w", err)
	}
	return value, nil
}

// ReadSecret reads a bounded plain-text credential, including standard input.
// Errors never include the input value.
func ReadSecret(reader io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximumSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("read credential input: %w", err)
	}
	if len(data) > maximumSecretBytes {
		return "", errors.New("credential input exceeds 64 KiB limit")
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("credential input is empty")
	}
	return value, nil
}

// EnsureServerKey creates a persisted daemon credential once.
func EnsureServerKey(tokenDir string) (string, error) {
	permissions := nativePermissions{}
	if err := permissions.secureDirectory(tokenDir); err != nil {
		return "", fmt.Errorf("secure server credential directory: %w", err)
	}
	var key string
	err := withStoreLock(tokenDir, func() error {
		path := filepath.Join(tokenDir, ServerKeyFilename)
		var err error
		key, err = ReadSecretFile(path)
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return fmt.Errorf("generate server credential: %w", err)
		}
		key = base64.RawURLEncoding.EncodeToString(random[:])
		candidate, err := os.CreateTemp(tokenDir, ".server-api-key-*")
		if err != nil {
			return fmt.Errorf("create server credential candidate: %w", err)
		}
		defer func() {
			_ = candidate.Close()
			_ = os.Remove(candidate.Name())
		}()
		if err := permissions.secureFile(candidate); err != nil {
			return fmt.Errorf("secure server credential candidate: %w", err)
		}
		if _, err := candidate.WriteString(key + "\n"); err != nil {
			return fmt.Errorf("write server credential: %w", err)
		}
		if err := candidate.Sync(); err != nil {
			return fmt.Errorf("sync server credential: %w", err)
		}
		if err := candidate.Close(); err != nil {
			return fmt.Errorf("close server credential: %w", err)
		}
		if err := atomicfile.Replace(candidate.Name(), path); err != nil {
			return fmt.Errorf("publish server credential: %w", err)
		}
		return atomicfile.SyncDir(tokenDir)
	})
	if err != nil {
		return "", err
	}
	return key, nil
}
