package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/providercredentials"
)

// RuntimeOverrides contains explicitly supplied CLI values. Nil means absent.
type RuntimeOverrides struct {
	BindAddr *string
	APIPort  *int
}

// Load reads optional TOML and applies process environment controls.
func Load(path, home string) (*Config, error) {
	return LoadWithOverrides(path, home, RuntimeOverrides{})
}

// LoadWithOverrides applies flags before validating lower-priority env values.
func LoadWithOverrides(path, home string, overrides RuntimeOverrides) (*Config, error) {
	return loadWithOverrides(path, home, overrides)
}

type secretSources struct {
	inline, file, environment  string
	inlineSet, fileSet, envSet bool
}

type runtimeCredential struct {
	sources  *secretSources
	value    string
	resolved bool
}

type runtimeSaveValue[T comparable] struct {
	set      bool
	original T
	runtime  T
}

func (v *runtimeSaveValue[T]) capture(original, runtime T) {
	if !v.set {
		v.original = original
	}
	v.runtime = runtime
	v.set = true
}

func (v runtimeSaveValue[T]) restore(target *T) {
	if v.set && *target == v.runtime {
		*target = v.original
	}
}

type runtimeSaveStrings struct {
	set      bool
	original []string
	runtime  []string
}

func (v *runtimeSaveStrings) capture(original, runtime []string) {
	if !v.set {
		v.original = slices.Clone(original)
	}
	v.runtime = slices.Clone(runtime)
	v.set = true
}

func (v *runtimeSaveStrings) restore(target *[]string) {
	if v.set && slices.Equal(*target, v.runtime) {
		*target = slices.Clone(v.original)
	}
}

// runtimeConfigState records runtime-only values so unrelated saves do not
// make them permanent. A field changed after loading remains saveable.
type runtimeConfigState struct {
	flags               RuntimeOverrides
	bindAddr            runtimeSaveValue[string]
	apiPort             runtimeSaveValue[int]
	backupRepo          runtimeSaveValue[string]
	remoteURL           runtimeSaveValue[string]
	serverKeyFile       runtimeSaveValue[string]
	remoteKeyFile       runtimeSaveValue[string]
	docbankKeyFile      runtimeSaveValue[string]
	allowInsecure       runtimeSaveValue[bool]
	corsCredentials     runtimeSaveValue[bool]
	remoteAllowInsecure runtimeSaveValue[bool]
	corsOrigins         runtimeSaveStrings
	trustedProxies      runtimeSaveStrings
}

func (s runtimeConfigState) restore(c *Config) {
	s.bindAddr.restore(&c.Server.BindAddr)
	s.apiPort.restore(&c.Server.APIPort)
	s.backupRepo.restore(&c.Backup.Repo)
	s.remoteURL.restore(&c.Remote.URL)
	s.serverKeyFile.restore(&c.Server.APIKeyFile)
	s.remoteKeyFile.restore(&c.Remote.APIKeyFile)
	s.docbankKeyFile.restore(&c.Integrations.Docbank.APIKeyFile)
	s.allowInsecure.restore(&c.Server.AllowInsecure)
	s.corsCredentials.restore(&c.Server.CORSCredentials)
	s.remoteAllowInsecure.restore(&c.Remote.AllowInsecure)
	s.corsOrigins.restore(&c.Server.CORSOrigins)
	s.trustedProxies.restore(&c.Server.TrustedProxies)
}

func environmentSecret(prefix string) *secretSources {
	var s secretSources
	s.inline, s.inlineSet = os.LookupEnv(prefix)
	s.file, s.fileSet = os.LookupEnv(prefix + "_FILE")
	s.environment, s.envSet = os.LookupEnv(prefix + "_ENV")
	if !s.inlineSet && !s.fileSet && !s.envSet {
		return nil
	}
	return &s
}

func (s *secretSources) resolve() (string, error) {
	if s.inlineSet {
		if strings.TrimSpace(s.inline) == "" {
			return "", errors.New("inline credential environment is empty")
		}
		return providercredentials.ResolveSecret(s.inline, "", "")
	}
	if s.fileSet {
		if s.file == "" {
			return "", errors.New("credential file environment is empty")
		}
		return providercredentials.ReadSecretFile(s.file)
	}
	if s.environment == "" {
		return "", errors.New("credential environment name is empty")
	}
	return providercredentials.ResolveSecret("", "", s.environment)
}

func (c *Config) applyRuntimeOverrides(o RuntimeOverrides) error {
	bindSupplied := o.BindAddr != nil
	if o.BindAddr != nil {
		c.runtimeConfigState.flags.BindAddr = new(*o.BindAddr)
		c.runtimeConfigState.bindAddr.capture(c.Server.BindAddr, *o.BindAddr)
		c.Server.BindAddr = *o.BindAddr
		c.bindSource = "--bind"
	} else if v, ok := os.LookupEnv("MSGVAULT_BIND_ADDR"); ok {
		bindSupplied = true
		c.runtimeConfigState.bindAddr.capture(c.Server.BindAddr, v)
		c.Server.BindAddr = v
		c.bindSource = "MSGVAULT_BIND_ADDR"
	}
	if bindSupplied && strings.TrimSpace(c.Server.BindAddr) == "" {
		return errors.New("server bind address is empty")
	}
	if o.APIPort != nil {
		c.runtimeConfigState.flags.APIPort = new(*o.APIPort)
		c.runtimeConfigState.apiPort.capture(c.Server.APIPort, *o.APIPort)
		c.Server.APIPort = *o.APIPort
	} else if v, ok := os.LookupEnv("MSGVAULT_API_PORT"); ok {
		port, err := strconv.Atoi(v)
		if err != nil {
			return errors.New("MSGVAULT_API_PORT must be an integer between 0 and 65535")
		}
		c.runtimeConfigState.apiPort.capture(c.Server.APIPort, port)
		c.Server.APIPort = port
	}
	if v, ok := os.LookupEnv("MSGVAULT_BACKUP_REPO"); ok {
		value := expandPath(v)
		c.runtimeConfigState.backupRepo.capture(c.Backup.Repo, value)
		c.Backup.Repo = value
	} else {
		c.Backup.Repo = expandPath(c.Backup.Repo)
	}
	if v, ok := os.LookupEnv("MSGVAULT_REMOTE_URL"); ok {
		c.runtimeConfigState.remoteURL.capture(c.Remote.URL, v)
		c.Remote.URL = v
	}
	for _, entry := range []struct {
		name        string
		destination *bool
		record      *runtimeSaveValue[bool]
	}{
		{"MSGVAULT_ALLOW_INSECURE", &c.Server.AllowInsecure, &c.runtimeConfigState.allowInsecure},
		{"MSGVAULT_CORS_CREDENTIALS", &c.Server.CORSCredentials, &c.runtimeConfigState.corsCredentials},
		{"MSGVAULT_REMOTE_ALLOW_INSECURE", &c.Remote.AllowInsecure, &c.runtimeConfigState.remoteAllowInsecure},
	} {
		if v, ok := os.LookupEnv(entry.name); ok {
			value, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("%s must be a boolean", entry.name)
			}
			entry.record.capture(*entry.destination, value)
			*entry.destination = value
		}
	}
	for _, entry := range []struct {
		name        string
		destination *[]string
		record      *runtimeSaveStrings
	}{
		{"MSGVAULT_CORS_ORIGINS", &c.Server.CORSOrigins, &c.runtimeConfigState.corsOrigins},
		{"MSGVAULT_TRUSTED_PROXIES", &c.Server.TrustedProxies, &c.runtimeConfigState.trustedProxies},
	} {
		if v, ok := os.LookupEnv(entry.name); ok {
			original := slices.Clone(*entry.destination)
			*entry.destination = []string{}
			for part := range strings.SplitSeq(v, ",") {
				if part = strings.TrimSpace(part); part != "" {
					*entry.destination = append(*entry.destination, part)
				}
			}
			entry.record.capture(original, *entry.destination)
		}
	}
	c.Server.credential.sources = environmentSecret("MSGVAULT_API_KEY")
	c.Remote.credential.sources = environmentSecret("MSGVAULT_REMOTE_API_KEY")
	for _, credential := range []*runtimeCredential{&c.Server.credential, &c.Remote.credential} {
		if credential.sources != nil && credential.sources.file != "" {
			credential.sources.file = c.credentialPath(credential.sources.file)
		}
	}
	return nil
}

func (c *Config) credentialPath(path string) string {
	if path == "" {
		return ""
	}
	path = expandPath(path)
	if c.configPath != "" {
		return resolveRelative(path, filepath.Dir(c.configPath))
	}
	return path
}

func (c *Config) resolveCredentialPaths() {
	for _, entry := range []struct {
		path   *string
		record *runtimeSaveValue[string]
	}{
		{&c.Server.APIKeyFile, &c.runtimeConfigState.serverKeyFile},
		{&c.Remote.APIKeyFile, &c.runtimeConfigState.remoteKeyFile},
		{&c.Integrations.Docbank.APIKeyFile, &c.runtimeConfigState.docbankKeyFile},
	} {
		if resolved := c.credentialPath(*entry.path); resolved != *entry.path {
			entry.record.capture(*entry.path, resolved)
			*entry.path = resolved
		}
	}
}

// ServerKeyFilePath is the default persisted daemon credential, under data_dir.
func (c *Config) ServerKeyFilePath() string {
	return filepath.Join(c.TokensDir(), providercredentials.ServerKeyFilename)
}

// HasCredentialSource distinguishes explicit input from default minting.
func (s *ServerConfig) HasCredentialSource() bool {
	return s.APIKey != "" || s.APIKeyFile != "" || s.APIKeyEnv != "" || s.credential.sources != nil
}

// ResolveServerKey reads only the selected server credential. Repeated calls
// refresh a key minted by a child daemon after this configuration was loaded.
func (c *Config) ResolveServerKey() error {
	s := &c.Server
	if s.credential.sources == nil {
		s.credential.sources = environmentSecret("MSGVAULT_API_KEY")
		if s.credential.sources != nil && s.credential.sources.file != "" {
			s.credential.sources.file = c.credentialPath(s.credential.sources.file)
		}
	}
	s.credential.resolved, s.credential.value = true, ""
	var key string
	var err error
	if s.credential.sources != nil {
		key, err = s.credential.sources.resolve()
	} else {
		key, err = providercredentials.ResolveSecret(s.APIKey, s.APIKeyFile, s.APIKeyEnv)
		if err == nil && !s.HasCredentialSource() && !s.AllowInsecure {
			key, err = providercredentials.ReadSecretFile(c.ServerKeyFilePath())
			if errors.Is(err, os.ErrNotExist) {
				key, err = "", nil
			}
		}
	}
	if err != nil {
		return fmt.Errorf("server API key: %w", err)
	}
	s.credential.value = key
	return nil
}

// ValidateServerKey checks the selected credential without creating one. A
// secure non-loopback server may mint its key after acquiring daemon ownership.
func (c *Config) ValidateServerKey() error {
	if err := c.ResolveServerKey(); err != nil {
		return err
	}
	if c.Server.shouldAutoMintKey() {
		return nil
	}
	if c.Server.AgentAccess && c.Server.AuthenticationKey() == "" {
		return errors.New("server agent_access requires an effective API key")
	}
	return c.Server.ValidateSecure()
}

// PrepareServerKey resolves explicit sources and mints a persistent key only
// when secure non-loopback serving otherwise has no credential.
func (c *Config) PrepareServerKey() error {
	if err := c.ValidateServerKey(); err != nil {
		return err
	}
	if c.Server.shouldAutoMintKey() {
		key, err := providercredentials.EnsureServerKey(filepath.Dir(c.ServerKeyFilePath()))
		if err != nil {
			return fmt.Errorf("persist server API key: %w", err)
		}
		c.Server.credential.value = key
	}
	return nil
}

// ResolveRemoteKey reads only the selected remote daemon credential.
func (c *Config) ResolveRemoteKey() error {
	r := &c.Remote
	r.credential.resolved, r.credential.value = true, ""
	var key string
	var err error
	if r.credential.sources != nil {
		key, err = r.credential.sources.resolve()
	} else {
		key, err = providercredentials.ResolveSecret(r.APIKey, r.APIKeyFile, r.APIKeyEnv)
	}
	if err != nil {
		return fmt.Errorf("remote API key: %w", err)
	}
	r.credential.value = key
	return nil
}

// AuthenticationKey returns the effective key without changing saved sources.
func (s *ServerConfig) AuthenticationKey() string {
	if s.credential.resolved {
		return s.credential.value
	}
	return s.APIKey
}

func (s *ServerConfig) shouldAutoMintKey() bool {
	return !s.IsLoopback() && !s.AllowInsecure && !s.HasCredentialSource() && s.AuthenticationKey() == ""
}

// AuthenticationKey returns the effective remote key.
func (r RemoteConfig) AuthenticationKey() string {
	if r.credential.resolved {
		return r.credential.value
	}
	return r.APIKey
}

// ResolveAPIKey rereads the selected Docbank credential for each request.
func (d DocbankIntegrationConfig) ResolveAPIKey() (string, error) {
	key, err := providercredentials.ResolveSecret(d.APIKey, d.APIKeyFile, d.APIKeyEnv)
	if err != nil {
		return "", fmt.Errorf("docbank API key: %w", err)
	}
	if key == "" {
		return "", errors.New("docbank API key is unavailable")
	}
	return key, nil
}
