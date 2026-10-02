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
		return "", errors.New("resolve Fastmail tokens directory")
	}
	path := strings.TrimSpace(s.APITokenFile)
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("fastmail token file must be under the tokens directory")
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return "", errors.New("read Fastmail token file")
	}
	defer func() { _ = directory.Close() }()
	file, err := directory.Open(rel)
	if err != nil {
		return "", errors.New("read Fastmail token file")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("fastmail token file must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		return "", errors.New("fastmail token file must have permissions 0600")
	}
	if info.Size() > 64<<10 {
		return "", errors.New("fastmail token file is too large")
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return "", errors.New("read Fastmail token file: unavailable")
	}
	if len(data) > 64<<10 {
		return "", errors.New("fastmail token file is too large")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("fastmail token file is empty")
	}
	return token, nil
}
