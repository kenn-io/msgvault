package mcpevents

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/store"
)

func loadKey(path string, rows []store.MCPSubscription) ([]byte, error) {
	fail := func() ([]byte, error) { return nil, &Error{Code: -32015, Reason: "events_key_unavailable"} }
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		for _, row := range rows {
			if len(row.SecretEnc) > 0 || len(row.PreviousSecretEnc) > 0 {
				return fail()
			}
		}
		if path == "" {
			return fail()
		}
		if err := fileutil.SecureMkdirAll(filepath.Dir(path), 0700); err != nil {
			return fail()
		}
		key, err = createKey(path)
		if errors.Is(err, fs.ErrExist) {
			key, err = os.ReadFile(path)
		}
	}
	if err != nil || len(key) != 32 {
		return fail()
	}
	info, err := os.Lstat(path)
	if err != nil || !keyFileModeAllowed(info.Mode(), runtime.GOOS) {
		return fail()
	}
	for _, row := range rows {
		if len(row.SecretEnc) > 0 {
			if _, err := decryptSecret(key, row.ID, "current", row.SecretEnc); err != nil {
				return fail()
			}
		}
		if len(row.PreviousSecretEnc) > 0 {
			if _, err := decryptSecret(key, row.ID, "previous", row.PreviousSecretEnc); err != nil {
				return fail()
			}
		}
	}
	return key, nil
}

// createKey publishes a new key only when path does not exist. The key is
// staged in a private temporary file, synced, and then linked into place, so
// a crash cannot leave a partial key and a concurrent creator's key is never
// replaced. An existing path fails with fs.ErrExist.
func createKey(path string) ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate MCP Events key: %w", err)
	}
	if err := atomicfile.WriteNew(path, key, atomicfile.WithPrivate()); err != nil {
		return nil, err
	}
	// WithPrivate also admits SYSTEM and Administrators on Windows; keep the
	// repository's current-user-only policy for owner-only files.
	if err := fileutil.SecureChmod(path, 0600); err != nil {
		return nil, err
	}
	return key, nil
}

func keyFileModeAllowed(mode os.FileMode, platform string) bool {
	// Windows file modes do not describe DACL access. SecureOpenFile applies
	// the repository's current-user DACL policy when creating the key.
	return mode.IsRegular() && (platform == "windows" || mode.Perm()&0077 == 0)
}
