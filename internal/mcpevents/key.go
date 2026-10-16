package mcpevents

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/store"
)

func loadKey(path string, rows []store.MCPSubscription) ([]byte, error) {
	fail := func() ([]byte, error) { return nil, &Error{Code: -32015, Reason: "events_key_unavailable"} }
	key, err := readKeyFile(path)
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
			key, err = readKeyFile(path)
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

// readKeyFile permits Windows delete sharing so concurrent no-replace
// publishers can inspect the destination while another creator reads it.
func readKeyFile(path string) ([]byte, error) {
	if runtime.GOOS != "windows" {
		return os.ReadFile(path)
	}
	file, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		return nil, fmt.Errorf("open MCP Events key: %w", err)
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, 33))
}

// createKey publishes a complete owner-only key without replacing an
// existing entry. Permissions are finalized on the staging file: changing
// them after publication races concurrent readers on Windows.
func createKey(path string) ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate MCP Events key: %w", err)
	}
	var staged *os.File
	var err error
	if runtime.GOOS == "windows" {
		// Protect the staging file from creation, before narrowing its DACL
		// to the repository's current-user-only policy below.
		staged, err = safefileio.CreatePrivateTemp(filepath.Dir(path), ".mcp-events-key-*")
	} else {
		staged, err = os.CreateTemp(filepath.Dir(path), ".mcp-events-key-*")
	}
	if err != nil {
		return nil, fmt.Errorf("create MCP Events staging key: %w", err)
	}
	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()
	// Apply the repository's current-user-only Windows DACL before writing
	// the secret, while the temporary file is still private and empty.
	if err := fileutil.SecureChmod(staged.Name(), 0600); err != nil {
		return nil, err
	}
	if n, err := staged.Write(key); err != nil {
		return nil, err
	} else if n != len(key) {
		return nil, io.ErrShortWrite
	}
	if err := staged.Sync(); err != nil {
		return nil, err
	}
	if err := staged.Close(); err != nil {
		return nil, err
	}
	if runtime.GOOS == "windows" {
		// Windows needs a write-through rename for durable publication.
		err = atomicfile.RenameNoReplace(staged.Name(), path)
	} else {
		err = atomicfile.PublishNoReplace(staged.Name(), path)
	}
	if err != nil {
		return nil, err
	}
	if err := os.Remove(staged.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := atomicfile.SyncDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return key, nil
}

func keyFileModeAllowed(mode os.FileMode, platform string) bool {
	// Windows file modes do not describe DACL access. SecureOpenFile applies
	// the repository's current-user DACL policy when creating the key.
	return mode.IsRegular() && (platform == "windows" || mode.Perm()&0077 == 0)
}
