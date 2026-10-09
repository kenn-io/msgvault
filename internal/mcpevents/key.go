package mcpevents

import (
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

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
		var f *os.File
		f, err = fileutil.SecureOpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			key, err = os.ReadFile(path)
		} else if err == nil {
			key = make([]byte, 32)
			_, err = rand.Read(key)
			if err == nil {
				var n int
				n, err = f.Write(key)
				if err == nil && n != len(key) {
					err = io.ErrShortWrite
				}
			}
			if err == nil {
				err = f.Sync()
			}
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
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

func keyFileModeAllowed(mode os.FileMode, platform string) bool {
	// Windows file modes do not describe DACL access. SecureOpenFile applies
	// the repository's current-user DACL policy when creating the key.
	return mode.IsRegular() && (platform == "windows" || mode.Perm()&0077 == 0)
}
