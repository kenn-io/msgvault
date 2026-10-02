package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func (s FastmailSource) validateCredentialSource() error {
	count := 0
	for _, value := range []string{s.APIToken, s.APITokenEnv, s.APITokenFile} {
		if strings.TrimSpace(value) != "" {
			count++
		}
	}
	if count == 0 {
		return errors.New("api_token is required, or configure api_token_env or api_token_file")
	}
	if count != 1 {
		return errors.New("configure exactly one of api_token, api_token_env, api_token_file")
	}
	return nil
}

// FastmailAPIToken resolves the selected credential at request time. Relative
// files are resolved under tokens/; resolved secrets are never saved to config.
func (c *Config) FastmailAPIToken(s FastmailSource) (string, error) {
	if err := s.validateCredentialSource(); err != nil {
		return "", err
	}
	if token := strings.TrimSpace(s.APIToken); token != "" {
		return token, nil
	}
	if variable := strings.TrimSpace(s.APITokenEnv); variable != "" {
		token := strings.TrimSpace(os.Getenv(variable))
		if token == "" {
			return "", fmt.Errorf("fastmail token environment variable %q is empty or unset", variable)
		}
		return token, nil
	}
	root, err := filepath.Abs(c.TokensDir())
	if err != nil {
		return "", fmt.Errorf("resolve Fastmail tokens directory: %w", err)
	}
	path := strings.TrimSpace(s.APITokenFile)
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("fastmail token file must be under the tokens directory")
	}
	tokens, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("read Fastmail token file: %w", err)
	}
	defer func() { _ = tokens.Close() }()
	file, err := tokens.Open(rel)
	if err != nil {
		return "", fmt.Errorf("read Fastmail token file: %w", err)
	}
	defer func() { _ = file.Close() }()
	// Windows has no mode bits to check, as with the service account key.
	if runtime.GOOS != "windows" {
		info, err := file.Stat()
		if err != nil {
			return "", fmt.Errorf("read Fastmail token file: %w", err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("fastmail token file %s permissions are too open (%04o); use chmod 600", path, info.Mode().Perm())
		}
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("read Fastmail token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("fastmail token file %s is empty", path)
	}
	return token, nil
}
