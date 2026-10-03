package requestsign

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ReadPrivateFile refuses symlinks, nonregular files, unsafe permissions and
// identity changes during open. Its limit bounds allocations before decoding.
func ReadPrivateFile(path string, maxBytes int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("open private file: %w", err)
	}
	if !before.Mode().IsRegular() || before.Size() > maxBytes {
		return nil, errors.New("private file must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read private file: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !os.SameFile(before, info) {
		return nil, errors.New("private file changed while opening")
	}
	if err := privateFilePermissions(file, info); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read private file: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("private file exceeds byte limit")
	}
	return data, nil
}

func ReadSigningSecret(path string) ([]byte, error) {
	data, err := ReadPrivateFile(path, 4096)
	if err != nil {
		return nil, err
	}
	secret, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(secret) != 64 {
		return nil, errors.New("signing secret must encode exactly 64 random bytes as base64")
	}
	return secret, nil
}

func ReadAPIKey(path string) (string, error) {
	data, err := ReadPrivateFile(path, 4096)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(data))
	if len(key) < 32 || len(key) > 512 {
		return "", errors.New("dedicated API credential must contain 32 to 512 characters")
	}
	for _, b := range []byte(key) {
		if b < 0x21 || b > 0x7e {
			return "", errors.New("API credential must use visible ASCII characters")
		}
	}
	return key, nil
}
